//go:build darwin && cgo

package vz

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"runtime"
	"strings"
	"sync"
	"time"

	"github.com/Code-Hex/vz/v3"

	"github.com/discobox-ai/vm/pkg/guest"
	"github.com/discobox-ai/vm/pkg/machine"
)

// Check confirms the host can run macOS guests: Apple silicon, and a binary
// signed with the virtualization entitlement.
func (*Driver) Check(context.Context) error {
	if runtime.GOARCH != "arm64" {
		return errors.New("vz: macOS guests need an Apple silicon Mac")
	}
	if !entitled() {
		return fmt.Errorf("vz: this binary is not signed with the %s entitlement, so it cannot create a VM; build it with `make build`, which signs it", virtualizationEntitlement)
	}
	return nil
}

// Prepare clones the parent layer's bundle into the instance. A cold clone
// gets its own machine identifier (and a MAC address on its first boot). A
// resume clone takes a whole staged bundle, identity and disk included.
func (*Driver) Prepare(_ context.Context, inst machine.InstanceSpec) error {
	if len(inst.Chain) == 0 {
		return errors.New("vz: instance has no image")
	}
	switch inst.Mode {
	case "", machine.Cold:
		dst := bundle(inst.Dir)
		if err := cloneLayer(bundle(inst.Parent().Dir), dst); err != nil {
			return err
		}
		return dst.newIdentifier()
	case machine.Resume:
		return takeState(inst.WarmDir, inst.Dir)
	default:
		return fmt.Errorf("vz: clone mode %q: %w", inst.Mode, machine.ErrUnsupported)
	}
}

// Boot starts a prepared instance. The first boot of a resume clone restores
// its saved state and consumes it; a state the framework rejects (a locked
// screen, a host update) falls back to a cold boot rather than failing the
// run. Every other boot is cold.
func (*Driver) Boot(_ context.Context, inst machine.InstanceSpec, opts machine.BootOptions) (machine.Machine, error) {
	b := bundle(inst.Dir)
	if err := b.installed(); err != nil {
		return nil, err
	}
	m, err := b.readMeta()
	if err != nil {
		return nil, err
	}
	console := opts.Console
	if console == nil {
		console = io.Discard
	}
	if _, err := os.Stat(b.path(stateName)); err == nil {
		vm, err := resume(b, m, opts.Shares)
		// Consumed either way: a later boot of this instance is cold.
		_ = os.Remove(b.path(stateName))
		m.Saved = nil
		if werr := b.writeMeta(m); werr != nil && err == nil {
			_ = vm.Kill(context.Background())
			return nil, werr
		}
		if err == nil {
			fmt.Fprintln(console, "vz: resumed a saved state")
			return vm, vm.display(opts)
		}
		fmt.Fprintf(console, "%v; booting cold instead\n", err)
	}
	cpus, memory := size(m, opts.CPUs, opts.Memory)
	vm, err := start(b, cpus, memory, opts.Shares, nil)
	if err != nil {
		return nil, err
	}
	fmt.Fprintf(console, "vz: booted with %d CPUs and %d MiB\n", cpus, memory>>20)
	return vm, vm.display(opts)
}

// display opens a window on the guest's screen when the boot asks for one,
// and closes it when the guest stops. A boot that asked for a window and
// cannot have one fails, and takes its VM down with it.
func (m *vmMachine) display(opts machine.BootOptions) error {
	if !opts.GUI {
		return nil
	}
	title := opts.Title
	if title == "" {
		title = "disco-vm"
	}
	closeWindow, err := openWindow(m.vm, title)
	if err != nil {
		_ = m.Kill(context.Background())
		return err
	}
	go func() {
		<-m.done
		closeWindow()
	}()
	return nil
}

// start boots a bundle from power-off, provisioning its first account if asked.
func start(b bundle, cpus uint, memory uint64, shares []machine.Share, provision *vz.MacGuestProvisioningOptions) (*vmMachine, error) {
	var options []vz.VirtualMachineStartOption
	if provision != nil {
		options = append(options, vz.WithMacGuestProvisioning(*provision))
	}
	vm, err := unlocked(b, cpus, memory, shares, func(vm *vz.VirtualMachine) error { return vm.Start(options...) })
	if err != nil {
		return nil, entitlementHint(fmt.Errorf("vz: start: %w", err))
	}
	return watch(vm), nil
}

// resume restores a bundle's saved state at the size it was saved at, which
// a restore needs exactly.
func resume(b bundle, m meta, shares []machine.Share) (*vmMachine, error) {
	if m.Saved == nil {
		return nil, errors.New("vz: the saved state has no record of its size")
	}
	// A restore takes a stopped VM to paused, holding the memory the guest
	// had when it was saved; resuming is what makes it run.
	vm, err := unlocked(b, m.Saved.CPUs, m.Saved.Memory, shares, func(vm *vz.VirtualMachine) error {
		return vm.RestoreMachineStateFromURL(b.path(stateName))
	})
	if err != nil {
		return nil, fmt.Errorf("vz: restore (\"permission denied\" means the Mac's screen is locked; \"invalid argument\" means the configuration differs from the saved one; a host OS update can invalidate a state too): %w", err)
	}
	if err := vm.Resume(); err != nil {
		if vm.CanStop() {
			_ = vm.Stop()
		}
		return nil, fmt.Errorf("vz: resume: %w", err)
	}
	return watch(vm), nil
}

// unlocked creates a VM for the bundle and starts or restores it, retrying
// while the bundle's auxiliary storage is locked. A VM holds that lock until
// the VM object is freed, not merely stopped, and the bindings free it from a
// finalizer, so a bundle booted again by the process that last booted it (the
// install's provisioning boot, a build's reboot step) waits for a garbage
// collection. A VM whose start failed is not started again: it would stop at
// once in the error state. Each attempt gets a new one.
func unlocked(b bundle, cpus uint, memory uint64, shares []machine.Share, run func(*vz.VirtualMachine) error) (*vz.VirtualMachine, error) {
	deadline := time.Now().Add(30 * time.Second)
	for {
		vm, err := newVM(b, cpus, memory, shares)
		if err != nil {
			return nil, err
		}
		err = run(vm)
		if err == nil {
			return vm, nil
		}
		if !strings.Contains(err.Error(), "lock auxiliary storage") || time.Now().After(deadline) {
			return nil, err
		}
		runtime.GC()
		time.Sleep(250 * time.Millisecond)
	}
}

func newVM(b bundle, cpus uint, memory uint64, shares []machine.Share) (*vz.VirtualMachine, error) {
	config, err := b.configuration(cpus, memory, shares)
	if err != nil {
		return nil, err
	}
	vm, err := vz.NewVirtualMachine(config)
	if err != nil {
		return nil, entitlementHint(fmt.Errorf("vz: create virtual machine: %w", err))
	}
	return vm, nil
}

// vmMachine is a running guest. It lives in the process that created it, which
// is always a shim or the builder: a Virtualization.framework VM dies with its
// process.
type vmMachine struct {
	vm     *vz.VirtualMachine
	socket *vz.VirtioSocketDevice
	done   chan struct{}
	err    error
	kill   sync.Once
}

// watch follows a started VM's state until it stops. The framework queues
// state changes from the VM's creation on, so none is missed; a started VM's
// queue holds only starting and running states, and the first stopped or
// error state is the end.
func watch(vm *vz.VirtualMachine) *vmMachine {
	m := &vmMachine{vm: vm, done: make(chan struct{})}
	if devices := vm.SocketDevices(); len(devices) > 0 {
		m.socket = devices[0]
	}
	go func() {
		defer close(m.done)
		for state := range vm.StateChangedNotify() {
			switch state {
			case vz.VirtualMachineStateStopped:
				return
			case vz.VirtualMachineStateError:
				m.err = errors.New("vz: the virtual machine stopped with an error")
				return
			}
		}
	}()
	return m
}

// Dial connects to a guest port over vsock. The framework's connect does not
// take a deadline, so the caller's is enforced here, and a connection that
// arrives after the caller gave up is closed.
func (m *vmMachine) Dial(ctx context.Context, port uint32) (net.Conn, error) {
	select {
	case <-m.done:
		return nil, errors.New("vz: the guest is stopped")
	default:
	}
	if m.socket == nil {
		return nil, errors.New("vz: the guest has no socket device")
	}
	type result struct {
		conn *vz.VirtioSocketConnection
		err  error
	}
	ch := make(chan result, 1)
	go func() {
		conn, err := m.socket.Connect(port)
		ch <- result{conn, err}
	}()
	select {
	case r := <-ch:
		if r.err != nil {
			return nil, fmt.Errorf("vz: connect to guest port %d: %w", port, r.err)
		}
		return r.conn, nil
	case <-ctx.Done():
		go func() {
			if r := <-ch; r.conn != nil {
				_ = r.conn.Close()
			}
		}()
		return nil, ctx.Err()
	case <-m.done:
		return nil, errors.New("vz: the guest stopped")
	}
}

// Kill powers the guest off at once. A macOS guest answers the framework's
// stop request by sleeping, so an orderly shutdown goes through the agent and
// this is only the fallback.
func (m *vmMachine) Kill(context.Context) error {
	var err error
	m.kill.Do(func() {
		if m.vm.CanStop() {
			err = m.vm.Stop()
		}
	})
	if err != nil {
		return fmt.Errorf("vz: stop: %w", err)
	}
	<-m.done
	return nil
}

func (m *vmMachine) Done() <-chan struct{} { return m.done }

func (m *vmMachine) Err() error {
	<-m.done
	return m.err
}

// agent is a client for the guest agent on this machine.
func (m *vmMachine) agent() *guest.Client {
	return guest.NewClient(func(ctx context.Context) (net.Conn, error) { return m.Dial(ctx, guest.AgentPort) })
}
