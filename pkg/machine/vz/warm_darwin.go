//go:build darwin && cgo

package vz

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"time"

	"github.com/discobox-ai/vm/internal/fsutil"
	"github.com/discobox-ai/vm/pkg/machine"
)

// Warm stages resume clones until spec.Count are ready. Each one is a clone of
// the image with its own identity, booted cold, saved once its agent answers,
// and stopped. The pool is on disk, so Warm returns no Stage and the warm shim
// exits when it returns. Clones are staged one at a time: a staging boot counts
// against the framework's two running macOS guests.
func (d *Driver) Warm(ctx context.Context, spec machine.WarmSpec) (machine.Stage, error) {
	if len(spec.Chain) == 0 {
		return nil, errors.New("vz: warm: no image")
	}
	log := spec.Log
	if log == nil {
		log = io.Discard
	}
	parent := bundle(spec.Chain[len(spec.Chain)-1].Dir)
	m, err := parent.readMeta()
	if err != nil {
		return nil, err
	}
	cpus, memory := size(m, spec.CPUs, spec.Memory)
	states, err := stagedStates(spec.Dir, true)
	if err != nil {
		return nil, err
	}
	want := max(spec.Count, 1)
	for n := len(states); n < want; n++ {
		started := time.Now()
		// Staged under a temporary name and renamed, so Prepare never takes
		// a half-written state.
		tmp := bundle(filepath.Join(spec.Dir, "tmp-"+fsutil.RandomHex(6)))
		if err := stage(ctx, parent, tmp, cpus, memory); err != nil {
			_ = os.RemoveAll(string(tmp))
			return nil, err
		}
		if err := os.MkdirAll(filepath.Join(spec.Dir, statesName), 0o700); err != nil {
			return nil, err
		}
		if err := os.Rename(string(tmp), filepath.Join(spec.Dir, statesName, fsutil.RandomHex(6))); err != nil {
			_ = os.RemoveAll(string(tmp))
			return nil, err
		}
		fmt.Fprintf(log, "vz: staged resume clone %d of %d in %s\n", n+1, want, time.Since(started).Round(time.Second))
	}
	return nil, nil
}

// stage boots a fresh clone of parent in dst and saves its state.
func stage(ctx context.Context, parent, dst bundle, cpus uint, memory uint64) error {
	if err := cloneLayer(parent, dst); err != nil {
		return err
	}
	if err := dst.newIdentifier(); err != nil {
		return err
	}
	config, err := dst.configuration(cpus, memory, nil)
	if err != nil {
		return err
	}
	if ok, err := config.ValidateSaveRestoreSupport(); err != nil || !ok {
		if err == nil {
			err = errors.New("configuration rejected")
		}
		return fmt.Errorf("vz: this machine cannot be saved: %w", err)
	}
	vm, err := start(dst, cpus, memory, nil, nil)
	if err != nil {
		return err
	}
	defer func() { _ = vm.Kill(context.Background()) }()

	ready, cancel := context.WithTimeout(ctx, 10*time.Minute)
	agent := vm.agent()
	err = agent.WaitReady(ready, vm.Done())
	cancel()
	// Nothing of the host's may be connected when memory is saved: a restored
	// guest would hold connections to nobody.
	agent.Close()
	if err != nil {
		return fmt.Errorf("vz: stage: %w", err)
	}

	if err := vm.vm.Pause(); err != nil {
		return fmt.Errorf("vz: pause before saving: %w", err)
	}
	if err := vm.vm.SaveMachineStateToPath(dst.path(stateName)); err != nil {
		return fmt.Errorf("vz: save machine state: %w", err)
	}
	// Stopped while paused, so the disk is exactly what the saved memory
	// believes it is.
	if err := vm.Kill(ctx); err != nil {
		return err
	}
	m, err := dst.readMeta()
	if err != nil {
		return err
	}
	m.Saved = &saved{CPUs: cpus, Memory: memory, HostBuild: hostBuild()}
	return dst.writeMeta(m)
}

// Warmth counts the staged states this host can restore now. It reports none
// while the screen is locked, when every restore would be refused, so that an
// auto clone boots the image cold instead of a resume clone's disk.
func (*Driver) Warmth(_ context.Context, spec machine.WarmSpec) (machine.Warmth, error) {
	cold := machine.Warmth{Mode: machine.Cold}
	states, err := stagedStates(spec.Dir, false)
	if err != nil || len(states) == 0 || screenLocked() {
		return cold, err
	}
	return machine.Warmth{Mode: machine.Resume, Clones: len(states)}, nil
}
