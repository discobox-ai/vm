package hcs

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"path/filepath"
	"sync"
	"time"

	"github.com/Microsoft/go-winio"
	"github.com/Microsoft/go-winio/pkg/guid"

	"github.com/discobox-ai/vm/internal/fsutil"
	"github.com/discobox-ai/vm/internal/proc"
	"github.com/discobox-ai/vm/pkg/guest"
	"github.com/discobox-ai/vm/pkg/machine"
	"github.com/discobox-ai/vm/pkg/machine/local"
)

// An install's window. Setup has no desktop until its automatic logon, so a
// window on an install is the agent's display stream from then on, through
// the Zero Day Patch reboot, to the shutdown that commits it: never the
// guest's Remote Desktop, whose sign-in would take the console session from
// the logon Setup is finishing.
//
// The installing process shows it with `install --gui`. Any other process
// reaches the install's VM by the record firstBoot writes beside the
// install's breadcrumb, and dials its guest over hvsocket: it never opens the
// compute system, whose last handle closing is what ends the VM.

// installVMFile records the install's running VM in the image's directory
// while its first boot runs.
const installVMFile = ".install-vm"

// installRecord is the installing process and its VM's runtime ID.
type installRecord struct {
	PID       int
	RuntimeID string
}

// installTitle names an install's window.
const installTitle = "disco-vm: install"

// recordInstall writes the install's record in dir, and returns what removes
// it again.
func recordInstall(dir string, m *vm) (func(), error) {
	path := filepath.Join(dir, installVMFile)
	if err := fsutil.WriteJSON(path, installRecord{PID: os.Getpid(), RuntimeID: m.runtimeID.String()}); err != nil {
		return nil, err
	}
	return func() { _ = os.Remove(path) }, nil
}

// showInstall keeps a window open on the install's guest desktop from its
// first logon, until someone closes it or the VM stops.
func showInstall(ctx context.Context, m machine.Machine, log io.Writer) {
	keepOpen(ctx, m, log, func(ctx context.Context) error { return viewDisplay(ctx, dialDisplay(m), installTitle) })
}

// ViewInstall opens a window on the install in progress into dst, from its
// first logon, and returns once someone closes it, ctx is done, or the
// install's VM stops. An install whose first boot is not running is
// machine.ErrNotRunning.
func (*Hypervisor) ViewInstall(ctx context.Context, dst local.Layer, title string, log io.Writer) error {
	m, err := openInstall(dst.Dir)
	if err != nil {
		return err
	}
	keepOpen(ctx, m, log, func(ctx context.Context) error { return viewDisplay(ctx, dialDisplay(m), title) })
	return ctx.Err()
}

// openInstall is the install in dir's running VM, as another process reaches
// it.
func openInstall(dir string) (*installVM, error) {
	path := filepath.Join(dir, installVMFile)
	var rec installRecord
	if err := fsutil.ReadJSON(path, &rec); err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil, fmt.Errorf("the install has not booted its VM yet, or has finished with it: %w", machine.ErrNotRunning)
		}
		return nil, err
	}
	if !proc.Alive(rec.PID) {
		return nil, fmt.Errorf("the installing process (pid %d) is gone: %w", rec.PID, machine.ErrNotRunning)
	}
	id, err := guid.FromString(rec.RuntimeID)
	if err != nil {
		return nil, fmt.Errorf("hcs: the install's record %s: %w", path, err)
	}
	m := &installVM{record: path, pid: rec.PID, runtimeID: id, done: make(chan struct{})}
	m.Client = guest.NewClient(func(ctx context.Context) (net.Conn, error) { return m.Dial(ctx, guest.AgentPort) })
	return m, nil
}

// installVM is an install's VM, which another process holds: reached over
// hvsocket by its runtime ID, and stopped, as far as this process can tell,
// once the installing process is gone or has removed its record.
type installVM struct {
	*guest.Client
	record    string
	pid       int
	runtimeID guid.GUID
	done      chan struct{}
	watching  sync.Once
}

var _ machine.Machine = (*installVM)(nil)

func (m *installVM) Dial(ctx context.Context, port uint32) (net.Conn, error) {
	return winio.Dial(ctx, &winio.HvsockAddr{VMID: m.runtimeID, ServiceID: winio.VsockServiceID(port)})
}

// Kill is refused: the VM is the installing process's to stop.
func (*installVM) Kill(context.Context) error {
	return errors.New("hcs: an install's VM is stopped by the install, or by interrupting it")
}

func (m *installVM) Done() <-chan struct{} {
	m.watching.Do(func() {
		go func() {
			for {
				if _, err := os.Stat(m.record); err != nil || !proc.Alive(m.pid) {
					close(m.done)
					return
				}
				time.Sleep(time.Second)
			}
		}()
	})
	return m.done
}

// Err is nil: how the install went is the installing process's to say.
func (m *installVM) Err() error {
	<-m.Done()
	return nil
}
