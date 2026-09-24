package engine

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/discobox-ai/vm/internal/fsutil"
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
}

// RunShim is the body of `disco-vm shim <id>`: boot the instance, serve its
// control API, and return when the machine stops. Canceling ctx (a signal)
// shuts the guest down in order first.
func (e *Engine) RunShim(ctx context.Context, id string, gui bool) error {
	inst, err := e.Get(id)
	if err != nil {
		return err
	}
	booted, err := e.Boot(ctx, inst, BootOptions{Console: os.Stderr, GUI: gui})
	if err != nil {
		return err
	}
	forwards, err := serveForwards(booted.Machine, inst.Forwards)
	if err != nil {
		_ = booted.Machine.Kill(context.Background())
		return err
	}
	defer forwards()
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		_ = booted.Machine.Kill(context.Background())
		return err
	}
	var stopOnce sync.Once
	var stopErr error
	stop := func(timeout time.Duration) error {
		stopOnce.Do(func() { stopErr = booted.Shutdown(context.Background(), timeout) })
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
	mux.HandleFunc("GET /dial", func(w http.ResponseWriter, r *http.Request) {
		port, err := strconv.ParseUint(r.URL.Query().Get("port"), 10, 32)
		if err != nil {
			http.Error(w, "port is required", http.StatusBadRequest)
			return
		}
		upstream, err := booted.Machine.Dial(r.Context(), uint32(port))
		if err != nil {
			http.Error(w, err.Error(), http.StatusBadGateway)
			return
		}
		conn, rw, err := w.(http.Hijacker).Hijack()
		if err != nil {
			_ = upstream.Close()
			return
		}
		_, _ = rw.WriteString("HTTP/1.1 101 Switching Protocols\r\nConnection: Upgrade\r\nUpgrade: disco-vm-dial\r\n\r\n")
		_ = rw.Flush()
		splice(&bufConn{Conn: conn, r: rw.Reader}, upstream)
	})
	statePath := filepath.Join(e.instanceDir(id), shimStateName)
	state, closeControl, err := serveControl(listener, mux, statePath)
	if err != nil {
		_ = booted.Machine.Kill(context.Background())
		return err
	}
	fmt.Fprintf(os.Stderr, "shim: %s running, control on %s\n", inst.Name, state.Addr)

	select {
	case <-booted.Machine.Done():
	case <-ctx.Done():
		_ = stop(time.Minute)
	}
	closeControl()
	err = booted.Machine.Err()
	fmt.Fprintf(os.Stderr, "shim: %s stopped (%v)\n", inst.Name, err)
	return err
}

// serveForwards listens for the guest on each forward's port and splices every
// connection into its host socket. The returned func stops listening.
func serveForwards(m machine.Machine, forwards []Forward) (func(), error) {
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
		go func(l net.Listener, f Forward) {
			for {
				conn, err := l.Accept()
				if err != nil {
					return
				}
				go func() {
					host, err := net.Dial("unix", f.Socket)
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
// and publishes it at statePath. The returned func unpublishes and stops it.
func serveControl(listener net.Listener, mux *http.ServeMux, statePath string) (shimState, func(), error) {
	state := shimState{PID: os.Getpid(), Addr: listener.Addr().String(), Token: fsutil.RandomHex(16)}
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

// splice copies both ways until either side is done, then closes both.
func splice(a, b net.Conn) {
	var wg sync.WaitGroup
	wg.Add(2)
	copyHalf := func(dst, src net.Conn) {
		defer wg.Done()
		_, _ = io.Copy(dst, src)
		_ = dst.Close()
		_ = src.Close()
	}
	go copyHalf(a, b)
	go copyHalf(b, a)
	wg.Wait()
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

// ShimMain is the body of a shim, given the arguments the engine appends to
// ShimCommand: [--gui] INSTANCE, or --warm LAYER. A program that embeds the
// engine calls it from the subcommand ShimCommand names.
func (e *Engine) ShimMain(ctx context.Context, args []string) error {
	var gui, warm bool
	var rest []string
	for _, arg := range args {
		switch arg {
		case "--gui":
			gui = true
		case "--warm":
			warm = true
		default:
			rest = append(rest, arg)
		}
	}
	if len(rest) != 1 || (warm && gui) {
		return fmt.Errorf("shim: want [--gui] INSTANCE or --warm LAYER, not %q", args)
	}
	if warm {
		return e.RunWarmShim(ctx, rest[0])
	}
	return e.RunShim(ctx, rest[0], gui)
}

// spawnShim starts a shim with args, detached from this process, so the VM
// outlives the command that started it. The returned channel yields once,
// when the shim exits.
func (e *Engine) spawnShim(args []string, log *os.File) (<-chan error, error) {
	argv := e.ShimCommand
	if len(argv) == 0 {
		argv = []string{e.Exe, "--root", e.Root, "--driver", e.Driver.Name(), "shim"}
	}
	cmd := exec.Command(argv[0], append(slices.Clone(argv[1:]), args...)...)
	cmd.Stdout, cmd.Stderr = log, log
	detach(cmd)
	if err := cmd.Start(); err != nil {
		return nil, fmt.Errorf("start shim: %w", err)
	}
	exited := make(chan error, 1)
	go func() { exited <- cmd.Wait() }()
	return exited, nil
}

// shimClient is another process's handle on a running shim.
type shimClient struct {
	state shimState
	http  *http.Client
}

// shim finds a running instance's shim. It fails when the instance is not
// running.
func (e *Engine) shim(id string) (*shimClient, error) {
	return openShim(filepath.Join(e.instanceDir(id), shimStateName))
}

// openShim finds the shim published at statePath.
func openShim(statePath string) (*shimClient, error) {
	var state shimState
	if err := fsutil.ReadJSON(statePath, &state); err != nil {
		return nil, err
	}
	if !processAlive(state.PID) {
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
	deadline := time.Now().Add(15 * time.Second)
	for processAlive(c.state.PID) && time.Now().Before(deadline) {
		time.Sleep(50 * time.Millisecond)
	}
	if msg := strings.TrimSpace(string(body)); msg != "" {
		return errors.New(msg)
	}
	return nil
}

func (c *shimClient) dial(ctx context.Context, port uint32) (net.Conn, error) {
	var d net.Dialer
	conn, err := d.DialContext(ctx, "tcp", c.state.Addr)
	if err != nil {
		return nil, err
	}
	req, err := http.NewRequest(http.MethodGet, "http://shim/dial?port="+strconv.FormatUint(uint64(port), 10), nil)
	if err != nil {
		_ = conn.Close()
		return nil, err
	}
	req.Header.Set("Authorization", "Bearer "+c.state.Token)
	req.Header.Set("Connection", "Upgrade")
	req.Header.Set("Upgrade", "disco-vm-dial")
	if deadline, ok := ctx.Deadline(); ok {
		_ = conn.SetDeadline(deadline)
	}
	if err := req.Write(conn); err != nil {
		_ = conn.Close()
		return nil, err
	}
	reader := bufio.NewReader(conn)
	resp, err := http.ReadResponse(reader, req)
	if err != nil {
		_ = conn.Close()
		return nil, err
	}
	if resp.StatusCode != http.StatusSwitchingProtocols {
		msg, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
		_ = conn.Close()
		return nil, fmt.Errorf("dial port %d: %s", port, strings.TrimSpace(string(msg)))
	}
	_ = conn.SetDeadline(time.Time{})
	return &bufConn{Conn: conn, r: reader}, nil
}
