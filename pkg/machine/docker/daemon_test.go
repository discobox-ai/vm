package docker

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/discobox-ai/vm/pkg/guest"
	"github.com/discobox-ai/vm/pkg/machine"
)

// relayEnv makes this test binary `disco-vm pipe` when the agent runs it, so
// GuestDaemon's relay is a real process the agent starts and kills.
const relayEnv = "DISCO_VM_DOCKER_TEST_RELAY"

func TestMain(m *testing.M) {
	if os.Getenv(relayEnv) != "" && len(os.Args) == 3 && os.Args[1] == "pipe" {
		os.Exit(testRelay(os.Args[2]))
	}
	_ = os.Setenv(relayEnv, "1")
	os.Exit(m.Run())
}

// testRelay is `disco-vm pipe address`, and two addresses of its own: hang,
// a relay that never gets ready, and fail, one that fails once ready.
func testRelay(address string) int {
	switch address {
	case "hang":
		// Killed by the test; a bare select would be a deadlock once stdin
		// ends.
		time.Sleep(time.Hour)
		return 0
	case "fail":
		_, _ = os.Stdout.Write([]byte{guest.PipeReady})
		fmt.Fprintln(os.Stderr, "relay broke")
		return 3
	}
	if err := guest.Pipe(context.Background(), address, os.Stdin, os.Stdout); err != nil {
		fmt.Fprintln(os.Stderr, err)
		return 1
	}
	return 0
}

// fakeDaemon is enough of the Engine API to drive both of a driver's request
// paths: /info, and an exec whose hijacked stream writes guest.PipeReady,
// reads its stdin to EOF, then echoes it and "bye" before closing. /slow
// answers only once its request is gone. It counts connections still open.
type fakeDaemon struct {
	addr string
	open atomic.Int64
}

func startFakeDaemon(t *testing.T) *fakeDaemon {
	t.Helper()
	l, err := (&net.ListenConfig{}).Listen(t.Context(), "tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	fd := &fakeDaemon{addr: l.Addr().String()}
	mux := http.NewServeMux()
	mux.HandleFunc("GET /info", func(w http.ResponseWriter, _ *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]any{"OSType": "linux", "Architecture": "x86_64", "CgroupVersion": "2"})
	})
	mux.HandleFunc("GET /slow", func(_ http.ResponseWriter, r *http.Request) { <-r.Context().Done() })
	mux.HandleFunc("POST /containers/{id}/exec", func(w http.ResponseWriter, _ *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]string{"Id": "e1"})
	})
	mux.HandleFunc("GET /exec/e1/json", func(w http.ResponseWriter, _ *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]any{"Running": false, "ExitCode": 0})
	})
	mux.HandleFunc("POST /exec/e1/start", func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.Copy(io.Discard, r.Body)
		conn, rw, err := http.NewResponseController(w).Hijack()
		if err != nil {
			return
		}
		defer conn.Close()
		_, _ = rw.WriteString("HTTP/1.1 101 UPGRADED\r\nContent-Type: application/vnd.docker.raw-stream\r\nConnection: Upgrade\r\nUpgrade: tcp\r\n\r\n")
		_, _ = rw.Write(frame(1, []byte{guest.PipeReady}))
		_ = rw.Flush()
		in, _ := io.ReadAll(rw)
		_, _ = rw.Write(frame(1, in))
		_, _ = rw.Write(frame(2, []byte("not stdout")))
		_, _ = rw.Write(frame(1, []byte("bye")))
		_ = rw.Flush()
	})
	srv := &http.Server{Handler: mux, ReadHeaderTimeout: 5 * time.Second, ConnState: func(_ net.Conn, s http.ConnState) {
		switch s {
		case http.StateNew:
			fd.open.Add(1)
		case http.StateClosed, http.StateHijacked:
			fd.open.Add(-1)
		}
	}}
	go func() { _ = srv.Serve(l) }()
	t.Cleanup(func() { _ = srv.Close() })
	return fd
}

// frame is one frame of Docker's multiplexed stream.
func frame(stream byte, p []byte) []byte {
	n := len(p)
	return append([]byte{stream, 0, 0, 0, byte(n >> 24), byte(n >> 16), byte(n >> 8), byte(n)}, p...)
}

// waitOpen counts the daemon's connections, waiting for it to settle at
// want, since a connection a relay held closes on the daemon's side a moment
// after the relay is killed.
func (fd *fakeDaemon) waitOpen(t *testing.T, want int64) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for fd.open.Load() != want {
		if time.Now().After(deadline) {
			t.Fatalf("the daemon has %d connections open, want %d", fd.open.Load(), want)
		}
		time.Sleep(10 * time.Millisecond)
	}
}

// startTestAgent is a real disco-vm agent on loopback, as the fake driver
// runs it, and a client of it: the guest operations a host VM's are.
func startTestAgent(t *testing.T) *guest.Client {
	t.Helper()
	l, err := guest.Listen("tcp:127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	server := &guest.Server{Version: "test", Root: t.TempDir(), Fake: true, Exit: func(int) {}}
	go func() { _ = server.Serve(l) }()
	t.Cleanup(func() { _ = l.Close() })
	client := guest.NewClient(func(ctx context.Context) (net.Conn, error) {
		var d net.Dialer
		return d.DialContext(ctx, "tcp", l.Addr().String())
	})
	ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
	defer cancel()
	if err := client.WaitReady(ctx, nil); err != nil {
		t.Fatal(err)
	}
	return client
}

func testAgentPath(t *testing.T) string {
	t.Helper()
	exe, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	return exe
}

// TestDaemonRequests drives both request paths, an ordinary request and a
// hijacked exec stream with its half-close, against the daemon reached
// natively (the configured host) and through a guest's agent; the guest's leave no
// connection to the daemon, which is to say no relay, once each is done.
func TestDaemonRequests(t *testing.T) {
	agent := startTestAgent(t)
	daemons := map[string]func(t *testing.T, fd *fakeDaemon) Daemon{
		"native": func(t *testing.T, fd *fakeDaemon) Daemon {
			t.Helper()
			return HostDaemon("tcp://"+fd.addr, machine.Config{}.Dialer())
		},
		"guest": func(t *testing.T, fd *fakeDaemon) Daemon {
			t.Helper()
			return GuestDaemon(agent, testAgentPath(t), "tcp:"+fd.addr)
		},
	}
	for name, daemon := range daemons {
		t.Run(name, func(t *testing.T) {
			fd := startFakeDaemon(t)
			d := daemon(t, fd)
			driver, err := NewWithDaemon(machine.Config{}, d)
			if err != nil {
				t.Fatal(err)
			}
			a := driver.api
			ctx, cancel := context.WithTimeout(t.Context(), 30*time.Second)
			defer cancel()
			in, err := a.info(ctx)
			if err != nil || in.OSType != "linux" {
				t.Fatalf("info: %+v, %v", in, err)
			}
			if !d.KeepIdle {
				fd.waitOpen(t, 0)
			}

			conn, err := dialExec(ctx, a, "c1", []string{"pipe"}, guestAddr{"c1", 7300})
			if err != nil {
				t.Fatal(err)
			}
			if _, err := io.WriteString(conn, "ping"); err != nil {
				t.Fatal(err)
			}
			// The half-close reaches the daemon, which answers after it.
			if err := conn.(interface{ CloseWrite() error }).CloseWrite(); err != nil {
				t.Fatal(err)
			}
			out, err := io.ReadAll(conn)
			if err != nil || string(out) != "pingbye" {
				t.Fatalf("read %q, %v; want pingbye", out, err)
			}
			if got := string(conn.(*execConn).stderrBytes()); got != "not stdout" {
				t.Errorf("stderr %q", got)
			}
			_ = conn.Close()
			if !d.KeepIdle {
				fd.waitOpen(t, 0)
			}
		})
	}
}

// TestGuestDaemonNotReady fails a dial whose relay cannot reach the daemon,
// with the relay's exit status and stderr, rather than handing back a
// connection that reads EOF.
func TestGuestDaemonNotReady(t *testing.T) {
	l, err := (&net.ListenConfig{}).Listen(t.Context(), "tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	gone := l.Addr().String()
	_ = l.Close()
	d := GuestDaemon(startTestAgent(t), testAgentPath(t), "tcp:"+gone)
	conn, err := d.Dial(t.Context())
	if err == nil {
		_ = conn.Close()
		t.Fatal("dialed a daemon that is not there")
	}
	if !strings.Contains(err.Error(), "ended before it was ready: exited 1") || !strings.Contains(err.Error(), gone) {
		t.Fatalf("not the relay's failure: %v", err)
	}
}

// TestGuestDaemonFailsAfterReady reads a relay that fails once connected as
// an error with its exit status, not as the daemon's EOF.
func TestGuestDaemonFailsAfterReady(t *testing.T) {
	d := GuestDaemon(startTestAgent(t), testAgentPath(t), "fail")
	conn, err := d.Dial(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	_, err = io.ReadAll(conn)
	if err == nil || !strings.Contains(err.Error(), "exited 3: relay broke") {
		t.Fatalf("read after the relay failed: %v", err)
	}
}

// execCounter is a guest that counts the Execs it starts and those closed.
type execCounter struct {
	machine.Guest
	calls, closed atomic.Int64
}

func (g *execCounter) Exec(ctx context.Context, req machine.ExecRequest) (machine.Process, error) {
	g.calls.Add(1)
	p, err := g.Guest.Exec(ctx, req)
	if err != nil {
		g.closed.Add(1)
		return nil, err
	}
	return &countedProcess{Process: p, g: g}, nil
}

type countedProcess struct {
	machine.Process
	g    *execCounter
	once sync.Once
}

func (p *countedProcess) Close() error {
	p.once.Do(func() { p.g.closed.Add(1) })
	return p.Process.Close()
}

// TestGuestDaemonCanceled stops a dial canceled before it starts (with no
// relay), while its relay is getting ready, and a request while it waits on
// the daemon, killing that request's relay.
func TestGuestDaemonCanceled(t *testing.T) {
	agent := startTestAgent(t)
	exe := testAgentPath(t)

	t.Run("before", func(t *testing.T) {
		g := &execCounter{Guest: agent}
		ctx, cancel := context.WithCancel(t.Context())
		cancel()
		if _, err := GuestDaemon(g, exe, "hang").Dial(ctx); !errors.Is(err, context.Canceled) {
			t.Fatalf("dial: %v", err)
		}
		if n := g.calls.Load(); n != 0 {
			t.Fatalf("%d relays started", n)
		}
	})

	t.Run("ready", func(t *testing.T) {
		ctx, cancel := context.WithTimeout(t.Context(), 500*time.Millisecond)
		defer cancel()
		started := time.Now()
		if _, err := GuestDaemon(agent, exe, "hang").Dial(ctx); !errors.Is(err, context.DeadlineExceeded) {
			t.Fatalf("dial: %v", err)
		}
		if waited := time.Since(started); waited > 10*time.Second {
			t.Fatalf("the canceled dial took %s", waited)
		}
	})

	// net/http dials on a context detached from the request, so a request
	// canceled while its relay starts leaves the dial to relayReady, which
	// ends the relay that never got ready.
	t.Run("request-while-ready", func(t *testing.T) {
		defer func(d time.Duration) { relayReady = d }(relayReady)
		relayReady = time.Second
		g := &execCounter{Guest: agent}
		a := newAPI(GuestDaemon(g, exe, "hang"))
		ctx, cancel := context.WithTimeout(t.Context(), 100*time.Millisecond)
		defer cancel()
		resp, err := a.do(ctx, http.MethodGet, "/info", nil, nil, "")
		if err == nil {
			_ = resp.Body.Close()
		}
		if !errors.Is(err, context.DeadlineExceeded) {
			t.Fatalf("request: %v", err)
		}
		deadline := time.Now().Add(10 * time.Second)
		for g.calls.Load() == 0 || g.closed.Load() != g.calls.Load() {
			if time.Now().After(deadline) {
				t.Fatalf("%d relays started, %d closed", g.calls.Load(), g.closed.Load())
			}
			time.Sleep(10 * time.Millisecond)
		}
	})

	t.Run("request", func(t *testing.T) {
		fd := startFakeDaemon(t)
		a := newAPI(GuestDaemon(agent, exe, "tcp:"+fd.addr))
		ctx, cancel := context.WithCancel(t.Context())
		go func() {
			// Cancel once the request waits on the daemon.
			for fd.open.Load() == 0 && ctx.Err() == nil {
				time.Sleep(10 * time.Millisecond)
			}
			cancel()
		}()
		resp, err := a.do(ctx, http.MethodGet, "/slow", nil, nil, "")
		if err == nil {
			_ = resp.Body.Close()
			t.Fatal("the canceled request answered")
		}
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("request: %v", err)
		}
		fd.waitOpen(t, 0)
	})
}

// TestProcessConnDeadline times out a read of a relay that sends nothing,
// as the hijacked exec's handshake relies on.
func TestProcessConnDeadline(t *testing.T) {
	fd := startFakeDaemon(t)
	conn, err := GuestDaemon(startTestAgent(t), testAgentPath(t), "tcp:"+fd.addr).Dial(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	_ = conn.SetReadDeadline(time.Now().Add(100 * time.Millisecond))
	if _, err := bufio.NewReader(conn).ReadByte(); !errors.Is(err, os.ErrDeadlineExceeded) {
		t.Fatalf("read past its deadline: %v", err)
	}
}
