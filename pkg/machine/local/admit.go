package local

import (
	"context"
	"errors"
	"fmt"
	"net"
	"path/filepath"
	"strconv"

	"github.com/discobox-ai/vm/internal/fsutil"
	"github.com/discobox-ai/vm/pkg/machine"
)

// bootLockName is the file in an instance's directory whose lock the process
// holding its machine keeps, so that a VM boots once at a time.
const bootLockName = "boot.lock"

// slotsDir holds, per guest OS the hypervisor caps (Capabilities.MaxRunning),
// one file per running guest it allows, shared by every pool in the
// directory: running/<hypervisor>/<os>/<n>.
func (d *Driver) slotsDir(guestOS machine.OS) string {
	return filepath.Join(d.dir, "running", d.hv.Name(), string(guestOS))
}

// admit takes what a boot of rec holds for as long as its machine runs: the
// instance's boot lock, and, for a guest OS the hypervisor caps, a free one
// of its slots, refused with machine.ErrCapacity when every slot is held.
// Each is an operating system lock taken without waiting, so concurrent
// boots in any pool and any process are admitted one by one, a boot that has
// not finished holds its slot, and a process that dies gives back what it
// held. A lock that cannot be taken for any other reason refuses the boot.
// The returned func releases both.
func (d *Driver) admit(rec instanceRecord) (func(), error) {
	unlock, err := fsutil.TryLock(filepath.Join(d.instanceDir(rec.ID), bootLockName))
	if errors.Is(err, fsutil.ErrLocked) {
		return nil, fmt.Errorf("instance %s is already running", rec.Name)
	}
	if err != nil {
		return nil, err
	}
	limit := d.hv.Capabilities().MaxRunning[rec.GuestOS]
	if limit <= 0 {
		return unlock, nil
	}
	for n := range limit {
		release, err := fsutil.TryLock(filepath.Join(d.slotsDir(rec.GuestOS), strconv.Itoa(n)))
		if err == nil {
			return func() { release(); unlock() }, nil
		}
		if !errors.Is(err, fsutil.ErrLocked) {
			unlock()
			return nil, err
		}
	}
	unlock()
	return nil, fmt.Errorf("%s runs at most %d %s guests at once, in every pool, and all %d are running or booting: %w", d.hv.Name(), limit, rec.GuestOS, limit, machine.ErrCapacity)
}

// admittedMachine is a machine booted in this process, which holds its
// admission until it stops: its Done is closed, and its Kill returns, only
// once the admission is released, so a caller that saw one guest stop finds
// its slot free. Were the
// process to die first, the operating system would release it.
type admittedMachine struct {
	machine.Machine
	done chan struct{}
}

var _ machine.HostListener = (*admittedMachine)(nil)

func admitted(m machine.Machine, release func()) *admittedMachine {
	a := &admittedMachine{Machine: m, done: make(chan struct{})}
	go func() {
		<-m.Done()
		release()
		close(a.done)
	}()
	return a
}

func (a *admittedMachine) Done() <-chan struct{} { return a.done }

// Kill returns once the admission is released too, so a caller that killed
// one guest finds its slot free.
func (a *admittedMachine) Kill(ctx context.Context) error {
	if err := a.Machine.Kill(ctx); err != nil {
		return err
	}
	select {
	case <-a.done:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

// Listen is the machine's own, where it takes a guest's connections to the
// host.
func (a *admittedMachine) Listen(port uint32) (net.Listener, error) {
	hl, ok := a.Machine.(machine.HostListener)
	if !ok {
		return nil, fmt.Errorf("this machine cannot take a guest's connections to the host: %w", machine.ErrUnsupported)
	}
	return hl.Listen(port)
}
