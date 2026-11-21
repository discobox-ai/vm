package fake

import (
	"context"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"

	"github.com/discobox-ai/vm/pkg/machine"
	"github.com/discobox-ai/vm/pkg/machine/local"
)

// pausedName marks a running fake machine paused, as a provider pauses one
// (Archil at a session's limit). Its guest process keeps running: the
// pause is only what the driver reports, and what Start, or the guest's
// first exec or copy, undoes.
const pausedName = "paused"

// Pause pauses a running fake VM as a provider would: the driver then
// reports it machine.StatePaused, reading that wakes nothing, its Start
// resumes it, a stop stops it as it is, and its first exec or copy resumes
// it as archil's does. It is a test hook, so that a caller can test how it
// handles a pause with no provider account. d is a fake driver (machine.New
// "fake"); a VM already paused is left so, and one not running is
// machine.ErrNotRunning.
func Pause(ctx context.Context, d machine.Driver, inst machine.Instance) error {
	ld, ok := d.(*local.Driver)
	if !ok {
		return fmt.Errorf("fake: pause: %T is not a fake driver: %w", d, machine.ErrUnsupported)
	}
	if _, ok := ld.Hypervisor().(*Hypervisor); !ok {
		return fmt.Errorf("fake: pause: the %s driver is not the fake's: %w", ld.Name(), machine.ErrUnsupported)
	}
	state, err := ld.State(ctx, inst)
	if err != nil {
		return err
	}
	if state == machine.StateStopped {
		return fmt.Errorf("fake: pause %s: %w", inst.Name, machine.ErrNotRunning)
	}
	hs, err := ld.Spec(ctx, inst)
	if err != nil {
		return err
	}
	return os.WriteFile(pausedFile(hs.Dir), nil, 0o644) //nolint:gosec // G306: an empty marker
}

func pausedFile(dir string) string { return filepath.Join(dir, pausedName) }

// Paused reports a running instance Pause paused.
func (*Hypervisor) Paused(inst local.InstanceSpec) bool {
	_, err := os.Stat(pausedFile(inst.Dir))
	return err == nil
}

// Resume undoes Pause.
func (*Hypervisor) Resume(_ context.Context, inst local.InstanceSpec) error {
	return unpause(inst.Dir)
}

func unpause(dir string) error {
	if err := os.Remove(pausedFile(dir)); err != nil && !errors.Is(err, fs.ErrNotExist) {
		return fmt.Errorf("fake: resume: %w", err)
	}
	return nil
}

// Exec resumes a paused guest first, as archil's first exec does.
func (m *proc) Exec(ctx context.Context, req machine.ExecRequest) (machine.Process, error) {
	if err := unpause(m.dir); err != nil {
		return nil, err
	}
	return m.Client.Exec(ctx, req)
}

// CopyTo resumes a paused guest first, as archil's first copy does.
func (m *proc) CopyTo(ctx context.Context, dir string, tarStream io.Reader) error {
	if err := unpause(m.dir); err != nil {
		return err
	}
	return m.Client.CopyTo(ctx, dir, tarStream)
}

// CopyFrom resumes a paused guest first, as archil's first copy does.
func (m *proc) CopyFrom(ctx context.Context, path string) (io.ReadCloser, error) {
	if err := unpause(m.dir); err != nil {
		return nil, err
	}
	return m.Client.CopyFrom(ctx, path)
}
