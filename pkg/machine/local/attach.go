package local

import (
	"context"
	"fmt"
	"io"
	"net"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/discobox-ai/vm/internal/fsutil"
	"github.com/discobox-ai/vm/internal/proc"
	"github.com/discobox-ai/vm/pkg/guest"
	"github.com/discobox-ai/vm/pkg/machine"
)

// Attach reaches the instance's machine through its shim, as any process can.
func (d *Driver) Attach(ctx context.Context, inst machine.Instance) (machine.Machine, error) {
	if err := d.authorizeInstance(ctx, inst); err != nil {
		return nil, err
	}
	shim, err := d.openShim(inst.ID)
	if err != nil {
		return nil, fmt.Errorf("instance %s: %w", inst.Name, machine.ErrNotRunning)
	}
	statusCtx, cancel := context.WithTimeout(ctx, 2*time.Second)
	defer cancel()
	if err := shim.status(statusCtx); err != nil {
		return nil, fmt.Errorf("instance %s: %w", inst.Name, machine.ErrNotRunning)
	}
	m := &shimMachine{shim: shim, done: make(chan struct{})}
	m.Client = guest.NewClient(func(ctx context.Context) (net.Conn, error) { return shim.dialGuest(ctx) })
	return m, nil
}

// View opens a window on the instance's desktop in this process, through its
// shim, on a hypervisor that can (MachineViewer). One that shows a window
// only from the process that booted it refuses it as not implemented yet.
func (d *Driver) View(ctx context.Context, inst machine.Instance, title string) error {
	viewer, ok := d.hv.(MachineViewer)
	if !ok {
		return fmt.Errorf("%s opens a window only when it boots an instance (start --gui): %w", d.hv.Name(), machine.ErrNotImplemented)
	}
	m, err := d.Attach(ctx, inst)
	if err != nil {
		return err
	}
	return viewer.View(ctx, m, title)
}

// ViewInstall opens a window in this process on the pool's install in
// progress of the VM name, or on its only one when name is empty, on a
// hypervisor that can (MachineViewer).
func (d *Driver) ViewInstall(ctx context.Context, name, title string, log io.Writer) error {
	if err := d.authorize(ctx); err != nil {
		return err
	}
	viewer, ok := d.hv.(MachineViewer)
	if !ok {
		return fmt.Errorf("%s opens a window on an install only from the install (install --gui): %w", d.hv.Name(), machine.ErrNotImplemented)
	}
	paths, _ := filepath.Glob(filepath.Join(d.instancesDir(), "*", installingFile))
	var found []instanceRecord
	for _, path := range paths {
		var rec instanceRecord
		if fsutil.ReadJSON(path, &rec) != nil || rec.Pool != d.pool || name != "" && rec.Name != name {
			continue
		}
		found = append(found, rec)
	}
	switch len(found) {
	case 0:
		if name == "" {
			return fmt.Errorf("no install is in progress: %w", machine.ErrNotFound)
		}
		return fmt.Errorf("no install of %s is in progress: %w", name, machine.ErrNotFound)
	case 1:
	default:
		names := make([]string, len(found))
		for i, rec := range found {
			names[i] = rec.Name
		}
		slices.Sort(names)
		return fmt.Errorf("installs of %s are in progress; name one", strings.Join(names, ", "))
	}
	rec := found[0]
	hs, err := d.spec(rec)
	if err != nil {
		return err
	}
	if err := viewer.ViewInstall(ctx, Layer{ID: rec.ID, Dir: hs.Dir}, title, log); err != nil {
		return fmt.Errorf("install of %s: %w", rec.Name, err)
	}
	return nil
}

// shimMachine is a running machine its shim holds. Its guest operations are
// the shim's relay of the machine's; it is stopped once the shim has exited,
// which a shim does when its machine stops.
type shimMachine struct {
	*guest.Client
	shim     *shimClient
	done     chan struct{}
	watching sync.Once
}

func (m *shimMachine) Kill(ctx context.Context) error {
	err := m.shim.kill(ctx)
	if err != nil && !proc.Alive(m.shim.state.PID) {
		return nil
	}
	return err
}

func (m *shimMachine) Done() <-chan struct{} {
	m.watching.Do(func() {
		go func() {
			for proc.Alive(m.shim.state.PID) {
				time.Sleep(100 * time.Millisecond)
			}
			close(m.done)
		}()
	})
	return m.done
}

// Dial reaches a guest port through the shim, which dials it on the machine
// it holds: the agent's or the service's (guest.DialablePort).
func (m *shimMachine) Dial(ctx context.Context, port uint32) (net.Conn, error) {
	return m.shim.dial(ctx, port)
}

// Err is nil: how the machine stopped is in its shim's log.
func (m *shimMachine) Err() error {
	<-m.Done()
	return nil
}
