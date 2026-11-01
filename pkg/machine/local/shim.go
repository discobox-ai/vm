package local

import (
	"bufio"
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
	"github.com/discobox-ai/vm/internal/proc"
	"github.com/discobox-ai/vm/pkg/guest"
	"github.com/discobox-ai/vm/pkg/machine"
)

const (
	shimStateName  = "shim.json"
	shimSocketName = "shim.sock"
	// maxSocketPath is the longest path a Unix socket may have on every host
	// a shim runs on: macOS's sun_path holds 104 bytes, Linux's 108.
	maxSocketPath = 103
)

// shimState is how other processes find a running instance's shim. It exists
// only while the shim is serving, and is private to the owner, as is the
// socket it names; the token is a second check on top.
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
	inst, err := d.readInstance(id)
	if err != nil {
		return err
	}
	hs, err := d.spec(inst)
	if err != nil {
		return err
	}
	_, port, err := d.hv.Service(hs)
	if err != nil {
		return err
	}
	m, err := d.boot(ctx, inst, machine.BootOptions{Console: os.Stderr, GUI: gui})
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
	go serveService(serviceListener, m, port)
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
		relay.hand(&bufConn{Conn: conn, r: rw.Reader})
	})
	// Other processes reach any guest port here: the connection, once
	// upgraded, is spliced into the machine's Dial of the port.
	mux.HandleFunc("GET /dial", func(w http.ResponseWriter, r *http.Request) {
		port, err := strconv.ParseUint(r.URL.Query().Get("port"), 10, 32)
		if err != nil || port == 0 {
			http.Error(w, "dial: want ?port=N", http.StatusBadRequest)
			return
		}
		dialCtx, cancel := context.WithTimeout(r.Context(), 30*time.Second)
		upstream, err := m.Dial(dialCtx, uint32(port))
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
		go spliceHalves(&bufConn{Conn: conn, r: rw.Reader}, upstream)
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
		_ = stop(time.Minute)
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
			spliceHalves(conn, upstream)
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
					spliceHalves(conn, upstream)
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
	peer, err := d.Instance(ctx, f.Instance)
	if err != nil {
		return nil, err
	}
	shim, err := d.openShim(peer.ID)
	if err != nil {
		return nil, fmt.Errorf("instance %s: %w", peer.Name, machine.ErrNotRunning)
	}
	return shim.dial(ctx, f.InstancePort)
}

// serveControl serves a shim's control API on listener, the Unix socket at
// socket, behind a bearer token, and publishes it at statePath with the
// address of the instance's service, if it serves one. The returned func
// unpublishes and stops it.
func serveControl(listener net.Listener, socket string, mux *http.ServeMux, statePath, service string) (shimState, func(), error) {
	state := shimState{PID: os.Getpid(), Socket: socket, Token: fsutil.RandomHex(16), Service: service}
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

// spliceHalves copies both ways and passes each end's EOF on as a half-close,
// so one side can finish sending and still read the answer. It closes both
// once both directions are done. An end that cannot half-close is closed.
func spliceHalves(a, b net.Conn) {
	var wg sync.WaitGroup
	wg.Add(2)
	copyHalf := func(dst, src net.Conn) {
		defer wg.Done()
		_, _ = io.Copy(dst, src)
		if cw, ok := dst.(interface{ CloseWrite() error }); ok {
			_ = cw.CloseWrite()
		} else {
			_ = dst.Close()
		}
	}
	go copyHalf(a, b)
	go copyHalf(b, a)
	wg.Wait()
	_ = a.Close()
	_ = b.Close()
}

// bufConn is a hijacked connection whose first bytes may already be buffered.
type bufConn struct {
	net.Conn
	r *bufio.Reader
}

func (c *bufConn) Read(p []byte) (int, error) { return c.r.Read(p) }

// CloseWrite half-closes the connection underneath, so a splice through it
// passes an EOF on; one that cannot is closed.
func (c *bufConn) CloseWrite() error {
	if cw, ok := c.Conn.(interface{ CloseWrite() error }); ok {
		return cw.CloseWrite()
	}
	return c.Close()
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
	return d.runShim(ctx, rest[0], gui)
}

// shimCommand is how a shim is started: the configured command, or this
// executable's `shim` subcommand for the same driver, project, and directory.
func (d *Driver) shimCommand() ([]string, error) {
	if len(d.shim) > 0 {
		return d.shim, nil
	}
	exe, err := os.Executable()
	if err != nil {
		return nil, err
	}
	return []string{exe, "--root", d.dir, "--driver", d.hv.Name(), "--project", d.project, "shim"}, nil
}

// spawnShim starts a shim with args, detached from this process, so the VM
// outlives the command that started it. The returned channel yields once,
// when the shim exits.
func (d *Driver) spawnShim(args []string, log *os.File) (<-chan error, error) {
	argv, err := d.shimCommand()
	if err != nil {
		return nil, err
	}
	// Not tied to a ctx: the shim outlives the command that spawned it.
	cmd := exec.CommandContext(context.Background(), argv[0], append(slices.Clone(argv[1:]), args...)...) //nolint:gosec // G204: argv is this executable or its configured shim command
	cmd.Stdout, cmd.Stderr = log, log
	// The shim holds the machine as the directory's owner, with no key.
	cmd.Env = machine.WithoutKeys(os.Environ())
	proc.Detach(cmd)
	if err := cmd.Start(); err != nil {
		return nil, fmt.Errorf("start shim: %w", err)
	}
	exited := make(chan error, 1)
	go func() { exited <- cmd.Wait() }()
	return exited, nil
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
	if shim, err := d.openShim(inst.ID); err == nil && shim.status(ctx) == nil {
		return nil, fmt.Errorf("instance %s is already running", inst.Name)
	}
	dir := d.instanceDir(inst.ID)
	_ = os.Remove(filepath.Join(dir, shimStateName))
	logFile, err := os.OpenFile(filepath.Join(dir, "shim.log"), os.O_WRONLY|os.O_CREATE|os.O_APPEND, 0o600)
	if err != nil {
		return nil, err
	}
	defer logFile.Close()
	args := []string{inst.ID}
	if opts.GUI {
		args = []string{"--gui", inst.ID}
	}
	exited, err := d.spawnShim(args, logFile)
	if err != nil {
		return nil, err
	}
	for {
		if m, err := d.Attach(ctx, inst); err == nil {
			return m, nil
		}
		select {
		case err := <-exited:
			return nil, fmt.Errorf("instance %s failed to start: %w\n%s", inst.Name, err, tail(filepath.Join(dir, "shim.log")))
		case <-ctx.Done():
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
}

// openShim finds a running instance's shim. It fails when the instance is not
// running.
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
	return &shimClient{state: state, http: &http.Client{Transport: transport}}, nil
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
	return c.upgrade(ctx, "/dial?port="+strconv.FormatUint(uint64(port), 10), dialUpgrade)
}

// upgrade asks the shim to upgrade a connection to one of its streams, and
// returns the connection once it has.
func (c *shimClient) upgrade(ctx context.Context, path, token string) (net.Conn, error) {
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
	resp, err := http.ReadResponse(reader, req) //nolint:bodyclose // on 101 the body is the upgraded conn, returned as bufConn; otherwise conn is closed
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
	return &bufConn{Conn: conn, r: reader}, nil
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
