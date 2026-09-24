package vz

import (
	"context"
	"errors"
	"io/fs"
	"os"
	"path/filepath"

	"golang.org/x/sys/unix"

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
		Forward:           true,
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

// A stage is a set of templates under <stage>/templates: complete bundles,
// each with its own identity (machine identifier and MAC), its saved state,
// and the disk exactly as it was when the state was saved, since a state
// restores only against that disk and only under that identity. A template is
// never used up. Each resume clone is an APFS clone of one, taken at boot.
//
// Two clones of one template must never run at once: they would share a MAC,
// and so an address on the NAT, and a machine identifier, which is undefined
// behavior in the guest. The framework runs at most two macOS guests at once,
// so a stage has two templates, and a booting clone locks a free one for as
// long as its VM runs.
const (
	templatesName = "templates"
	// templateCount is the framework's limit on running macOS guests.
	templateCount = 2
	lockName      = "lock"
	// pendingName marks a resume clone that has not had its first boot: Prepare
	// leaves it, and the boot takes a template.
	pendingName = "resume.pending"
)

// templates lists a stage's usable templates. With prune it also removes any
// this host can no longer restore.
func templates(warmDir string, prune bool) ([]bundle, error) {
	if warmDir == "" {
		return nil, nil
	}
	dir := filepath.Join(warmDir, templatesName)
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
			// would be rejected, so it is not a template this stage can use.
			if prune {
				_ = os.RemoveAll(string(b))
			}
			continue
		}
		out = append(out, b)
	}
	return out, nil
}

// templateFiles are what a resume clone copies from its template.
var templateFiles = append([]string{identifierName, macName, stateName}, layerFiles...)

// lockTemplate takes a template for one VM's life, or reports false when a
// running VM already has it. The lock is the process's: it goes when the
// process does, so a shim that dies frees its template.
func lockTemplate(t bundle) (*os.File, bool) {
	f, err := os.OpenFile(t.path(lockName), os.O_RDWR|os.O_CREATE, 0o600)
	if err != nil {
		return nil, false
	}
	if err := unix.Flock(int(f.Fd()), unix.LOCK_EX|unix.LOCK_NB); err != nil {
		f.Close()
		return nil, false
	}
	return f, true
}

// cloneTemplate clones a template's files into an instance's bundle.
func cloneTemplate(t, dst bundle) error {
	for _, name := range templateFiles {
		_ = os.Remove(dst.path(name))
		if err := clonefile(t.path(name), dst.path(name)); err != nil {
			return err
		}
	}
	return nil
}
