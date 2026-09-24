//go:build darwin && cgo

package vz

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"path/filepath"
	"reflect"
	"runtime"
	"strings"
	"sync"
	"time"
	"unsafe"

	"github.com/Code-Hex/vz/v3"
	"golang.org/x/sys/unix"

	"github.com/discobox-ai/vm/pkg/guest"
	"github.com/discobox-ai/vm/pkg/machine"
)

// Check confirms the host can run macOS guests: Apple silicon, and a binary
// signed with the virtualization entitlement.
func (*Driver) Check(context.Context) error {
	if runtime.GOARCH != "arm64" {
		return errors.New("vz: macOS guests need an Apple silicon Mac")
	}
	// A macOS guest has no hypervisor: the framework nests only Linux guests,
	// so disco-vm inside a macOS VM cannot run one.
	if hv, err := unix.SysctlUint32("kern.hv_support"); err != nil || hv != 1 {
		return errors.New("vz: this Mac has no hypervisor (kern.hv_support is not 1); inside a macOS VM there is none to nest")
	}
	if !entitled() {
		return fmt.Errorf("vz: this binary is not signed with the %s entitlement, so it cannot create a VM; build it with `make build`, which signs it", virtualizationEntitlement)
	}
	return nil
}

// Prepare clones the parent layer's bundle into the instance, with its own
// machine identifier (and a MAC address on its first boot). A resume clone
// only checks that the stage has a template and marks the instance: its first
// boot takes a template that no running VM has, which only the process that
// will run the VM can hold.
func (*Driver) Prepare(_ context.Context, inst machine.InstanceSpec) error {
	if len(inst.Chain) == 0 {
		return errors.New("vz: instance has no image")
	}
	switch inst.Mode {
	case "", machine.Cold:
		return coldClone(inst)
	case machine.Resume:
		if ts, err := templates(inst.WarmDir, true); err != nil || len(ts) == 0 {
			return fmt.Errorf("vz: %w", machine.ErrNotWarm)
		}
		if err := os.MkdirAll(inst.Dir, 0o700); err != nil {
			return err
		}
		return os.WriteFile(filepath.Join(inst.Dir, pendingName), nil, 0o600)
	default:
		return fmt.Errorf("vz: clone mode %q: %w", inst.Mode, machine.ErrUnsupported)
	}
}

// stageBase is what a resume clone boots cold from when it cannot resume: the
// stage's base, which has the stage's user, or else the image.
func stageBase(inst machine.InstanceSpec) bundle {
	if inst.WarmDir != "" {
		if base := bundle(filepath.Join(inst.WarmDir, "base")); base.installedLayer() == nil {
			return base
		}
	}
	return bundle(inst.Parent().Dir)
}

// coldClone clones the image into the instance with an identity of its own.
func coldClone(inst machine.InstanceSpec) error {
	dst := bundle(inst.Dir)
	if err := cloneLayer(bundle(inst.Parent().Dir), dst); err != nil {
		return err
	}
	return dst.newIdentifier()
}

// Boot starts a prepared instance. A resume clone's first boot restores a
// template, and falls back to a cold boot of the image when none is free or
// the framework refuses the restore (a locked screen, a host update). Every
// other boot is cold.
func (*Driver) Boot(_ context.Context, inst machine.InstanceSpec, opts machine.BootOptions) (machine.Machine, error) {
	b := bundle(inst.Dir)
	console := opts.Console
	if console == nil {
		console = io.Discard
	}
	if _, err := os.Stat(b.path(pendingName)); err == nil {
		vm, err := resumeTemplate(inst, opts)
		if err == nil {
			_ = os.Remove(b.path(pendingName))
			fmt.Fprintln(console, "vz: resumed a template")
			return vm, vm.display(opts)
		}
		src := stageBase(inst)
		fmt.Fprintf(console, "%v; booting a cold clone of %s instead\n", err, src)
		for _, name := range templateFiles {
			_ = os.Remove(b.path(name))
		}
		if err := cloneLayer(src, b); err != nil {
			return nil, err
		}
		if err := b.newIdentifier(); err != nil {
			return nil, err
		}
		_ = os.Remove(b.path(pendingName))
	}
	if err := b.installed(); err != nil {
		return nil, err
	}
	m, err := b.readMeta()
	if err != nil {
		return nil, err
	}
	if m.Template != "" {
		// Restored from a template, so still its identity, which the
		// template's next clone will run with too.
		if err := b.newIdentifier(); err != nil {
			return nil, err
		}
		_ = os.Remove(b.path(macName))
		m.Template = ""
		if err := b.writeMeta(m); err != nil {
			return nil, err
		}
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

// resumeTemplate locks a free template, clones it into the instance, and
// restores it. The lock is held until the VM stops.
func resumeTemplate(inst machine.InstanceSpec, opts machine.BootOptions) (*vmMachine, error) {
	ts, err := templates(inst.WarmDir, false)
	if err != nil {
		return nil, err
	}
	b := bundle(inst.Dir)
	for _, t := range ts {
		lock, ok := lockTemplate(t)
		if !ok {
			continue
		}
		if err := cloneTemplate(t, b); err != nil {
			lock.Close()
			return nil, err
		}
		m, err := b.readMeta()
		if err != nil {
			lock.Close()
			return nil, err
		}
		vm, err := resume(b, m, opts.Shares)
		// The state is the template's; the clone's copy is done with.
		_ = os.Remove(b.path(stateName))
		if err != nil {
			lock.Close()
			return nil, err
		}
		m.Saved, m.Template = nil, filepath.Base(string(t))
		if err := b.writeMeta(m); err != nil {
			lock.Close()
			_ = vm.Kill(context.Background())
			return nil, err
		}
		go func() {
			<-vm.done
			lock.Close()
		}()
		return vm, nil
	}
	return nil, errors.New("vz: every template is running in another VM")
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
		return vsockConn{r.conn}, nil
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

var _ machine.HostListener = (*vmMachine)(nil)

// Listen accepts what guest processes open to the host (CID 2) on a vsock
// port.
func (m *vmMachine) Listen(port uint32) (net.Listener, error) {
	if m.socket == nil {
		return nil, errors.New("vz: the guest has no socket device")
	}
	l, err := m.socket.Listen(port)
	if err != nil {
		return nil, fmt.Errorf("vz: listen for the guest on port %d: %w", port, err)
	}
	return hostListener{l}, nil
}

// hostListener hands out connections that can half-close.
type hostListener struct{ *vz.VirtioSocketListener }

func (l hostListener) Accept() (net.Conn, error) {
	c, err := l.AcceptVirtioSocketConnection()
	if err != nil {
		return nil, err
	}
	return vsockConn{c}, nil
}

// vsockConn is a framework vsock connection that can close its sending half,
// so the other end reads EOF while this one still reads: a request piped in
// and its answer read back needs it. The bindings keep the connection's
// socket unexported (a net.Conn made from the framework's Unix-domain
// descriptor), so it is found by reflection; if their layout changes,
// CloseWrite closes the whole connection instead.
type vsockConn struct{ *vz.VirtioSocketConnection }

func (c vsockConn) CloseWrite() error {
	field := reflect.ValueOf(c.VirtioSocketConnection).Elem().FieldByName("rawConn")
	if field.IsValid() && field.Kind() == reflect.Interface {
		raw := reflect.NewAt(field.Type(), unsafe.Pointer(field.UnsafeAddr())).Elem().Interface()
		if cw, ok := raw.(interface{ CloseWrite() error }); ok {
			return cw.CloseWrite()
		}
	}
	return c.Close()
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
