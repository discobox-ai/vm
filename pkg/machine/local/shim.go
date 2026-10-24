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
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/discobox-ai/vm/internal/fsutil"
	"github.com/discobox-ai/vm/internal/proc"
	"github.com/discobox-ai/vm/pkg/guest"
	"github.com/discobox-ai/vm/pkg/machine"
)

const shimStateName = "shim.json"

// shimState is how other processes find a running instance's shim. It exists
// only while the shim is serving; the token keeps other local users off the
// loopback port.
type shimState struct {
	PID   int    `json:"pid"`
	Addr  string `json:"addr"`
	Token string `json:"token"`
	// Service is the loopback address where the shim serves the instance's
	// service, with no token: the service authenticates its callers.
	Service string `json:"service,omitempty"`
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
	forwards, err := serveForwards(m, inst.Forwards)
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
	listener, err := (&net.ListenConfig{}).Listen(ctx, "tcp", "127.0.0.1:0")
	if err != nil {
		_ = m.Kill(context.Background())
		return err
	}
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
	statePath := filepath.Join(d.instanceDir(id), shimStateName)
	state, closeControl, err := serveControl(listener, mux, statePath, serviceListener.Addr().String())
	if err != nil {
		_ = m.Kill(context.Background())
		return err
	}
	fmt.Fprintf(os.Stderr, "shim: %s running, control on %s\n", inst.Name, state.Addr)

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
func serveService(l net.Listener, m Machine, port uint32) {
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
// connection into its host socket. The returned func stops listening.
func serveForwards(m machine.Machine, forwards []machine.Forward) (func(), error) {
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
					var d net.Dialer
					host, err := d.DialContext(context.Background(), "unix", f.Socket)
					if err != nil {
						fmt.Fprintf(os.Stderr, "shim: forward port %d: %v\n", f.Port, err)
						_ = conn.Close()
						return
					}
					spliceHalves(conn, host)
				}()
			}
		}(l, f)
	}
	return closeAll, nil
}

// serveControl serves a shim's control API on listener, behind a bearer token,
// and publishes it at statePath with the address of the instance's service,
// if it serves one. The returned func unpublishes and stops it.
func serveControl(listener net.Listener, mux *http.ServeMux, statePath, service string) (shimState, func(), error) {
	state := shimState{PID: os.Getpid(), Addr: listener.Addr().String(), Token: fsutil.RandomHex(16), Service: service}
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
	return &shimClient{state: state, http: &http.Client{}}, nil
}

func (c *shimClient) do(ctx context.Context, method, path string) (*http.Response, error) {
	req, err := http.NewRequestWithContext(ctx, method, "http://"+c.state.Addr+path, nil)
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
	var d net.Dialer
	conn, err := d.DialContext(ctx, "tcp", c.state.Addr)
	if err != nil {
		return nil, err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, "http://shim/guest", nil)
	if err != nil {
		_ = conn.Close()
		return nil, err
	}
	req.Header.Set("Authorization", "Bearer "+c.state.Token)
	req.Header.Set("Connection", "Upgrade")
	req.Header.Set("Upgrade", guestUpgrade)
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
		return nil, fmt.Errorf("shim: guest: %s", strings.TrimSpace(string(msg)))
	}
	_ = conn.SetDeadline(time.Time{})
	return &bufConn{Conn: conn, r: reader}, nil
}

// guestUpgrade is the Upgrade token of a connection to a shim's relay.
const guestUpgrade = "disco-vm-guest"

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
