package build

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"time"

	"github.com/discobox-ai/vm/internal/fsutil"
	"github.com/discobox-ai/vm/internal/units"
	"github.com/discobox-ai/vm/pkg/engine"
	"github.com/discobox-ai/vm/pkg/guest"
	"github.com/discobox-ai/vm/pkg/image"
	"github.com/discobox-ai/vm/pkg/machine"
)

// Options is one build.
type Options struct {
	// File is the spec.
	File string
	// Context is the directory copy sources and relative media paths are
	// relative to; it defaults to the spec's directory.
	Context string
	// Args override the spec's args. Every one must be declared in the spec.
	Args map[string]string
	// Tags are extra references for the result, beside the spec's own.
	Tags []string
	// NoCache rebuilds every layer.
	NoCache bool
	// Agent is the disco-vm binary for the guest, baked in at install. It
	// defaults to this binary, which is right when guest and host OS match.
	Agent string
}

// Result is a finished build.
type Result struct {
	Ref   string
	Layer string
}

// Builder runs builds against an engine.
type Builder struct {
	Engine *engine.Engine
	// Out receives the build log and every step's output.
	Out io.Writer
}

func (b *Builder) logf(format string, args ...any) {
	fmt.Fprintf(b.Out, format+"\n", args...)
}

// Build runs a spec to completion and tags the result.
func (b *Builder) Build(ctx context.Context, opts Options) (*Result, error) {
	spec, err := Load(opts.File)
	if err != nil {
		return nil, err
	}
	contextDir := opts.Context
	if contextDir == "" {
		contextDir = filepath.Dir(opts.File)
	}
	if contextDir, err = filepath.Abs(contextDir); err != nil {
		return nil, err
	}
	args := map[string]string{}
	for k, v := range spec.Args {
		args[k] = v
	}
	for k, v := range opts.Args {
		if _, declared := spec.Args[k]; !declared {
			return nil, fmt.Errorf("build arg %s is not declared in the spec's args", k)
		}
		args[k] = v
	}
	spec.Name, spec.Tag = expand(spec.Name, args), expand(spec.Tag, args)
	if err := spec.validateRef(); err != nil {
		return nil, err
	}
	for _, tag := range opts.Tags {
		if err := image.ValidateRef(tag); err != nil {
			return nil, err
		}
	}

	driver := b.Engine.Driver
	store := b.Engine.Images
	salt := ""
	if opts.NoCache {
		// A fresh salt gives every layer a fresh ID, so nothing already in the
		// store is reused or overwritten.
		salt = fsutil.RandomHex(8)
	}

	// The base decides the guest OS, which decides which layers and steps apply.
	var parent string
	var guestOS machine.OS
	var install *Install
	if spec.From.Install != nil {
		in := *spec.From.Install
		in.Media, in.Edition, in.Disk = expand(in.Media, args), expand(in.Edition, args), expand(in.Disk, args)
		install = &in
		guestOS = in.OS
	} else {
		ref := expand(spec.From.Image, args)
		if parent, err = store.Resolve(ref); err != nil {
			return nil, fmt.Errorf("from.image: %w", err)
		}
		layer, err := store.Layer(parent)
		if err != nil {
			return nil, err
		}
		if layer.Driver != driver.Name() {
			return nil, fmt.Errorf("from.image %s was built with driver %s, not %s", ref, layer.Driver, driver.Name())
		}
		guestOS = layer.GuestOS
	}
	if !driver.Capabilities().SupportsOS(guestOS) {
		return nil, fmt.Errorf("driver %s cannot run %s guests", driver.Name(), guestOS)
	}
	p, err := plan(spec, args, contextDir, guestOS)
	if err != nil {
		return nil, err
	}

	if install != nil {
		if parent, err = b.install(ctx, p, *install, contextDir, opts.Agent, salt); err != nil {
			return nil, err
		}
	} else {
		b.logf("==> from %s (%s)", expand(spec.From.Image, args), image.Short(parent))
	}
	for _, name := range p.Skipped {
		b.logf("==> %s: skipped, nothing in it applies to %s", name, guestOS)
	}
	for i, layer := range p.Layers {
		key := layerKey(parent, driver.Name(), guestOS, layer, salt)
		prefix := fmt.Sprintf("[%d/%d] %s", i+1, len(p.Layers), layer.Name)
		if store.Has(key) {
			b.logf("==> %s: CACHED %s", prefix, image.Short(key))
			parent = key
			continue
		}
		b.logf("==> %s", prefix)
		started := time.Now()
		if err := b.buildLayer(ctx, p, spec.Name, parent, key, layer); err != nil {
			return nil, fmt.Errorf("%s: %w", prefix, err)
		}
		b.logf("==> %s: committed %s in %s", prefix, image.Short(key), time.Since(started).Round(time.Second))
		parent = key
	}

	for _, ref := range append([]string{p.Ref}, opts.Tags...) {
		if err := store.Tag(ref, parent); err != nil {
			return nil, err
		}
		b.logf("==> tagged %s", image.NormalizeRef(ref))
	}
	return &Result{Ref: p.Ref, Layer: parent}, nil
}

// install creates (or finds cached) the base layer for a from.install build.
func (b *Builder) install(ctx context.Context, p *Plan, in Install, contextDir, agent, salt string) (string, error) {
	driver, store := b.Engine.Driver, b.Engine.Images
	mediaPath := ""
	if in.Media != "latest" {
		if filepath.IsAbs(in.Media) || filepath.VolumeName(in.Media) != "" {
			mediaPath = in.Media
		} else {
			resolved, err := resolveContext(contextDir, in.Media)
			if err != nil {
				return "", fmt.Errorf("from.install.media: %w", err)
			}
			mediaPath = resolved
		}
	}
	key, err := installKey(driver.Name(), in, mediaPath, salt)
	if err != nil {
		return "", err
	}
	if store.Has(key) {
		b.logf("==> install %s: CACHED %s", in.OS, image.Short(key))
		return key, nil
	}
	if agent == "" {
		if in.OS != machine.OS(runtime.GOOS) {
			return "", fmt.Errorf("installing a %s guest from a %s host needs --agent, the disco-vm binary built for %s", in.OS, runtime.GOOS, in.OS)
		}
		agent = b.Engine.Exe
	}
	diskBytes, err := units.ParseBytes(in.Disk)
	if err != nil {
		return "", fmt.Errorf("from.install.disk: %w", err)
	}
	media := in.Media
	if mediaPath != "" {
		media = mediaPath
	}
	b.logf("==> install %s from %s", in.OS, media)
	pending, err := store.Begin(image.Layer{ID: key, Driver: driver.Name(), GuestOS: in.OS, Comment: "install " + string(in.OS)})
	if err != nil {
		return "", err
	}
	err = driver.Install(ctx, machine.InstallSpec{
		GuestOS:   in.OS,
		Media:     media,
		Edition:   in.Edition,
		DiskBytes: int64(diskBytes),
		CPUs:      p.CPUs,
		Memory:    p.Memory,
		Agent:     agent,
		Options:   in.Options,
		Log:       b.Out,
	}, pending.MachineLayer())
	if err != nil {
		pending.Abort()
		return "", fmt.Errorf("install %s: %w", in.OS, err)
	}
	if _, err := pending.Commit(); err != nil {
		return "", err
	}
	return key, nil
}

// buildLayer boots the parent, runs the layer's steps, shuts the guest down in
// order, and commits its disk as the layer key.
func (b *Builder) buildLayer(ctx context.Context, p *Plan, name, parent, key string, layer PlannedLayer) (err error) {
	e := b.Engine
	inst, err := e.Create(ctx, parent, engine.CreateOptions{Temporary: true, CPUs: p.CPUs, Memory: p.Memory})
	if err != nil {
		return err
	}
	booted, err := e.Boot(ctx, inst, io.Discard)
	if err != nil {
		e.Discard(context.Background(), inst)
		return fmt.Errorf("boot: %w", err)
	}
	defer func() {
		if err != nil {
			if booted != nil {
				_ = booted.Machine.Kill(context.Background())
			}
			e.Discard(context.Background(), inst)
		}
	}()

	for i, step := range layer.Steps {
		b.logf("--> step %d/%d: %s", i+1, len(layer.Steps), step.Describe())
		stepCtx, cancel := context.WithTimeout(ctx, step.Timeout)
		switch step.Kind {
		case "run":
			var code int
			code, err = booted.Guest.Run(stepCtx, guest.ExecRequest{Argv: step.Argv, Env: step.Env, Dir: step.Workdir, User: step.User}, nil, b.Out, b.Out)
			if err == nil && code != 0 {
				err = fmt.Errorf("exited with code %d", code)
			}
		case "copy":
			err = copyIn(stepCtx, booted.Guest, step.CopySrc, step.CopyDst)
		case "reboot":
			if err = booted.Shutdown(stepCtx, 10*time.Minute); err == nil {
				booted, err = e.Boot(stepCtx, inst, io.Discard)
			} else {
				booted = nil
			}
		}
		cancel()
		if err != nil {
			return fmt.Errorf("step %d (%s): %w", i+1, step.Describe(), err)
		}
	}

	// A layer is cut only from a clean shutdown. A guest that had to be forced
	// off may have unflushed writes, and caching that disk would poison every
	// build after this one.
	if err = booted.Shutdown(ctx, 10*time.Minute); err != nil {
		booted = nil
		return fmt.Errorf("shutdown before commit: %w", err)
	}
	booted = nil
	pending, err := e.Images.Begin(image.Layer{
		ID: key, Parent: parent, Driver: e.Driver.Name(), GuestOS: p.GuestOS,
		Comment: name + ":" + layer.Name,
	})
	if err != nil {
		return err
	}
	if _, err = e.Commit(ctx, inst, pending); err != nil {
		pending.Abort()
		return fmt.Errorf("commit: %w", err)
	}
	return nil
}

// copyIn streams a context file or directory into a guest directory.
func copyIn(ctx context.Context, client *guest.Client, src, dst string) error {
	if _, err := os.Stat(src); err != nil {
		return err
	}
	reader, writer := io.Pipe()
	go func() { writer.CloseWithError(guest.Pack(writer, src, true)) }()
	err := client.CopyTo(ctx, dst, reader)
	_ = reader.Close()
	return err
}

// ParseArgs reads KEY=VALUE build args.
func ParseArgs(pairs []string) (map[string]string, error) {
	out := map[string]string{}
	for _, pair := range pairs {
		k, v, ok := strings.Cut(pair, "=")
		if !ok || k == "" {
			return nil, errors.New("build arg " + pair + " is not KEY=VALUE")
		}
		out[k] = v
	}
	return out, nil
}
