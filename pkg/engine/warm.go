package engine

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"sync"
	"time"

	"github.com/discobox-ai/vm/internal/fsutil"
	"github.com/discobox-ai/vm/internal/units"
	"github.com/discobox-ai/vm/pkg/machine"
)

// A warm image is one the driver has staged so that clones skip the cold boot:
// vz saves machine states to resume, and HCS freezes a live template to fork.
// A stage belongs to the image's top layer and lives in warm/<layer>/, with
// the driver's files in machine/.
//
// Warming runs in a warm shim, `disco-vm shim --warm <layer>`, for the same
// reason an instance runs in a shim: a stage that lives in memory (an HCS
// template) must stay resident in the process that made it, and that cannot
// be the command that asked for it. A stage that lives on disk lets the warm
// shim exit as soon as it is written.

const warmStateName = "warm.json"

// warmState is what was asked of a stage. The warm shim reads it, so the
// options need no flags of their own.
type warmState struct {
	Layer   string        `json:"layer"`
	Image   string        `json:"image"`
	Count   int           `json:"count"`
	CPUs    int           `json:"cpus,omitempty"`
	Memory  uint64        `json:"memory,omitempty"`
	User    *machine.User `json:"user,omitempty"`
	Created time.Time     `json:"created"`
}

func (e *Engine) warmDir(layer string) string { return filepath.Join(e.Root, "warm", layer) }

func (e *Engine) warmMachineDir(layer string) string {
	return filepath.Join(e.warmDir(layer), "machine")
}

func (e *Engine) warmer() (machine.Warmer, error) {
	warmer, ok := e.Driver.(machine.Warmer)
	if !ok {
		return nil, fmt.Errorf("driver %s cannot warm images: %w", e.Driver.Name(), machine.ErrUnsupported)
	}
	return warmer, nil
}

func (e *Engine) readWarm(layer string) (*warmState, error) {
	var st warmState
	if err := fsutil.ReadJSON(filepath.Join(e.warmDir(layer), warmStateName), &st); err != nil {
		return nil, err
	}
	return &st, nil
}

func (e *Engine) warmSpec(st *warmState) (machine.WarmSpec, error) {
	layer, err := e.Images.Layer(st.Layer)
	if err != nil {
		return machine.WarmSpec{}, err
	}
	chain, err := e.Images.MachineChain(st.Layer)
	if err != nil {
		return machine.WarmSpec{}, err
	}
	return machine.WarmSpec{
		GuestOS: layer.GuestOS, Chain: chain, Dir: e.warmMachineDir(st.Layer),
		Count: st.Count, CPUs: st.CPUs, Memory: st.Memory, User: st.User,
	}, nil
}

// IsWarm reports whether a layer has a stage, usable or not. A warm layer
// must be cooled before it can be deleted.
func (e *Engine) IsWarm(layer string) bool {
	_, err := os.Stat(e.warmDir(layer))
	return err == nil
}

// Warmth reports what a layer's stage can serve now; zero when it has none.
func (e *Engine) Warmth(ctx context.Context, layer string) machine.Warmth {
	cold := machine.Warmth{Mode: machine.Cold}
	warmer, err := e.warmer()
	if err != nil {
		return cold
	}
	st, err := e.readWarm(layer)
	if err != nil {
		return cold
	}
	spec, err := e.warmSpec(st)
	if err != nil {
		return cold
	}
	w, err := warmer.Warmth(ctx, spec)
	if err != nil || w.Clones == 0 {
		return cold
	}
	return w
}

// StageInfo is an image's stage as a person reads it.
type StageInfo struct {
	// Warmth is what the stage can serve now, and why not when it is held.
	machine.Warmth
	// Count is how many clones warm was asked to stage.
	Count int
	// User is the account the stage logs in, if any.
	User *machine.User
}

// Stage describes a layer's stage; ok is false when the layer has none.
func (e *Engine) Stage(ctx context.Context, layer string) (info StageInfo, ok bool) {
	st, err := e.readWarm(layer)
	if err != nil {
		return StageInfo{}, false
	}
	info = StageInfo{Warmth: machine.Warmth{Mode: machine.Cold}, Count: st.Count, User: st.User}
	if warmer, err := e.warmer(); err == nil {
		if spec, err := e.warmSpec(st); err == nil {
			if w, err := warmer.Warmth(ctx, spec); err == nil {
				info.Warmth = w
			}
		}
	}
	return info, true
}

// cloneMode resolves a requested mode against what the image is warm for.
func (e *Engine) cloneMode(ctx context.Context, ref, layer string, opts CreateOptions) (machine.CloneMode, error) {
	auto := opts.Mode == "" || opts.Mode == machine.Auto
	if opts.Mode == machine.Cold {
		return machine.Cold, nil
	}
	if !auto && !e.Driver.Capabilities().SupportsMode(opts.Mode) {
		return "", fmt.Errorf("driver %s cannot clone in %s mode: %w", e.Driver.Name(), opts.Mode, machine.ErrUnsupported)
	}
	w := e.Warmth(ctx, layer)
	if w.Clones == 0 || (!auto && w.Mode != opts.Mode) {
		if auto {
			return machine.Cold, nil
		}
		return "", notWarmError(fmt.Sprintf("image %s has no %s clones staged; run `disco-vm warm %s`", ref, opts.Mode, ref))
	}
	// A clone runs at its stage's size; a different size means a cold boot.
	if st, err := e.readWarm(layer); err != nil || st.CPUs != opts.CPUs || st.Memory != opts.Memory {
		if auto {
			return machine.Cold, nil
		}
		if err != nil {
			return "", err
		}
		return "", fmt.Errorf("image %s is warm at %s; clone it at that size or cold", ref, sizeOf(st.CPUs, st.Memory))
	}
	return w.Mode, nil
}

// notWarmError says what to do about an image that is not warm, and is
// machine.ErrNotWarm to errors.Is.
type notWarmError string

func (e notWarmError) Error() string { return string(e) }
func (notWarmError) Unwrap() error   { return machine.ErrNotWarm }

// userOf describes the user a stage logs in, for comparing and for messages.
func userOf(u *machine.User) string {
	if u == nil {
		return "without a user"
	}
	if u.UID != 0 {
		return fmt.Sprintf("as %s (uid %d)", u.Name, u.UID)
	}
	return "as " + u.Name
}

func sizeOf(cpus int, memory uint64) string {
	c, m := "the default CPUs", "the default memory"
	if cpus != 0 {
		c = fmt.Sprintf("%d CPUs", cpus)
	}
	if memory != 0 {
		m = units.FormatBytes(memory)
	}
	return c + " and " + m
}

// WarmOptions describes a stage.
type WarmOptions struct {
	// Count is how many clones to stage where a stage is a pool used once per
	// clone (fake)
	// (resume). Zero means one. Warming a warm image tops it up.
	Count int
	// CPUs and Memory size the staged machine, and so every clone of it.
	CPUs   int
	Memory uint64
	// User is created in the stage and logged in, so every clone of it starts
	// as that user.
	User *machine.User
	// Timeout bounds warming.
	Timeout time.Duration
}

// Warm stages an image for fast clones and reports what is ready.
func (e *Engine) Warm(ctx context.Context, ref string, opts WarmOptions) (machine.Warmth, error) {
	none := machine.Warmth{Mode: machine.Cold}
	if _, err := e.warmer(); err != nil {
		return none, err
	}
	layerID, err := e.Images.Resolve(ref)
	if err != nil {
		return none, err
	}
	layer, err := e.Images.Layer(layerID)
	if err != nil {
		return none, err
	}
	if layer.Driver != e.Driver.Name() {
		return none, fmt.Errorf("image %s was built for driver %s, not %s", ref, layer.Driver, e.Driver.Name())
	}
	if opts.Count <= 0 {
		opts.Count = 1
	}
	if opts.Timeout == 0 {
		opts.Timeout = 30 * time.Minute
	}
	dir := e.warmDir(layerID)
	if prev, err := e.readWarm(layerID); err == nil && (prev.CPUs != opts.CPUs || prev.Memory != opts.Memory) {
		return none, fmt.Errorf("image %s is already warm at %s; `disco-vm warm --rm %s` first", ref, sizeOf(prev.CPUs, prev.Memory), ref)
	}
	if prev, err := e.readWarm(layerID); err == nil && userOf(prev.User) != userOf(opts.User) {
		return none, fmt.Errorf("image %s is already warm %s; `disco-vm warm --rm %s` first", ref, userOf(prev.User), ref)
	}
	if shim, err := openShim(filepath.Join(dir, shimStateName)); err == nil && shim.status(ctx) == nil {
		// A resident stage is up, and it serves any number of clones.
		return e.Warmth(ctx, layerID), nil
	}
	if err := os.MkdirAll(e.warmMachineDir(layerID), 0o755); err != nil {
		return none, err
	}
	st := warmState{Layer: layerID, Image: ref, Count: opts.Count, CPUs: opts.CPUs, Memory: opts.Memory, User: opts.User, Created: time.Now().UTC()}
	if err := fsutil.WriteJSON(filepath.Join(dir, warmStateName), st); err != nil {
		return none, err
	}
	fail := func(err error) (machine.Warmth, error) {
		// Keep what an earlier warm staged; drop a stage that never was.
		if e.Warmth(context.Background(), layerID).Clones == 0 {
			_ = e.cool(context.Background(), layerID)
		}
		return none, err
	}

	_ = os.Remove(filepath.Join(dir, shimStateName))
	logFile, err := os.OpenFile(filepath.Join(dir, "shim.log"), os.O_WRONLY|os.O_CREATE|os.O_APPEND, 0o600)
	if err != nil {
		return fail(err)
	}
	exited, err := e.spawnShim([]string{"--warm", layerID}, logFile)
	// The shim has its own handle; ours would keep a failed stage's directory
	// from being removed on Windows.
	logFile.Close()
	if err != nil {
		return fail(err)
	}
	ctx, cancel := context.WithTimeout(ctx, opts.Timeout)
	defer cancel()
	for {
		// Warm is done when the shim exits cleanly (a stage on disk) or
		// publishes its control API (a resident stage).
		select {
		case err := <-exited:
			if err != nil {
				return fail(fmt.Errorf("warm %s: %v\n%s", ref, err, tail(filepath.Join(dir, "shim.log"))))
			}
			return e.Warmth(ctx, layerID), nil
		case <-ctx.Done():
			if shim, err := openShim(filepath.Join(dir, shimStateName)); err == nil {
				_ = shim.stop(context.Background(), time.Minute)
			}
			return fail(fmt.Errorf("warm %s: %w", ref, ctx.Err()))
		case <-time.After(50 * time.Millisecond):
		}
		if _, err := openShim(filepath.Join(dir, shimStateName)); err == nil {
			return e.Warmth(ctx, layerID), nil
		}
	}
}

// Cool releases an image's stage.
func (e *Engine) Cool(ctx context.Context, ref string) error {
	layerID, err := e.Images.Resolve(ref)
	if err != nil {
		return err
	}
	if !e.IsWarm(layerID) {
		return fmt.Errorf("image %s is not warm", ref)
	}
	return e.cool(ctx, layerID)
}

func (e *Engine) cool(ctx context.Context, layerID string) error {
	dir := e.warmDir(layerID)
	if shim, err := openShim(filepath.Join(dir, shimStateName)); err == nil {
		if err := shim.stop(ctx, time.Minute); err != nil {
			return err
		}
	}
	if warmer, err := e.warmer(); err == nil {
		if st, err := e.readWarm(layerID); err == nil {
			if spec, err := e.warmSpec(st); err == nil {
				if err := warmer.Cool(ctx, spec); err != nil {
					return err
				}
			}
		}
	}
	return os.RemoveAll(dir)
}

// RunWarmShim is the body of `disco-vm shim --warm <layer>`: stage the layer,
// and hold the stage until it is cooled if it lives in memory.
func (e *Engine) RunWarmShim(ctx context.Context, layerID string) error {
	warmer, err := e.warmer()
	if err != nil {
		return err
	}
	st, err := e.readWarm(layerID)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return fmt.Errorf("layer %s has no warm request", layerID)
		}
		return err
	}
	spec, err := e.warmSpec(st)
	if err != nil {
		return err
	}
	spec.Log = os.Stderr
	stage, err := warmer.Warm(ctx, spec)
	if err != nil || stage == nil {
		return err
	}

	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		_ = stage.Close(context.Background())
		return err
	}
	var closeOnce sync.Once
	var closeErr error
	closeStage := func() error {
		closeOnce.Do(func() { closeErr = stage.Close(context.Background()) })
		return closeErr
	}
	mux := http.NewServeMux()
	mux.HandleFunc("GET /status", func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
	})
	mux.HandleFunc("POST /stop", func(w http.ResponseWriter, _ *http.Request) {
		if err := closeStage(); err != nil {
			http.Error(w, err.Error(), http.StatusOK)
			return
		}
		w.WriteHeader(http.StatusNoContent)
	})
	state, closeControl, err := serveControl(listener, mux, filepath.Join(e.warmDir(layerID), shimStateName))
	if err != nil {
		_ = closeStage()
		return err
	}
	fmt.Fprintf(os.Stderr, "shim: %s warm, control on %s\n", st.Image, state.Addr)
	select {
	case <-stage.Done():
	case <-ctx.Done():
		_ = closeStage()
	}
	closeControl()
	fmt.Fprintf(os.Stderr, "shim: %s cooled\n", st.Image)
	return nil
}
