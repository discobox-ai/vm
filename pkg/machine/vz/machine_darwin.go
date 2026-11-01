//go:build darwin && cgo

package vz

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
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
	"github.com/discobox-ai/vm/pkg/machine/local"
)

// Check confirms the host can run macOS guests: Apple silicon, and a binary
// signed with the virtualization entitlement.
func (*Hypervisor) Check(context.Context) error {
	if runtime.GOARCH != "arm64" {
		return errors.New("vz: macOS guests need an Apple silicon Mac")
	}
	// A macOS guest has no hypervisor: the framework nests only Linux guests,
	// so disco-vm inside a macOS VM cannot run one.
	if hv, err := unix.SysctlUint32("kern.hv_support"); err != nil || hv != 1 {
		return errors.New("vz: this Mac has no hypervisor (kern.hv_support is not 1); inside a macOS VM there is none to nest")
	}
	if !entitled() {
		return fmt.Errorf("vz: this binary is not signed with the %s entitlement, so it cannot create a VM; build it with `go tool task build`, which signs it", virtualizationEntitlement)
	}
	return nil
}

// Prepare clones the parent layer's bundle into the instance, with its own
// machine identity: a macOS guest's machine identifier (and a MAC address on
// its first boot), a Linux guest's machine ID and MAC address. An instance of
// a base is a Linux guest made from its image, pulled into the cache first.
func (*Hypervisor) Prepare(ctx context.Context, inst local.InstanceSpec) error {
	if inst.Base != nil {
		return prepareLinux(ctx, inst)
	}
	if len(inst.Chain) == 0 {
		return errors.New("vz: instance has no image")
	}
	src, dst := bundle(inst.Parent().Dir), bundle(inst.Dir)
	m, err := src.readMeta()
	if err != nil {
		return err
	}
	if err := cloneLayer(src, dst, m.guestOS()); err != nil {
		return err
	}
	if m.guestOS() == machine.Linux {
		return dst.newLinuxIdentity()
	}
	return dst.newIdentifier()
}

// pendingName marked a resume clone that had not had its first boot, from
// before resume clones were removed (ADR 26-10-15-851). Such an instance has
// no disk to boot.
const pendingName = "resume.pending"

// Boot starts a prepared instance from power-off and waits for its agent. A
// Linux guest's agent is the one this boot handed it, and one that speaks
// another protocol is killed and refused.
func (*Hypervisor) Boot(ctx context.Context, inst local.InstanceSpec, opts local.BootOptions) (machine.Machine, error) {
	m, err := bundle(inst.Dir).readMeta()
	if err != nil {
		return nil, err
	}
	var vm *vmMachine
	if m.guestOS() == machine.Linux {
		vm, err = bootLinux(inst, m, opts)
	} else {
		vm, err = bootInstance(inst, opts)
	}
	if err != nil {
		return nil, err
	}
	if err := vm.WaitReady(ctx, vm.done); err != nil {
		_ = vm.Kill(context.Background())
		return nil, err
	}
	if m.guestOS() == machine.Linux {
		if err := guest.CheckProtocol(ctx, vm); err != nil {
			_ = vm.Kill(context.Background())
			return nil, err
		}
	}
	return vm, nil
}

// fenced reports that the instance was created fenced to nothing beyond its
// networks, which vz holds as no network device.
func fenced(inst local.InstanceSpec) bool { return inst.Egress != nil && len(inst.Egress.Allow) == 0 }

func bootInstance(inst local.InstanceSpec, opts local.BootOptions) (*vmMachine, error) {
	b := bundle(inst.Dir)
	console := opts.Console
	if console == nil {
		console = io.Discard
	}
	if _, err := os.Stat(b.path(pendingName)); err == nil {
		return nil, fmt.Errorf("vz: instance %s was prepared as a resume clone and never booted, and resume clones are gone; remove it and run its image again", inst.ID)
	}
	if err := b.installed(machine.Darwin); err != nil {
		return nil, err
	}
	m, err := b.readMeta()
	if err != nil {
		return nil, err
	}
	if m.Template != "" {
		// A resume clone from before they were removed, restored from a
		// template, so still the template's identity.
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
	vm, err := start(b, cpus, memory, opts.Shares, !fenced(inst), nil)
	if err != nil {
		return nil, err
	}
	fmt.Fprintf(console, "vz: booted with %d CPUs and %d MiB\n", cpus, memory>>20)
	return vm, vm.display(opts)
}

// display opens a window on the guest's screen when the boot asks for one,
// and closes it when the guest stops. A boot that asked for a window and
// cannot have one fails, and takes its VM down with it.
func (m *vmMachine) display(opts local.BootOptions) error {
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

// start boots a macOS bundle from power-off, with a network device when nic,
// provisioning its first account if asked.
func start(b bundle, cpus uint, memory uint64, shares []machine.Share, nic bool, provision *vz.MacGuestProvisioningOptions) (*vmMachine, error) {
	var options []vz.VirtualMachineStartOption
	if provision != nil {
		options = append(options, vz.WithMacGuestProvisioning(*provision))
	}
	vm, err := unlocked(b, cpus, memory, shares, nic, func(vm *vz.VirtualMachine) error { return vm.Start(options...) })
	if err != nil {
		return nil, entitlementHint(fmt.Errorf("vz: start: %w", err))
	}
	return watch(vm), nil
}

// unlocked creates a VM for the bundle and starts it, retrying
// while the bundle's auxiliary storage is locked. A VM holds that lock until
// the VM object is freed, not merely stopped, and the bindings free it from a
// finalizer, so a bundle booted again by the process that last booted it (the
// install's provisioning boot, a build's reboot step) waits for a garbage
// collection. A VM whose start failed is not started again: it would stop at
// once in the error state. Each attempt gets a new one.
func unlocked(b bundle, cpus uint, memory uint64, shares []machine.Share, nic bool, run func(*vz.VirtualMachine) error) (*vz.VirtualMachine, error) {
	deadline := time.Now().Add(30 * time.Second)
	for {
		vm, err := newVM(b, cpus, memory, shares, nic)
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

func newVM(b bundle, cpus uint, memory uint64, shares []machine.Share, nic bool) (*vz.VirtualMachine, error) {
	config, err := b.configuration(cpus, memory, shares, nic)
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
	// Client is the guest agent over vsock: the machine's guest operations.
	*guest.Client
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
	m.Client = guest.NewClient(func(ctx context.Context) (net.Conn, error) { return m.Dial(ctx, guest.AgentPort) })
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

var (
	_ machine.HostListener = (*vmMachine)(nil)
	_ machine.Machine      = (*vmMachine)(nil)
)

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
// stop request by sleeping, so an orderly shutdown goes through the agent
// (Shutdown) and this is only the fallback.
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
