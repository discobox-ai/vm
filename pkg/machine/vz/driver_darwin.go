package vz

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"

	"github.com/discobox-ai/vm/pkg/machine"
)

func init() {
	machine.Register("vz", func() (machine.Driver, error) { return &Driver{}, nil })
}

// Driver runs macOS guests under Virtualization.framework.
type Driver struct{}

var (
	_ machine.Driver = (*Driver)(nil)
	_ machine.Warmer = (*Driver)(nil)
)

func (*Driver) Name() string { return "vz" }

// Capabilities: APFS clones, saved states that resume once, and the
// framework's limit of two concurrently running macOS guests.
func (*Driver) Capabilities() machine.Capabilities {
	return machine.Capabilities{
		GuestOS:           []machine.OS{machine.Darwin},
		CloneModes:        []machine.CloneMode{machine.Cold, machine.Resume},
		MaxRunning:        map[machine.OS]int{machine.Darwin: 2},
		SharedDirectories: true,
		Display:           true,
	}
}

// Commit moves the stopped instance's disk into the layer. A saved state, a
// MAC address, and the machine identifier stay behind: they are the running
// machine's identity, and every clone of the layer gets its own.
func (*Driver) Commit(_ context.Context, inst machine.InstanceSpec, dst machine.Layer) error {
	return moveLayer(bundle(inst.Dir), bundle(dst.Dir))
}

func (*Driver) Destroy(_ context.Context, inst machine.InstanceSpec) error {
	return os.RemoveAll(inst.Dir)
}

func (*Driver) DeleteLayer(context.Context, machine.Layer) error { return nil }

func (*Driver) Cool(_ context.Context, spec machine.WarmSpec) error {
	return os.RemoveAll(spec.Dir)
}

// A stage is a pool of complete bundles under <stage>/states, each with the
// disk exactly as it was when its state was saved: a state resumes only
// against that disk. One is taken whole, by rename, per resume clone.
const statesName = "states"

// stagedStates lists a stage's usable bundles. With prune it also removes any
// this host can no longer restore.
func stagedStates(warmDir string, prune bool) ([]bundle, error) {
	if warmDir == "" {
		return nil, nil
	}
	dir := filepath.Join(warmDir, statesName)
	entries, err := os.ReadDir(dir)
	if errors.Is(err, fs.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	build := hostBuild()
	var out []bundle
	for _, entry := range entries {
		b := bundle(filepath.Join(dir, entry.Name()))
		m, err := b.readMeta()
		_, serr := os.Stat(b.path(stateName))
		if err != nil || serr != nil || m.Saved == nil || m.Saved.HostBuild != build {
			// Written before a host update, or never finished: a restore
			// would be rejected, so it is not a clone this stage can serve.
			if prune {
				_ = os.RemoveAll(string(b))
			}
			continue
		}
		out = append(out, b)
	}
	return out, nil
}

// takeState moves one staged bundle into dir. A rename is atomic, so two
// clones racing for the last state cannot both win.
func takeState(warmDir, dir string) error {
	states, err := stagedStates(warmDir, true)
	if err != nil {
		return err
	}
	// Prepare's directory is empty if it exists; rename needs it gone.
	_ = os.Remove(dir)
	if err := os.MkdirAll(filepath.Dir(dir), 0o700); err != nil {
		return err
	}
	for _, state := range states {
		if err := os.Rename(string(state), dir); err == nil {
			return nil
		}
	}
	return fmt.Errorf("vz: %w", machine.ErrNotWarm)
}
