package local

import (
	"bufio"
	"cmp"
	"context"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/discobox-ai/vm/internal/fsutil"
	"github.com/discobox-ai/vm/internal/netutil"
	"github.com/discobox-ai/vm/internal/proc"
	"github.com/discobox-ai/vm/internal/version"
	"github.com/discobox-ai/vm/pkg/guest"
	"github.com/discobox-ai/vm/pkg/machine"
)

const (
	shimStateName  = "shim.json"
	shimSocketName = "shim.sock"
	// shimRefusedName holds why a shim's boot was refused for capacity,
	// written before the shim exits and removed by the Start that spawns
	// the next one.
	shimRefusedName = "shim.refused"
	// shimConfigName is the driver configuration a shim is started with
	// (writeShimConfig), readable by its owner alone.
	shimConfigName = "driver-config.json"
	// shimProtocol is the version of what a caller asks of a shim beyond the
	// part that lasts (shimState): its relay on /guest, which also speaks
	// guest.Protocol, and its streams on /dial. It moves whenever a change
	// to them means an engine and a shim of different builds cannot work
	// together. A shim outlives the engine that started it, an embedder's
	// restart included (discobox ADR 26-11-11-963 §7), so an engine meets shims of
	// other builds, and attaches one of another protocol only to stop it
	// (staleMachine). 1 is every shim from before shim.json named its
	// protocol, whose /dial varies by build (none before 2026-11-01, any
	// port with the service's unmapped before 2026-11-03), so none of them
	// is used beyond the part that lasts.
	shimProtocol = 2
	// shimStarterEnv, set in a shim's environment, says its stdin is its
	// starter's lifeline (holdStarter): the shim gives its boot up if the
	// pipe closes before the Start that spawned it says it is started. A
	// shim of a build before it ignores it, and holds its machine whatever
	// becomes of its starter.
	shimStarterEnv = "DISCO_VM_SHIM_STARTER"
	// abandonWait is how long a Start that gave up waits for its shim to
	// cancel its boot and exit.
	abandonWait = 30 * time.Second
	// maxSocketPath is the longest path a Unix socket may have on every host
	// a shim runs on: macOS's sun_path holds 104 bytes, Linux's 108.
	maxSocketPath = 103
)

// errStarterGaveUp is the cause of a shim's context once the Start that
// spawned it gave up before it came up.
var errStarterGaveUp = errors.New("shim: the start that spawned it gave up")

// shimState is how other processes find a running instance's shim. It exists
// only while the shim is serving, and is private to the owner, as is the
// socket it names; the token is a second check on top.
//
// Its PID, socket (or address), token, and service, and the control API's
// /status, /stop, and /kill, last: every shim of every build keeps them, so
// any engine finds, stops, and kills any shim. The rest is shimProtocol's.
type shimState struct {
	PID int `json:"pid"`
	// Socket is the owner-only Unix socket the control API is served on:
	// shim.sock in the instance's directory, or one in a private temporary
	// directory when that path is too long for a socket.
	Socket string `json:"socket,omitempty"`
	// Addr is the loopback address a shim from before the socket serves its
	// control API on. Such a shim is attached through it until it exits.
	Addr  string `json:"addr,omitempty"`
	Token string `json:"token"`
	// Service is the loopback address where the shim serves the instance's
	// service, with no token: the service authenticates its callers.
	Service string `json:"service,omitempty"`
	// Protocol is the shim's shimProtocol, and GuestProtocol the
	// guest.Protocol its relay speaks. A shim from before they were
	// written is protocol 1, and its relay speaks guest protocol 1.
	Protocol      int `json:"protocol,omitempty"`
	GuestProtocol int `json:"guestProtocol,omitempty"`
	// Build is the shim's build (internal/version), to name it in an error.
	Build string `json:"build,omitempty"`
}

// usable refuses a shim that speaks another protocol than this build, naming
// both, with machine.ErrStale.
func (s shimState) usable() error {
	protocol, guestProtocol := max(s.Protocol, 1), max(s.GuestProtocol, 1)
	if protocol == shimProtocol && guestProtocol == guest.Protocol {
		return nil
	}
	build := s.Build
	if build == "" {
		build = "unknown"
	}
	return fmt.Errorf("its shim (build %s) speaks shim protocol %d and guest protocol %d, and this build (%s) speaks %d and %d: %w",
		build, protocol, guestProtocol, version.String(), shimProtocol, guest.Protocol, machine.ErrStale)
}

// dial connects to the shim's control API.
func (s shimState) dial(ctx context.Context) (net.Conn, error) {
	var d net.Dialer
	if s.Socket != "" {
		return d.DialContext(ctx, "unix", s.Socket)
	}
	return d.DialContext(ctx, "tcp", s.Addr)
}

// listenControl listens on the Unix socket a shim serves its control API on,
// readable and writable by the owner alone. The returned func removes what
// it made.
func listenControl(ctx context.Context, dir string) (net.Listener, string, func(), error) {
	path := filepath.Join(dir, shimSocketName)
	cleanup := func() { _ = os.Remove(path) }
	if len(path) > maxSocketPath {
		tmp, err := os.MkdirTemp("", "dvm-shim-")
		if err != nil {
			return nil, "", nil, err
		}
		path = filepath.Join(tmp, shimSocketName)
		cleanup = func() { _ = os.RemoveAll(tmp) }
	}
	_ = os.Remove(path)
	l, err := (&net.ListenConfig{}).Listen(ctx, "unix", path)
	if err != nil {
		cleanup()
		return nil, "", nil, fmt.Errorf("shim: listen on %s: %w", path, err)
	}
	// Windows keeps no Unix permission bits on a socket; its ACL is the
	// directory's, which is the owner's.
	if err := os.Chmod(path, 0o600); err != nil && runtime.GOOS != "windows" {
		_ = l.Close()
		cleanup()
		return nil, "", nil, err
	}
	return l, path, cleanup, nil
}

// runShim is the body of a shim: boot the instance, serve its control API,
// its URL, and its forwards, and return when the machine stops. Canceling ctx
// (a signal) shuts the guest down in order first.
func (d *Driver) runShim(ctx context.Context, id string, gui bool) error {
	rec, err := d.readRecord(id)
	if err != nil {
		return err
	}
	if rec.Pool != d.pool {
		// A shim command an embedder names carries no pool, and it runs
		// the VMs of every pool it makes (its Engine.From and Forwards
		// reach across them): the record says which is the VM's, to the
		// owner. A key the driver issued is its pool's alone.
		if issued, err := d.caller(ctx); err != nil || issued != nil {
			return fmt.Errorf("instance %s: %w", id, machine.ErrNotFound)
		}
		if d, err = d.InPool(rec.Pool); err != nil {
			return err
		}
	}
	if err := d.checkShimConfig(id); err != nil {
		return err
	}
	inst := rec.Instance
	hs, err := d.spec(rec)
	if err != nil {
		return err
	}
	_, servicePort, err := d.hv.Service(hs)
	if err != nil {
		return err
	}
	if err := context.Cause(ctx); err != nil {
		return err
	}
	m, err := d.boot(ctx, inst, machine.BootOptions{Console: os.Stderr, GUI: gui})
	if errors.Is(err, machine.ErrCapacity) {
		// Start reads this once the shim has exited, and refuses with
		// ErrCapacity as it would have before spawning the shim.
		why := strings.TrimSuffix(err.Error(), ": "+machine.ErrCapacity.Error())
		_ = os.WriteFile(filepath.Join(d.instanceDir(id), shimRefusedName), []byte(why), 0o600)
	}
	if err != nil {
		return err
	}
	forwards, err := d.serveForwards(m, inst.Forwards)
	if err != nil {
		_ = m.Kill(context.Background())
		return err
	}
	defer forwards()
	serviceListener, err := (&net.ListenConfig{}).Listen(ctx, "tcp", "127.0.0.1:0")
	if err != nil {
		_ = m.Kill(context.Background())
		return err
	}
	defer serviceListener.Close()
	go serveService(serviceListener, m, servicePort)
	listener, socket, removeSocket, err := listenControl(ctx, d.instanceDir(id))
	if err != nil {
		_ = m.Kill(context.Background())
		return err
	}
	defer removeSocket()
	var stopOnce sync.Once
	var stopErr error
	stop := func(timeout time.Duration) error {
		stopOnce.Do(func() { stopErr = shutdown(context.Background(), m, timeout) })
		return stopErr
	}

	mux := http.NewServeMux()
	mux.HandleFunc("GET /status", func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
	})
	mux.HandleFunc("POST /stop", func(w http.ResponseWriter, r *http.Request) {
		timeout, err := time.ParseDuration(r.URL.Query().Get("timeout"))
		if err != nil || timeout <= 0 {
			timeout = time.Minute
		}
		if err := stop(timeout); err != nil {
			// Forced off is still off; say how it went.
			http.Error(w, err.Error(), http.StatusOK)
			return
		}
		w.WriteHeader(http.StatusNoContent)
	})
	mux.HandleFunc("POST /kill", func(w http.ResponseWriter, r *http.Request) {
		if err := m.Kill(r.Context()); err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		w.WriteHeader(http.StatusNoContent)
	})
	// Other processes reach the machine's guest operations here, on
	// connections the relay serves once they are upgraded.
	relay := newConnListener(listener.Addr())
	relayServer := &http.Server{Handler: guest.Relay(m), ReadHeaderTimeout: 30 * time.Second}
	go func() { _ = relayServer.Serve(relay) }()
	defer relayServer.Close()
	mux.HandleFunc("GET /guest", func(w http.ResponseWriter, _ *http.Request) {
		conn, rw, err := http.NewResponseController(w).Hijack()
		if err != nil {
			return
		}
		_, _ = rw.WriteString("HTTP/1.1 101 Switching Protocols\r\nConnection: Upgrade\r\nUpgrade: " + guestUpgrade + "\r\n\r\n")
		if err := rw.Flush(); err != nil {
			_ = conn.Close()
			return
		}
		relay.hand(&netutil.BufConn{Conn: conn, R: rw.Reader})
	})
	// Other processes reach the agent's port and the service's here
	// (guest.DialablePort): the connection, once upgraded, is spliced into
	// the machine's Dial of the port, the service's as the hypervisor serves
	// it.
	mux.HandleFunc("GET /dial", func(w http.ResponseWriter, r *http.Request) {
		port, err := strconv.ParseUint(r.URL.Query().Get("port"), 10, 32)
		if err != nil || port == 0 {
			http.Error(w, "dial: want ?port=N", http.StatusBadRequest)
			return
		}
		if !guest.DialablePort(uint32(port)) {
			http.Error(w, fmt.Sprintf("dial: guest port %d is neither the agent's nor the service's", port), http.StatusForbidden)
			return
		}
		target := uint32(port)
		if target == guest.ServicePort {
			target = servicePort
		}
		dialCtx, cancel := context.WithTimeout(r.Context(), 30*time.Second)
		upstream, err := m.Dial(dialCtx, target)
		cancel()
		if err != nil {
			http.Error(w, err.Error(), http.StatusBadGateway)
			return
		}
		conn, rw, err := http.NewResponseController(w).Hijack()
		if err != nil {
			_ = upstream.Close()
			return
		}
		_, _ = rw.WriteString("HTTP/1.1 101 Switching Protocols\r\nConnection: Upgrade\r\nUpgrade: " + dialUpgrade + "\r\n\r\n")
		if err := rw.Flush(); err != nil {
			_ = conn.Close()
			_ = upstream.Close()
			return
		}
		go netutil.Splice(&netutil.BufConn{Conn: conn, R: rw.Reader}, upstream)
	})
	statePath := filepath.Join(d.instanceDir(id), shimStateName)
	state, closeControl, err := serveControl(listener, socket, mux, statePath, serviceListener.Addr().String())
	if err != nil {
		_ = m.Kill(context.Background())
		return err
	}
	fmt.Fprintf(os.Stderr, "shim: %s running, control on %s\n", inst.Name, state.Socket)

	select {
	case <-m.Done():
	case <-ctx.Done():
		if errors.Is(context.Cause(ctx), errStarterGaveUp) {
			// No caller was handed the machine: nothing in it to keep.
			_ = m.Kill(context.Background())
		} else {
			_ = stop(time.Minute)
		}
	}
	closeControl()
	err = m.Err()
	fmt.Fprintf(os.Stderr, "shim: %s stopped (%v)\n", inst.Name, err)
	return err
}

// shutdown powers a machine off in order: the guest is asked, and the
// machine is killed only if it will not answer or will not stop in time.
func shutdown(ctx context.Context, m machine.Machine, timeout time.Duration) error {
	select {
	case <-m.Done():
		return m.Err()
	default:
	}
	deadline := time.Now().Add(timeout)
	askCtx, cancel := context.WithDeadline(ctx, deadline)
	err := m.Shutdown(askCtx, false)
	cancel()
	if err == nil {
		select {
		case <-m.Done():
			return m.Err()
		case <-time.After(time.Until(deadline)):
			err = fmt.Errorf("guest did not power off within %s", timeout)
		case <-ctx.Done():
			err = ctx.Err()
		}
	}
	if killErr := m.Kill(context.Background()); killErr != nil {
		return fmt.Errorf("not stopped in order (%w), nor forced off: %w", err, killErr)
	}
	return fmt.Errorf("forced off: %w", err)
}

// serveService splices every connection to the instance's loopback URL into
// the service's guest port, until the listener is closed. A guest with nothing
// listening yet closes the connection.
func serveService(l net.Listener, m machine.Machine, port uint32) {
	for {
		conn, err := l.Accept()
		if err != nil {
			return
		}
		go func() {
			ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
			upstream, err := m.Dial(ctx, port)
			cancel()
			if err != nil {
				_ = conn.Close()
				return
			}
			netutil.Splice(conn, upstream)
		}()
	}
}

// serveForwards listens for the guest on each forward's port and splices every
// connection into its host socket, or into its peer's guest port through the
// peer's shim. The returned func stops listening.
func (d *Driver) serveForwards(m machine.Machine, forwards []machine.Forward) (func(), error) {
	if len(forwards) == 0 {
		return func() {}, nil
	}
	hl, ok := m.(machine.HostListener)
	if !ok {
		return nil, errors.New("this machine cannot take a guest's connections to the host")
	}
	var listeners []net.Listener
	closeAll := func() {
		for _, l := range listeners {
			_ = l.Close()
		}
	}
	for _, f := range forwards {
		l, err := hl.Listen(f.Port)
		if err != nil {
			closeAll()
			return nil, err
		}
		listeners = append(listeners, l)
		go func(l net.Listener, f machine.Forward) {
			for {
				conn, err := l.Accept()
				if err != nil {
					return
				}
				go func() {
					// Not the shim's ctx: a guest shutting down in order after
					// a signal may still connect out.
					ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
					upstream, err := d.dialForward(ctx, f)
					cancel()
					if err != nil {
						fmt.Fprintf(os.Stderr, "shim: forward port %d: %v\n", f.Port, err)
						_ = conn.Close()
						return
					}
					netutil.Splice(conn, upstream)
				}()
			}
		}(l, f)
	}
	return closeAll, nil
}

// dialForward opens the far end of one of a forward's connections: its host
// socket, or its peer's guest port through the shim that holds the peer,
// found by name at every connection so a peer that restarted is followed.
func (d *Driver) dialForward(ctx context.Context, f machine.Forward) (net.Conn, error) {
	if !f.Peer() {
		var dialer net.Dialer
		return dialer.DialContext(ctx, "unix", f.Socket)
	}
	peer, err := d.instanceIn(cmp.Or(f.Pool, d.pool), f.Instance)
	if err != nil {
		return nil, err
	}
	shim, err := d.openShim(peer.ID)
	if err != nil {
		return nil, fmt.Errorf("instance %s: %w", peer.Name, machine.ErrNotRunning)
	}
	conn, err := shim.dial(ctx, f.InstancePort)
	if err != nil {
		return nil, fmt.Errorf("instance %s: %w", peer.Name, err)
	}
	return conn, nil
}

// serveControl serves a shim's control API on listener, the Unix socket at
// socket, behind a bearer token, and publishes it at statePath with the
// address of the instance's service, if it serves one. The returned func
// unpublishes and stops it.
func serveControl(listener net.Listener, socket string, mux *http.ServeMux, statePath, service string) (shimState, func(), error) {
	state := shimState{
		PID: os.Getpid(), Socket: socket, Token: fsutil.RandomHex(16), Service: service,
		Protocol: shimProtocol, GuestProtocol: guest.Protocol, Build: version.String(),
	}
	server := &http.Server{
		Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if r.Header.Get("Authorization") != "Bearer "+state.Token {
				http.Error(w, "unauthorized", http.StatusUnauthorized)
				return
			}
			mux.ServeHTTP(w, r)
		}),
		ReadHeaderTimeout: 10 * time.Second,
	}
	go func() { _ = server.Serve(listener) }()
	if err := fsutil.WriteJSON(statePath, state); err != nil {
		_ = server.Close()
		return shimState{}, nil, err
	}
	return state, func() {
		_ = os.Remove(statePath)
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = server.Shutdown(ctx)
	}, nil
}

// ShimMain is the body of a shim, given the arguments Start appends to the
// shim command: [--gui] INSTANCE-ID. driver is the one Start was called on,
// made again from the same configuration: a program that embeds the engine
// calls it from the subcommand its machine.Config.ShimCommand names.
func ShimMain(ctx context.Context, driver machine.Driver, args []string) error {
	var gui bool
	var rest []string
	for _, arg := range args {
		if arg == "--gui" {
			gui = true
		} else {
			rest = append(rest, arg)
		}
	}
	if len(rest) != 1 {
		return fmt.Errorf("shim: want [--gui] INSTANCE, not %q", args)
	}
	d, ok := driver.(*Driver)
	if !ok {
		return fmt.Errorf("shim: the %s driver runs no shims", driver.Name())
	}
	ctx, cancel := holdStarter(ctx)
	defer cancel(nil)
	return d.runShim(ctx, rest[0], gui)
}

// shimCommand is how a shim is started: the configured command, or this
// executable's `shim` subcommand for the same driver, pool, and directory,
// given the driver's configuration in the file config names, never on its
// command line (ADR 26-11-11-215 §6).
func (d *Driver) shimCommand(config string) ([]string, error) {
	if len(d.shim) > 0 {
		return d.shim, nil
	}
	exe, err := os.Executable()
	if err != nil {
		return nil, err
	}
	return []string{exe, "--root", d.dir, "--driver", d.hv.Name(), "--pool", d.pool, "--driver-config", "@" + config, "shim"}, nil
}

// writeShimConfig writes the driver's configuration to the instance's
// directory, readable by its owner alone, for the shim Start spawns: a shim
// of this executable is started with it, and any shim is refused a driver
// whose configuration is not it (checkShimConfig).
func (d *Driver) writeShimConfig(dir string) (string, error) {
	config, err := machine.CanonicalConfig(d.config)
	if err != nil {
		return "", err
	}
	path := filepath.Join(dir, shimConfigName)
	_ = os.Remove(path)
	if err := os.WriteFile(path, []byte(config), 0o600); err != nil {
		return "", err
	}
	return path, nil
}

// checkShimConfig refuses to run a shim with a driver whose configuration
// is not the one Start wrote for the instance: an embedder's shim made from
// another, or one that read its own environment. A shim run by hand, of an
// instance no Start wrote one for, has no starter to agree with.
func (d *Driver) checkShimConfig(id string) error {
	data, err := os.ReadFile(filepath.Join(d.instanceDir(id), shimConfigName))
	if errors.Is(err, fs.ErrNotExist) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("shim: the configuration the instance was started with: %w", err)
	}
	want, err := machine.CanonicalConfig(data)
	if err != nil {
		return fmt.Errorf("shim: the configuration the instance was started with: %w", err)
	}
	got, err := machine.CanonicalConfig(d.config)
	if err != nil {
		return err
	}
	if got != want {
		return fmt.Errorf("shim: the %s driver's configuration is not the one instance %s was started with", d.hv.Name(), id)
	}
	return nil
}

// startedShim is a shim Start spawned and waits on.
type startedShim struct {
	cmd *exec.Cmd
	// exited is closed once the shim exits, and err is then how.
	exited chan struct{}
	err    error
	// lifeline is the write end of the shim's stdin (shimStarterEnv): one
	// byte on it tells the shim it is started, and closing it without one
	// tells the shim its start gave up.
	lifeline *os.File
}

// spawnShim starts a shim with args, detached from this process, so the VM
// outlives the command that started it once the shim is up. Until then its
// stdin is the returned shim's lifeline.
func (d *Driver) spawnShim(config string, args []string, log *os.File) (*startedShim, error) {
	argv, err := d.shimCommand(config)
	if err != nil {
		return nil, err
	}
	stdin, lifeline, err := os.Pipe()
	if err != nil {
		return nil, err
	}
	defer stdin.Close()
	// Not tied to a ctx: the shim outlives the command that spawned it.
	cmd := exec.CommandContext(context.Background(), argv[0], append(slices.Clone(argv[1:]), args...)...) //nolint:gosec // G204: argv is this executable or its configured shim command
	cmd.Stdin, cmd.Stdout, cmd.Stderr = stdin, log, log
	// The shim holds the machine as the directory's owner, with no key.
	cmd.Env = append(machine.WithoutKeys(os.Environ()), shimStarterEnv+"=1")
	proc.Detach(cmd)
	if err := cmd.Start(); err != nil {
		_ = lifeline.Close()
		return nil, fmt.Errorf("start shim: %w", err)
	}
	s := &startedShim{cmd: cmd, exited: make(chan struct{}), lifeline: lifeline}
	go func() {
		s.err = cmd.Wait()
		close(s.exited)
	}()
	return s, nil
}

// started tells the shim it is started, so it holds its machine from now on
// whatever becomes of this process. It fails if the shim has exited.
func (s *startedShim) started() error {
	_, err := s.lifeline.Write([]byte{1})
	if closeErr := s.lifeline.Close(); err == nil {
		err = closeErr
	}
	return err
}

// abandon tells the shim its start gave up, so it cancels its boot, or
// kills the machine it booted, and exits; and waits up to abandonWait for it
// to, so its admission is free once this returns. It never kills the shim:
// an HCS VM, or a fake guest, outlives its shim's process, and a shim of a
// build before the lifeline would leave its VM with no shim at all. One that
// has not exited is described in the error returned; it holds the
// instance's boot lock until it does, and a shim that comes up after all
// is listed running, and is stopped as any other.
func (s *startedShim) abandon() error {
	_ = s.lifeline.Close()
	select {
	case <-s.exited:
		return nil
	case <-time.After(abandonWait):
		return fmt.Errorf("its shim (pid %d) has not given its boot up after %s", s.cmd.Process.Pid, abandonWait)
	}
}

// holdStarter returns ctx canceled, with errStarterGaveUp as its cause, if
// the shim's stdin closes before its starter says it is started: the Start
// that spawned it gave up, or its process died. A shim no Start spawned
// (shimStarterEnv unset), as one run by hand, has no starter to wait on.
// The variable is taken out of the shim's environment, so nothing the shim
// starts, a fake guest included, inherits it.
func holdStarter(ctx context.Context) (context.Context, context.CancelCauseFunc) {
	ctx, cancel := context.WithCancelCause(ctx)
	if os.Getenv(shimStarterEnv) == "" {
		return ctx, cancel
	}
	_ = os.Unsetenv(shimStarterEnv)
	go func() {
		var b [1]byte
		if n, _ := os.Stdin.Read(b[:]); n == 0 {
			cancel(errStarterGaveUp)
		}
		_ = os.Stdin.Close()
	}()
	return ctx, cancel
}

// Start boots the instance under a shim of its own, and returns once the shim
// has the machine and serves it.
func (d *Driver) Start(ctx context.Context, inst machine.Instance, opts machine.BootOptions) (machine.Machine, error) {
	if err := d.authorizeInstance(ctx, inst); err != nil {
		return nil, err
	}
	if opts.GUI && !d.hv.Capabilities().Display {
		return nil, fmt.Errorf("%s has no display to show: %w", d.hv.Name(), machine.ErrUnsupported)
	}
	rec, err := d.runnable(inst)
	if err != nil {
		return nil, err
	}
	if shim, err := d.openShim(inst.ID); err == nil && shim.status(ctx) == nil {
		return nil, fmt.Errorf("instance %s is already running", inst.Name)
	}
	// The shim's boot is what admits the instance and holds its slot, since
	// the shim outlives this process. Asking here first refuses a full host
	// before a shim is spawned, with machine.ErrCapacity; a boot admitted
	// between this and the shim's is refused by the shim instead, which
	// leaves shim.refused for this to return as ErrCapacity too.
	release, err := d.admit(rec)
	if err != nil {
		return nil, err
	}
	release()
	dir := d.instanceDir(inst.ID)
	_ = os.Remove(filepath.Join(dir, shimStateName))
	refused := filepath.Join(dir, shimRefusedName)
	_ = os.Remove(refused)
	logFile, err := os.OpenFile(filepath.Join(dir, "shim.log"), os.O_WRONLY|os.O_CREATE|os.O_APPEND, 0o600)
	if err != nil {
		return nil, err
	}
	defer logFile.Close()
	config, err := d.writeShimConfig(dir)
	if err != nil {
		return nil, err
	}
	args := []string{inst.ID}
	if opts.GUI {
		args = []string{"--gui", inst.ID}
	}
	shim, err := d.spawnShim(config, args, logFile)
	if err != nil {
		return nil, err
	}
	for {
		if m, err := d.Attach(ctx, inst); err == nil {
			if err := shim.started(); err != nil {
				_ = shim.abandon()
				return nil, fmt.Errorf("instance %s failed to start: its shim exited once up: %w\n%s", inst.Name, err, tail(filepath.Join(dir, "shim.log")))
			}
			return m, nil
		}
		select {
		case <-shim.exited:
			err := shim.err
			_ = shim.lifeline.Close()
			if why, readErr := os.ReadFile(refused); readErr == nil {
				_ = os.Remove(refused)
				return nil, fmt.Errorf("instance %s failed to start: %s: %w", inst.Name, strings.TrimSpace(string(why)), machine.ErrCapacity)
			}
			return nil, fmt.Errorf("instance %s failed to start: %w\n%s", inst.Name, err, tail(filepath.Join(dir, "shim.log")))
		case <-ctx.Done():
			// The shim cancels its boot, and stops the machine it may
			// already hold, once its lifeline closes unanswered.
			if err := shim.abandon(); err != nil {
				return nil, fmt.Errorf("instance %s: shim did not come up: %w; %w", inst.Name, ctx.Err(), err)
			}
			return nil, fmt.Errorf("instance %s: shim did not come up: %w", inst.Name, ctx.Err())
		case <-time.After(50 * time.Millisecond):
		}
	}
}

func tail(path string) string {
	data, err := os.ReadFile(path)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return ""
		}
		return err.Error()
	}
	lines := strings.Split(strings.TrimRight(string(data), "\n"), "\n")
	if len(lines) > 20 {
		lines = lines[len(lines)-20:]
	}
	return strings.Join(lines, "\n")
}

// shimClient is another process's handle on a running shim.
type shimClient struct {
	state shimState
	http  *http.Client
	// stale is why the shim cannot be used beyond the part of it that
	// lasts (shimState.usable), or nil.
	stale error
}

// openShim finds a running instance's shim, of any build. It fails when the
// instance is not running.
func (d *Driver) openShim(id string) (*shimClient, error) {
	statePath := filepath.Join(d.instanceDir(id), shimStateName)
	var state shimState
	if err := fsutil.ReadJSON(statePath, &state); err != nil {
		return nil, err
	}
	if !proc.Alive(state.PID) {
		return nil, errors.New("shim is gone")
	}
	// No keep-alives: a client is made per call, so an idle connection it
	// kept would hold a descriptor open in the caller and in the shim, one
	// per call, for as long as the caller runs.
	transport := &http.Transport{
		DialContext:       func(ctx context.Context, _, _ string) (net.Conn, error) { return state.dial(ctx) },
		DisableKeepAlives: true,
	}
	return &shimClient{state: state, http: &http.Client{Transport: transport}, stale: state.usable()}, nil
}

func (c *shimClient) do(ctx context.Context, method, path string) (*http.Response, error) {
	req, err := http.NewRequestWithContext(ctx, method, "http://shim"+path, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Authorization", "Bearer "+c.state.Token)
	return c.http.Do(req)
}

func (c *shimClient) status(ctx context.Context) error {
	resp, err := c.do(ctx, http.MethodGet, "/status")
	if err != nil {
		return err
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("shim status: %s", resp.Status)
	}
	return nil
}

// kill forces the machine off and waits for the shim to exit, so the caller
// can delete the instance's files at once (Windows will not remove a file a
// live process still has open). A shim from before /kill is stopped with no
// grace instead.
func (c *shimClient) kill(ctx context.Context) error {
	resp, err := c.do(ctx, http.MethodPost, "/kill")
	if err != nil {
		return err
	}
	resp.Body.Close()
	if resp.StatusCode == http.StatusNotFound {
		return c.stop(ctx, time.Second)
	}
	c.waitExit()
	return nil
}

// waitExit waits a moment for the shim's process to end.
func (c *shimClient) waitExit() {
	deadline := time.Now().Add(15 * time.Second)
	for proc.Alive(c.state.PID) && time.Now().Before(deadline) {
		time.Sleep(50 * time.Millisecond)
	}
}

// stop asks for an orderly shutdown and waits for the shim to exit, so the
// caller can delete the instance's files at once (Windows will not remove a
// file a live process still has open).
func (c *shimClient) stop(ctx context.Context, timeout time.Duration) error {
	resp, err := c.do(ctx, http.MethodPost, "/stop?timeout="+timeout.String())
	if err != nil {
		return err
	}
	body, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
	resp.Body.Close()
	c.waitExit()
	if msg := strings.TrimSpace(string(body)); msg != "" {
		return errors.New(msg)
	}
	return nil
}

// dialGuest opens a connection to the shim's relay of its machine's guest
// operations, which speaks the agent's protocol (guest.Client).
func (c *shimClient) dialGuest(ctx context.Context) (net.Conn, error) {
	return c.upgrade(ctx, "/guest", guestUpgrade)
}

// dial opens a stream to a guest port of the shim's machine.
func (c *shimClient) dial(ctx context.Context, port uint32) (net.Conn, error) {
	if !guest.DialablePort(port) {
		return nil, fmt.Errorf("guest port %d: a caller reaches the agent's port (%d) and the service's (%d) alone: %w", port, guest.AgentPort, guest.ServicePort, machine.ErrUnsupported)
	}
	return c.upgrade(ctx, "/dial?port="+strconv.FormatUint(uint64(port), 10), dialUpgrade)
}

// upgrade asks the shim to upgrade a connection to one of its streams, and
// returns the connection once it has. A stale shim's streams are refused.
func (c *shimClient) upgrade(ctx context.Context, path, token string) (net.Conn, error) {
	if c.stale != nil {
		return nil, c.stale
	}
	conn, err := c.state.dial(ctx)
	if err != nil {
		return nil, err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, "http://shim"+path, nil)
	if err != nil {
		_ = conn.Close()
		return nil, err
	}
	req.Header.Set("Authorization", "Bearer "+c.state.Token)
	req.Header.Set("Connection", "Upgrade")
	req.Header.Set("Upgrade", token)
	if deadline, ok := ctx.Deadline(); ok {
		_ = conn.SetDeadline(deadline)
	}
	if err := req.Write(conn); err != nil {
		_ = conn.Close()
		return nil, err
	}
	reader := bufio.NewReader(conn)
	resp, err := http.ReadResponse(reader, req) //nolint:bodyclose // on 101 the body is the upgraded conn, returned as netutil.BufConn; otherwise conn is closed
	if err != nil {
		_ = conn.Close()
		return nil, err
	}
	if resp.StatusCode != http.StatusSwitchingProtocols {
		msg, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
		_ = conn.Close()
		what, _, _ := strings.Cut(strings.TrimPrefix(path, "/"), "?")
		return nil, fmt.Errorf("shim: %s: %s", what, strings.TrimSpace(string(msg)))
	}
	_ = conn.SetDeadline(time.Time{})
	return &netutil.BufConn{Conn: conn, R: reader}, nil
}

const (
	// guestUpgrade is the Upgrade token of a connection to a shim's relay.
	guestUpgrade = "disco-vm-guest"
	// dialUpgrade is the Upgrade token of a connection to a guest port.
	dialUpgrade = "disco-vm-dial"
)

// connListener is a net.Listener whose connections are handed to it: those a
// shim's control API upgraded for its relay.
type connListener struct {
	addr   net.Addr
	conns  chan net.Conn
	closed chan struct{}
	once   sync.Once
}

func newConnListener(addr net.Addr) *connListener {
	return &connListener{addr: addr, conns: make(chan net.Conn), closed: make(chan struct{})}
}

// hand gives the listener a connection, or closes it once the listener is.
func (l *connListener) hand(conn net.Conn) {
	select {
	case l.conns <- conn:
	case <-l.closed:
		_ = conn.Close()
	}
}

func (l *connListener) Accept() (net.Conn, error) {
	select {
	case conn := <-l.conns:
		return conn, nil
	case <-l.closed:
		return nil, net.ErrClosed
	}
}

func (l *connListener) Close() error {
	l.once.Do(func() { close(l.closed) })
	return nil
}

func (l *connListener) Addr() net.Addr { return l.addr }
