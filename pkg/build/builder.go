package build

import (
	"context"
	"errors"
	"fmt"
	"io"
	"maps"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"time"

	"github.com/discobox-ai/vm/internal/fsutil"
	"github.com/discobox-ai/vm/internal/units"
	"github.com/discobox-ai/vm/pkg/engine"
	"github.com/discobox-ai/vm/pkg/guest"
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
	// Tags name the result, as `docker build -t` does. Without one the result
	// is only its image ID.
	Tags []string
	// NoCache rebuilds every layer.
	NoCache bool
	// Agent is the disco-vm binary for the guest, baked in at install by a
	// driver that lists Capabilities.Agent. It defaults to this binary, which
	// is right when guest and host OS match.
	Agent string
	// GUI shows every build VM's display in a window, in this process.
	GUI bool
}

// Result is a finished build.
type Result struct {
	// Ref is the first tag, or empty for an untagged build.
	Ref   string
	Layer string
}

// Builder runs builds against an engine.
type Builder struct {
	Engine *engine.Engine
	// Out receives the build log and every step's output.
	Out io.Writer

	gui bool
}

func (b *Builder) logf(format string, args ...any) {
	fmt.Fprintf(b.Out, format+"\n", args...)
}

// Build runs a spec to completion and tags the result.
func (b *Builder) Build(ctx context.Context, opts Options) (*Result, error) {
	if err := b.Engine.CheckGUI(opts.GUI); err != nil {
		return nil, err
	}
	b.gui = opts.GUI
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
	args := maps.Clone(spec.Args)
	if args == nil {
		args = map[string]string{}
	}
	for k, v := range opts.Args {
		if _, declared := spec.Args[k]; !declared {
			return nil, fmt.Errorf("build arg %s is not declared in the spec's args", k)
		}
		args[k] = v
	}
	for _, tag := range opts.Tags {
		if err := engine.ValidateRef(tag); err != nil {
			return nil, err
		}
	}

	driver := b.Engine.Driver
	// A project other than the default is part of every layer's key, so no
	// two projects ever share a layer ID; the default's keys are those from
	// before projects, so its cache stays valid.
	salt := ""
	if project := b.Engine.Project; project != machine.DefaultProject {
		salt = "project " + project + "|"
	}
	if opts.NoCache {
		// A fresh salt gives every layer a fresh ID, so nothing the driver
		// already has is reused or overwritten.
		salt += fsutil.RandomHex(8)
	}

	// The base decides the guest OS, which decides which layers and steps apply.
	// parent is the layer the next one is built on, and from is what its
	// build instance is created from: the same layer, or for the first layer
	// on a base of the driver's, the base, which has no layer to be a parent.
	var parent, from, parentKey string
	var guestOS machine.OS
	var install *Install
	if spec.From.Install != nil {
		in := *spec.From.Install
		in.Options = expandMap(in.Options, args)
		in.Media, in.Edition, in.Disk = expand(in.Media, args), expand(in.Edition, args), expand(in.Disk, args)
		install = &in
		guestOS = in.OS
	} else {
		ref := expand(spec.From.Image, args)
		if layer, err := b.Engine.Resolve(ctx, ref); err == nil {
			parent, guestOS = layer.ID, layer.GuestOS
			b.logf("==> from %s (%s)", ref, engine.Short(parent))
		} else {
			base, err := b.Engine.ResolveBase(ctx, ref, err)
			if err != nil {
				return nil, fmt.Errorf("from.image: %w", err)
			}
			// A layer's key names its parent; the first one on a base names
			// the base, by version, so a base captured again builds again.
			parent, from, guestOS = "", base.ID, base.GuestOS
			parentKey = "base " + driver.Name() + " " + base.ID + "@" + base.Version
			b.logf("==> from %s, one of %s's bases", ref, driver.Name())
		}
	}
	if !driver.Capabilities().SupportsOS(guestOS) {
		return nil, fmt.Errorf("driver %s cannot run %s guests", driver.Name(), guestOS)
	}
	p, err := plan(spec, args, contextDir, guestOS)
	if err != nil {
		return nil, err
	}
	if parent == "" && install == nil && len(p.Layers) == 0 {
		return nil, fmt.Errorf("from.image %s is one of %s's bases, not an image, and a build of it with no layers makes nothing", expand(spec.From.Image, args), driver.Name())
	}

	if install != nil {
		if parent, err = b.install(ctx, p, *install, contextDir, opts.Agent, salt); err != nil {
			return nil, err
		}
	}
	if parentKey == "" {
		parentKey, from = parent, parent
	}
	for _, name := range p.Skipped {
		b.logf("==> %s: skipped, nothing in it applies to %s", name, guestOS)
	}
	for i, layer := range p.Layers {
		prefix := fmt.Sprintf("[%d/%d] %s", i+1, len(p.Layers), layer.Name)
		key := layerKey(parentKey, driver.Name(), guestOS, layer, salt)
		cached, err := b.cached(ctx, key)
		if err != nil {
			return nil, fmt.Errorf("%s: %w", prefix, err)
		}
		if cached {
			b.logf("==> %s: CACHED %s", prefix, engine.Short(key))
			parent, from, parentKey = key, key, key
			continue
		}
		b.logf("==> %s", prefix)
		started := time.Now()
		if err := b.buildLayer(ctx, p, buildName(opts), from, key, layer); err != nil {
			return nil, fmt.Errorf("%s: %w", prefix, err)
		}
		b.logf("==> %s: committed %s in %s", prefix, engine.Short(key), time.Since(started).Round(time.Second))
		parent, from, parentKey = key, key, key
	}

	result := &Result{Layer: parent}
	for _, ref := range opts.Tags {
		if err := b.Engine.Tag(ctx, ref, parent); err != nil {
			return nil, err
		}
		b.logf("==> tagged %s", engine.NormalizeRef(ref))
	}
	if len(opts.Tags) > 0 {
		result.Ref = engine.NormalizeRef(opts.Tags[0])
	} else {
		b.logf("==> built %s, untagged; name it with -t, or `disco-vm tag %s NAME[:TAG]`", engine.Short(parent), engine.Short(parent))
	}
	return result, nil
}

// buildName names a build in layer comments and window titles: its first
// tag, or its spec's file name.
func buildName(opts Options) string {
	if len(opts.Tags) > 0 {
		return engine.NormalizeRef(opts.Tags[0])
	}
	return filepath.Base(opts.File)
}

// install creates (or finds cached) the base layer for a from.install build.
func (b *Builder) install(ctx context.Context, p *Plan, in Install, contextDir, agent, salt string) (string, error) {
	driver := b.Engine.Driver
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
	cached, err := b.cached(ctx, key)
	if err != nil {
		return "", err
	}
	if cached {
		b.logf("==> install %s: CACHED %s", in.OS, engine.Short(key))
		return key, nil
	}
	if agent == "" && driver.Capabilities().Agent {
		if in.OS != machine.OS(runtime.GOOS) {
			return "", fmt.Errorf("installing a %s guest from a %s host needs --agent, the disco-vm binary built for %s", in.OS, runtime.GOOS, in.OS)
		}
		if agent, err = os.Executable(); err != nil {
			return "", err
		}
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
	_, err = driver.Install(ctx, machine.InstallSpec{
		GuestOS:   in.OS,
		Media:     media,
		Edition:   in.Edition,
		DiskBytes: int64(diskBytes),
		CPUs:      p.CPUs,
		Memory:    p.Memory,
		Agent:     agent,
		Options:   in.Options,
		GUI:       b.gui,
		Log:       b.Out,
	}, machine.Layer{ID: key, GuestOS: in.OS, Comment: "install " + string(in.OS)})
	if err != nil {
		return "", fmt.Errorf("install %s: %w", in.OS, err)
	}
	return key, nil
}

// cached reports whether the driver has the layer its inputs give the ID key.
// One it lost (a boxd snapshot or a Docker image deleted behind disco-vm's
// back) is not there, so it is built again under the same ID.
func (b *Builder) cached(ctx context.Context, key string) (bool, error) {
	_, err := b.Engine.Driver.Layer(ctx, key)
	if errors.Is(err, machine.ErrNotFound) {
		return false, nil
	}
	return err == nil, err
}

// buildLayer boots an instance of from (the parent layer, or a base), runs the
// layer's steps, shuts the guest down in order, and commits its disk as the
// layer key on from's layer.
func (b *Builder) buildLayer(ctx context.Context, p *Plan, name, from, key string, layer PlannedLayer) (err error) {
	e := b.Engine
	inst, err := e.Create(ctx, from, engine.CreateOptions{Temporary: true, CPUs: p.CPUs, Memory: p.Memory})
	if err != nil {
		return err
	}
	boot := engine.BootOptions{Console: io.Discard, GUI: b.gui, Title: "disco-vm build: " + name + ": " + layer.Name}
	booted, err := e.Boot(ctx, inst, boot)
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
			code, err = machine.Run(stepCtx, booted.Machine, machine.ExecRequest{Argv: step.Argv, Env: step.Env, Dir: step.Workdir, User: step.User}, nil, b.Out, b.Out)
			if err == nil && code != 0 {
				err = fmt.Errorf("exited with code %d", code)
			}
		case "copy":
			err = copyIn(stepCtx, booted.Machine, step.CopySrc, step.CopyDst)
		case "reboot":
			if err = booted.Shutdown(stepCtx, 10*time.Minute); err == nil {
				booted, err = e.Boot(stepCtx, inst, boot)
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
	if _, err = e.Commit(ctx, inst, machine.Layer{ID: key, Comment: name + ":" + layer.Name}); err != nil {
		return fmt.Errorf("commit: %w", err)
	}
	return nil
}

// copyIn streams a context file or directory into a guest directory.
func copyIn(ctx context.Context, g machine.Guest, src, dst string) error {
	if _, err := os.Stat(src); err != nil {
		return err
	}
	reader, writer := io.Pipe()
	go func() { writer.CloseWithError(guest.Pack(writer, src, true)) }()
	err := g.CopyTo(ctx, dst, reader)
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
