//go:build linux

package libkrun

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"slices"
	"strconv"
	"strings"
	"sync"
	"syscall"

	"golang.org/x/sys/unix"

	"github.com/discobox-ai/vm/pkg/guest"
	"github.com/discobox-ai/vm/pkg/machine"
	"github.com/discobox-ai/vm/pkg/machine/linuximage"
	"github.com/discobox-ai/vm/pkg/machine/local"
)

// maxSocketPath is the longest path a Unix socket may have on Linux, less its
// NUL.
const maxSocketPath = 107

// launch is one boot's launcher configuration (launch.conf; the format is
// described in runtime/launch.c).
type launch struct {
	cpus    int
	memory  uint64
	kernel  string
	initrd  string
	cmdline string
	console string
	disk    string
	// in maps a guest port to the Unix socket libkrun listens on for it;
	// out, a guest port the guest connects to the host on to the socket the
	// shim listens on.
	in, out map[uint32]string
	// netFD is the launcher's descriptor of its end of passt's stream, or
	// zero for no network device.
	netFD  int
	mac    net.HardwareAddr
	shares []machine.Share
}

// render is the configuration as launch.c reads it, or an error for a value
// it cannot carry: a field may hold no space but the last, and no field may
// hold a newline.
func (l launch) render() ([]byte, error) {
	var b bytes.Buffer
	line := func(fields ...string) error {
		for i, f := range fields {
			if f == "" || strings.ContainsAny(f, "\n\x00") || (i < len(fields)-1 && strings.Contains(f, " ")) {
				return fmt.Errorf("libkrun: %q cannot be written to the launcher's configuration", f)
			}
		}
		b.WriteString(strings.Join(fields, " "))
		b.WriteByte('\n')
		return nil
	}
	errs := []error{
		line("krun-launch", launchVersion),
		line("cpus", strconv.Itoa(l.cpus)),
		line("memory", strconv.FormatUint(l.memory>>20, 10)),
		line("kernel", l.kernel),
		line("initrd", l.initrd),
		line("cmdline", l.cmdline),
		line("console", l.console),
		line("disk", "vda", "rw", l.disk),
	}
	for _, dir := range []struct {
		key   string
		ports map[uint32]string
	}{{"vsock-in", l.in}, {"vsock-out", l.out}} {
		ports := make([]uint32, 0, len(dir.ports))
		for port := range dir.ports {
			ports = append(ports, port)
		}
		slices.Sort(ports)
		for _, port := range ports {
			errs = append(errs, line(dir.key, strconv.FormatUint(uint64(port), 10), dir.ports[port]))
		}
	}
	if l.netFD > 0 {
		errs = append(errs, line("net", strconv.Itoa(l.netFD), l.mac.String()))
	}
	for _, share := range l.shares {
		mode := "rw"
		if share.ReadOnly {
			mode = "ro"
		}
		errs = append(errs, line("share", share.Tag, mode, share.HostPath))
	}
	return b.Bytes(), errors.Join(errs...)
}

// agentBinary is the agent a boot appends to the initrd: the configured one
// (Config.Agent), or the running disco-vm, which on a Linux amd64 host is a
// static linux/amd64 executable (ADR 26-11-03-530 §6). The protocol check
// after boot refuses one of another disco-vm.
func (h *Hypervisor) agentBinary() (string, error) {
	agent := h.agent
	if agent == "" {
		exe, err := os.Executable()
		if err != nil {
			return "", fmt.Errorf("libkrun: the agent: %w", err)
		}
		agent = exe
	}
	if err := linuximage.CheckAgent(agent, runtime.GOARCH); err != nil {
		return "", fmt.Errorf("libkrun: %w: the agent is the running disco-vm, or the configured one ($%s), and must be a static linux build (`go tool task build`)", err, AgentEnv)
	}
	return agent, nil
}

// checkForwards refuses a forward on the agent's port or the service's:
// libkrun maps a guest port one way, so a forward of it would replace the
// mapping the host reaches the guest's agent or service by.
func checkForwards(forwards []machine.Forward) error {
	for _, f := range forwards {
		if f.Port == guest.AgentPort || f.Port == guest.ServicePort {
			return fmt.Errorf("libkrun: a forward of guest port %d, which the host reaches the guest's agent or service on: %w", f.Port, machine.ErrUnsupported)
		}
	}
	return nil
}

// fenced reports that the instance was created fenced to nothing beyond its
// networks, which libkrun holds as no network device.
func fenced(inst local.InstanceSpec) bool { return inst.Egress != nil && len(inst.Egress.Allow) == 0 }

// Boot starts a prepared instance from power-off and waits for its agent:
// the running disco-vm, a static linux/amd64 executable, appended to the
// image's initrd. It writes the launcher's configuration, starts passt
// unless the instance is fenced, and runs the launcher through the runtime's
// own loader. A launcher that exits before the agent answers is refused with
// its standard error and the tail of the guest's console; an agent that
// speaks another protocol is killed and refused.
func (h *Hypervisor) Boot(ctx context.Context, inst local.InstanceSpec, opts local.BootOptions) (machine.Machine, error) {
	if opts.GUI {
		return nil, fmt.Errorf("libkrun: a guest has no display to show: %w", machine.ErrUnsupported)
	}
	for _, share := range opts.Shares {
		if share.Tag == "" {
			return nil, fmt.Errorf("libkrun: the share of %s needs a tag to mount it by", share.HostPath)
		}
	}
	if err := checkHost(); err != nil {
		return nil, err
	}
	b := bundle(inst.Dir)
	if err := b.prepared(); err != nil {
		return nil, err
	}
	m, err := b.readMeta()
	if err != nil {
		return nil, err
	}
	rt, err := h.runtime(ctx, inst.CacheDir, false)
	if err != nil {
		return nil, err
	}
	agent, err := h.agentBinary()
	if err != nil {
		return nil, err
	}
	if err := linuximage.WriteBootInitrd(b.path(bootInitrdName), b.path(initrdName), agent); err != nil {
		return nil, fmt.Errorf("libkrun: the boot's initrd: %w", err)
	}
	id, mac, err := b.identity()
	if err != nil {
		return nil, err
	}
	console := opts.Console
	if console == nil {
		console = io.Discard
	}

	vm := &vmMachine{done: make(chan struct{}), in: map[uint32]string{}, listeners: map[uint32]net.Listener{}}
	ok := false
	defer func() {
		if !ok {
			vm.release()
		}
	}()
	if err := checkForwards(inst.Forwards); err != nil {
		return nil, err
	}
	if err := vm.sockets(b, inst.Forwards); err != nil {
		return nil, err
	}
	cpus, memory := m.CPUs, m.Memory
	if opts.CPUs > 0 {
		cpus = opts.CPUs
	}
	if opts.Memory > 0 {
		memory = opts.Memory
	}
	nic := !fenced(inst)
	l := launch{
		cpus: min(cpus, 255), memory: memory,
		kernel: b.path(kernelName), initrd: b.path(bootInitrdName),
		cmdline: linuximage.Cmdline(id, nic) + " reboot=k", console: b.path(consoleName), disk: b.path(diskName),
		in: vm.in, out: map[uint32]string{}, mac: mac, shares: opts.Shares,
	}
	for port, ln := range vm.listeners {
		l.out[port] = ln.Addr().String()
	}
	var extra []*os.File
	if nic {
		netFile, err := vm.startPasst(rt, console)
		if err != nil {
			return nil, err
		}
		defer netFile.Close()
		extra = append(extra, netFile)
		l.netFD = 3
	}
	conf, err := l.render()
	if err != nil {
		return nil, err
	}
	if err := os.WriteFile(b.path(launchName), conf, 0o600); err != nil {
		return nil, fmt.Errorf("libkrun: %w", err)
	}
	// libkrun truncates the console it writes, so the last boot's is kept
	// beside it.
	_ = os.Rename(b.path(consoleName), b.path(prevConsoleName))
	if err := vm.start(rt, b, extra, console); err != nil {
		return nil, err
	}
	ok = true
	network := "a network device on passt"
	if !nic {
		network = "no network device"
	}
	// Through the launcher's buffer, which writes the console under its lock.
	vm.stderr.note(fmt.Sprintf("libkrun: booted with %d CPUs, %d MiB, and %s\n", l.cpus, memory>>20, network))

	vm.Client = guest.NewClient(func(ctx context.Context) (net.Conn, error) { return vm.Dial(ctx, guest.AgentPort) })
	if err := vm.WaitReady(ctx, vm.done); err != nil {
		_ = vm.Kill(context.Background())
		return nil, vm.bootError(b, err)
	}
	if err := guest.CheckProtocol(ctx, vm); err != nil {
		_ = vm.Kill(context.Background())
		return nil, err
	}
	return vm, nil
}

// vmMachine is a running guest: its launcher, a child of the shim that
// booted it, and passt beside it.
type vmMachine struct {
	// Client is the guest agent over vsock: the machine's guest operations.
	*guest.Client
	launcher *child
	passt    *child
	// in is each guest port a caller dials, to the Unix socket libkrun
	// listens on for it.
	in map[uint32]string
	// listeners are the forwards' sockets, listened on before the launcher
	// starts so the guest never connects to nothing; Listen hands each out.
	listeners map[uint32]net.Listener
	// tmp is the directory holding the sockets when the instance's own is
	// too deep for one, removed once the machine stops.
	tmp    string
	stderr *tailBuffer
	done   chan struct{}
	err    error
	mu     sync.Mutex
	killed bool
	// released is set once the machine let go of passt, and passtErr is
	// why passt exited before that.
	released bool
	passtErr error
}

var (
	_ machine.Machine      = (*vmMachine)(nil)
	_ machine.HostListener = (*vmMachine)(nil)
)

// sockets picks where the boot's vsock sockets go, maps the agent's port and
// the service's into the guest (ADR 26-11-03-247), and listens on one socket
// per forward.
func (vm *vmMachine) sockets(b bundle, forwards []machine.Forward) error {
	dir := string(b)
	if len(filepath.Join(dir, "vsock-out-4294967295.sock")) > maxSocketPath {
		tmp, err := os.MkdirTemp("", "dvm-krun-")
		if err != nil {
			return err
		}
		vm.tmp, dir = tmp, tmp
	}
	for _, port := range []uint32{guest.AgentPort, guest.ServicePort} {
		path := filepath.Join(dir, fmt.Sprintf("vsock-in-%d.sock", port))
		_ = os.Remove(path)
		vm.in[port] = path
	}
	for _, f := range forwards {
		path := filepath.Join(dir, fmt.Sprintf("vsock-out-%d.sock", f.Port))
		_ = os.Remove(path)
		l, err := (&net.ListenConfig{}).Listen(context.Background(), "unix", path)
		if err != nil {
			return fmt.Errorf("libkrun: forward port %d: %w", f.Port, err)
		}
		if err := os.Chmod(path, 0o600); err != nil {
			_ = l.Close()
			return err
		}
		vm.listeners[f.Port] = l
	}
	return nil
}

// passtArgs is how passt runs for a VM, on the launcher's stream at
// descriptor 3. It maps no address to the host's loopback, so a service the
// host binds there is out of the guest's reach, and forwards no port into
// the guest, which is reached only over vsock (discobox ADR 26-11-11-963
// §11), with the flags discobox's own libkrun provider gives it (--no-map-gw
// says again what --map-host-loopback none does). A host resolver on the
// loopback is then out of reach too, and passt hands the guest no DNS server.
func passtArgs() []string {
	return []string{
		"--fd", "3", "--foreground", "--quiet",
		"--map-host-loopback", "none", "--no-map-gw",
		"--tcp-ports", "none", "--udp-ports", "none",
	}
}

// startPasst starts passt on one end of a connected Unix stream and returns
// the other, for the launcher. passt serves the guest DHCP and DNS (passtArgs),
// and dies with the shim, or when the launcher has exited. Its standard error
// goes to the console, and so does its exit while the VM runs: the guest's
// network is gone then, and nothing else would say why.
func (vm *vmMachine) startPasst(rt krunRuntime, console io.Writer) (*os.File, error) {
	fds, err := unix.Socketpair(unix.AF_UNIX, unix.SOCK_STREAM|unix.SOCK_CLOEXEC, 0)
	if err != nil {
		return nil, fmt.Errorf("libkrun: passt's socket: %w", err)
	}
	ours, theirs := os.NewFile(uintptr(fds[0]), "passt"), os.NewFile(uintptr(fds[1]), "launcher")
	defer ours.Close()
	// Not bound to a context: passt lives as long as the VM, not the boot.
	cmd := exec.CommandContext(context.Background(), rt.path(passtName), passtArgs()...) //nolint:gosec // G204: the runtime's passt
	cmd.ExtraFiles = []*os.File{ours}
	out := &tailBuffer{tee: console}
	cmd.Stderr = out
	proc, err := startChild(cmd)
	if err != nil {
		_ = theirs.Close()
		return nil, fmt.Errorf("libkrun: start passt: %w", err)
	}
	vm.passt = proc
	go func() {
		<-proc.done
		vm.mu.Lock()
		released := vm.released
		if !released {
			vm.passtErr = fmt.Errorf("libkrun: passt exited (%s), so the guest has no network: %s", proc.state, tail(out.bytes(), 1024))
		}
		vm.mu.Unlock()
		if !released {
			out.note(vm.passtErr.Error() + "\n")
		}
	}()
	return theirs, nil
}

// start runs the launcher on the bundle's launch.conf through the runtime's
// loader, and follows it until it exits.
func (vm *vmMachine) start(rt krunRuntime, b bundle, extra []*os.File, console io.Writer) error {
	cmd := rt.command(context.Background(), b.path(launchName))
	cmd.Dir = string(b)
	cmd.ExtraFiles = extra
	vm.stderr = &tailBuffer{tee: console}
	cmd.Stdout = vm.stderr
	cmd.Stderr = vm.stderr
	cmd.Env = append(os.Environ(), "HOSTNAME=disco-vm")
	proc, err := startChild(cmd)
	if err != nil {
		return fmt.Errorf("libkrun: start the launcher: %w", err)
	}
	vm.launcher = proc
	go func() {
		<-proc.done
		vm.mu.Lock()
		killed := vm.killed
		vm.mu.Unlock()
		if proc.err != nil && !killed {
			vm.err = fmt.Errorf("libkrun: the launcher exited (%s): %s", proc.state, tail(vm.stderr.bytes(), 2048))
		}
		vm.release()
		close(vm.done)
	}()
	return nil
}

// child is a started process, reaped on the thread that forked it.
type child struct {
	proc  *os.Process
	done  chan struct{}
	state *os.ProcessState
	err   error
}

// startChild starts cmd as a child that dies with the shim
// (PR_SET_PDEATHSIG). The kernel sends that signal when the thread that
// forked the child exits, not the process, so the fork happens on a locked
// thread that also waits for the child, and so outlives it.
func startChild(cmd *exec.Cmd) (*child, error) {
	cmd.SysProcAttr = &syscall.SysProcAttr{Pdeathsig: syscall.SIGKILL}
	c := &child{done: make(chan struct{})}
	started := make(chan error, 1)
	go func() {
		// Never unlocked: the thread ends with this goroutine, once the
		// child has been reaped.
		runtime.LockOSThread()
		if err := cmd.Start(); err != nil {
			started <- err
			return
		}
		c.proc = cmd.Process
		started <- nil
		c.err = cmd.Wait()
		c.state = cmd.ProcessState
		close(c.done)
	}()
	if err := <-started; err != nil {
		return nil, err
	}
	return c, nil
}

// release stops passt, closes the forwards' listeners, and removes the
// boot's sockets.
func (vm *vmMachine) release() {
	vm.mu.Lock()
	vm.released = true
	vm.mu.Unlock()
	if vm.passt != nil {
		_ = vm.passt.proc.Kill()
		<-vm.passt.done
	}
	for _, l := range vm.listeners {
		_ = l.Close()
	}
	for _, path := range vm.in {
		_ = os.Remove(path)
	}
	if vm.tmp != "" {
		_ = os.RemoveAll(vm.tmp)
	}
}

// bootError is why a boot failed: the launcher's exit and its standard
// error, if it exited, and the tail of the guest's console.
func (vm *vmMachine) bootError(b bundle, err error) error {
	select {
	case <-vm.done:
		if vm.err != nil {
			err = vm.err
		}
	default:
	}
	vm.mu.Lock()
	if vm.passtErr != nil {
		err = fmt.Errorf("%w; %w", err, vm.passtErr)
	}
	vm.mu.Unlock()
	console, _ := os.ReadFile(b.path(consoleName))
	if t := tail(console, 4096); t != "" {
		return fmt.Errorf("%w\nthe guest's console (%s) ends:\n%s", err, b.path(consoleName), t)
	}
	return err
}

// Dial connects to the agent's port or the service's, through the Unix
// socket libkrun listens on for it. No other port was mapped at boot.
func (vm *vmMachine) Dial(ctx context.Context, port uint32) (net.Conn, error) {
	select {
	case <-vm.done:
		return nil, errors.New("libkrun: the guest is stopped")
	default:
	}
	path, ok := vm.in[port]
	if !ok {
		return nil, fmt.Errorf("libkrun: guest port %d was not mapped at boot: %w", port, machine.ErrUnsupported)
	}
	var d net.Dialer
	conn, err := d.DialContext(ctx, "unix", path)
	if err != nil {
		return nil, fmt.Errorf("libkrun: connect to guest port %d: %w", port, err)
	}
	return hostConn{conn}, nil
}

// hostConn is the host's end of a stream a caller dialed into the guest.
// libkrun (1.19) takes the host's half-close of a stream as its close and
// resets the guest's end, answer unread, so the host's CloseWrite does
// nothing: the guest never reads EOF from the caller, and the stream ends
// when the guest closes it, or when the caller closes this conn itself (a
// relay that saw only EOF from its own caller keeps it open until the guest
// writes or closes). The guest's half-close reaches the caller.
type hostConn struct{ net.Conn }

func (hostConn) CloseWrite() error { return nil }

// hostListener hands out forwardConns.
type hostListener struct{ net.Listener }

func (l hostListener) Accept() (net.Conn, error) {
	conn, err := l.Listener.Accept()
	if err != nil {
		return nil, err
	}
	return forwardConn{conn}, nil
}

// forwardConn is the host's end of a stream the guest opened to the host
// through a forward. The host's half-close cannot reach the guest as one
// (hostConn), so it closes the stream: what the far end sent has been
// written, and the guest, the client here, reads EOF. A guest still sending
// when the far end is done sending is cut off, as libkrun would cut it.
type forwardConn struct{ net.Conn }

func (c forwardConn) CloseWrite() error { return c.Close() }

// Listen hands out the socket the guest's connections to the host on a
// forward's port arrive at. Only a forward's port was mapped at boot.
func (vm *vmMachine) Listen(port uint32) (net.Listener, error) {
	l, ok := vm.listeners[port]
	if !ok {
		return nil, fmt.Errorf("libkrun: guest port %d was not forwarded at boot: %w", port, machine.ErrUnsupported)
	}
	return hostListener{l}, nil
}

// Kill powers the guest off at once: SIGKILL to its launcher.
func (vm *vmMachine) Kill(context.Context) error {
	vm.mu.Lock()
	vm.killed = true
	vm.mu.Unlock()
	select {
	case <-vm.done:
		return nil
	default:
	}
	if err := vm.launcher.proc.Kill(); err != nil && !errors.Is(err, os.ErrProcessDone) {
		return fmt.Errorf("libkrun: kill the launcher: %w", err)
	}
	<-vm.done
	return nil
}

func (vm *vmMachine) Done() <-chan struct{} { return vm.done }

func (vm *vmMachine) Err() error {
	<-vm.done
	return vm.err
}

// tailBuffer keeps the last 64 KiB written to it, and copies everything to
// tee.
type tailBuffer struct {
	mu  sync.Mutex
	buf []byte
	tee io.Writer
}

func (t *tailBuffer) Write(p []byte) (int, error) {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.buf = append(t.buf, p...)
	if over := len(t.buf) - 64<<10; over > 0 {
		t.buf = slices.Clone(t.buf[over:])
	}
	if t.tee != nil {
		_, _ = t.tee.Write(p)
	}
	return len(p), nil
}

// note writes s to tee alone, under the lock the launcher's output takes.
func (t *tailBuffer) note(s string) {
	t.mu.Lock()
	defer t.mu.Unlock()
	if t.tee != nil {
		_, _ = io.WriteString(t.tee, s)
	}
}

func (t *tailBuffer) bytes() []byte {
	t.mu.Lock()
	defer t.mu.Unlock()
	return slices.Clone(t.buf)
}
