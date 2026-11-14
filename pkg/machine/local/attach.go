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
	held := &shimProcess{shim: shim, done: make(chan struct{})}
	if shim.stale != nil {
		return &staleMachine{shimProcess: held, err: fmt.Errorf("instance %s: %w", inst.Name, shim.stale)}, nil
	}
	return &shimMachine{
		Client:      guest.NewClient(func(ctx context.Context) (net.Conn, error) { return shim.dialGuest(ctx) }),
		shimProcess: held,
	}, nil
}

// View opens a window on the instance's desktop in this process, through its
// shim, which reaches the guest's display for it (/display), on a
// hypervisor that reports Capabilities.View.
func (d *Driver) View(ctx context.Context, inst machine.Instance, title string) error {
	if !d.hv.Capabilities().View {
		return fmt.Errorf("%s opens a window only when it boots an instance (start --gui): %w", d.hv.Name(), machine.ErrUnsupported)
	}
	m, err := d.Attach(ctx, inst)
	if err != nil {
		return err
	}
	switch m := m.(type) {
	case *shimMachine:
		return d.hv.View(ctx, m, m.shim.display, title)
	case *staleMachine:
		return m.err
	default:
		return fmt.Errorf("instance %s: no window through a %T", inst.Name, m)
	}
}

// ViewInstall opens a window in this process on the pool's install in
// progress of the VM name, or on its only one when name is empty, on a
// hypervisor that reports Capabilities.View. An install whose process is
// gone (a crash left its record) is not in progress.
func (d *Driver) ViewInstall(ctx context.Context, name, title string, log io.Writer) error {
	if err := d.authorize(ctx); err != nil {
		return err
	}
	if !d.hv.Capabilities().View {
		return fmt.Errorf("%s opens a window on an install only from the install (install --gui): %w", d.hv.Name(), machine.ErrUnsupported)
	}
	paths, _ := filepath.Glob(filepath.Join(d.instancesDir(), "*", installingFile))
	var found []instanceRecord
	for _, path := range paths {
		var rec installingRecord
		if fsutil.ReadJSON(path, &rec) != nil || rec.Pool != d.pool || name != "" && rec.Name != name || !proc.Alive(rec.PID) {
			continue
		}
		found = append(found, rec.instanceRecord)
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
	if err := d.hv.ViewInstall(ctx, Layer{ID: rec.ID, Dir: hs.Dir}, title, log); err != nil {
		return fmt.Errorf("install of %s: %w", rec.Name, err)
	}
	return nil
}

// shimProcess is a running shim, as another process sees it: the machine it
// holds is killed through it, and stopped once it has exited, which a shim
// does when its machine stops.
type shimProcess struct {
	shim     *shimClient
	done     chan struct{}
	watching sync.Once
}

func (m *shimProcess) Kill(ctx context.Context) error {
	err := m.shim.kill(ctx)
	if err != nil && !proc.Alive(m.shim.state.PID) {
		return nil
	}
	return err
}

func (m *shimProcess) Done() <-chan struct{} {
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

// Err is nil: how the machine stopped is in its shim's log.
func (m *shimProcess) Err() error {
	<-m.Done()
	return nil
}

// shimMachine is a running machine its shim holds. Its guest operations are
// the shim's relay of the machine's.
type shimMachine struct {
	*guest.Client
	*shimProcess
}

// Dial reaches a guest port through the shim, which dials it on the machine
// it holds: the agent's or the service's (guest.DialablePort).
func (m *shimMachine) Dial(ctx context.Context, port uint32) (net.Conn, error) {
	return m.shim.dial(ctx, port)
}

// staleMachine is a running machine held by a shim of another protocol
// (shimState.usable). It is used only through the part of a shim that lasts:
// it reads as running, Shutdown asks the shim to stop it in order, and Kill
// and Done are a shimProcess's. Every other guest operation, and Dial,
// returns err, which wraps machine.ErrStale, so nothing is half done through
// a shim this build cannot speak to. Once stopped, the instance boots again
// on its own disk under a shim of this build.
type staleMachine struct {
	*shimProcess
	err error
}

func (m *staleMachine) Exec(context.Context, machine.ExecRequest) (machine.Process, error) {
	return nil, m.err
}

func (m *staleMachine) CopyTo(context.Context, string, io.Reader) error { return m.err }

func (m *staleMachine) CopyFrom(context.Context, string) (io.ReadCloser, error) { return nil, m.err }

func (m *staleMachine) Info(context.Context) (machine.GuestInfo, error) {
	return machine.GuestInfo{}, m.err
}

func (m *staleMachine) Dial(context.Context, uint32) (net.Conn, error) { return nil, m.err }

// Shutdown asks the shim to power its machine off in order, forced off once
// ctx's deadline (or a minute) has passed, and returns once it has exited.
// A reboot cannot be asked of it.
func (m *staleMachine) Shutdown(ctx context.Context, reboot bool) error {
	if reboot {
		return m.err
	}
	timeout := time.Minute
	if deadline, ok := ctx.Deadline(); ok {
		timeout = time.Until(deadline)
	}
	return m.shim.stop(ctx, timeout)
}
