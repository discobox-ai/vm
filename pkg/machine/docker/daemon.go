package docker

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"sync"
	"time"

	"github.com/discobox-ai/vm/pkg/guest"
	"github.com/discobox-ai/vm/pkg/machine"
)

// relayReady bounds starting a relay and its getting ready. net/http dials
// on a context detached from the request, so a request canceled while its
// relay starts does not stop the dial: this does.
var relayReady = 30 * time.Second

// HostDaemonAddress is where a Docker host VM's daemon listens, in
// `disco-vm pipe`'s form (ADR 26-11-04-412 §3).
const HostDaemonAddress = "unix:/run/docker.sock"

// GuestDaemon is the daemon listening at address (a tcp: or unix: address in
// `disco-vm pipe`'s form, such as HostDaemonAddress) inside a running guest,
// reached through the guest's agent: each connection is an Exec of
// `agent pipe address`, where agent is the path of the guest's own disco-vm,
// carried over the agent's port. It opens no port and needs nothing of the
// guest but its agent; it is privileged control of the daemon, never to be
// handed to an inner guest.
//
// A connection is ready once the relay has reached the daemon: a relay that
// fails first is a Dial error with its exit status and stderr, and one not
// ready within relayReady is killed, whatever ctx allows. Closing the
// connection ends the relay's Exec session, so the agent kills and reaps it,
// and its requests keep no connection idle, so neither outlives the request
// (ADR 26-11-04-412 §3).
func GuestDaemon(g machine.Guest, agent, address string) Daemon {
	addr := relayAddr(address)
	return Daemon{
		Name: fmt.Sprintf("%s in the guest", address),
		Dial: func(ctx context.Context) (net.Conn, error) {
			return dialProcess(ctx, g, []string{agent, "pipe", address}, addr)
		},
	}
}

// dialProcess runs a `disco-vm pipe` relay through the guest's Exec and
// returns its stdin and stdout as a net.Conn, once the relay has written
// guest.PipeReady. ctx, and relayReady at most, bound starting it and its
// getting ready, not the conn.
func dialProcess(ctx context.Context, g machine.Guest, argv []string, addr net.Addr) (net.Conn, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	ctx, cancel := context.WithTimeout(ctx, relayReady)
	defer cancel()
	p, err := g.Exec(ctx, machine.ExecRequest{Argv: argv})
	if err != nil {
		return nil, fmt.Errorf("docker: relay to %s: %w", addr, err)
	}
	c := newProcessConn(p, addr)
	stop := context.AfterFunc(ctx, func() { _ = c.out.SetReadDeadline(time.Unix(1, 0)) })
	var first [1]byte
	_, err = io.ReadFull(c.out, first[:])
	if !stop() {
		err = errors.Join(err, ctx.Err())
	}
	_ = c.out.SetReadDeadline(time.Time{})
	switch {
	case err != nil && ctx.Err() != nil:
		_ = c.Close()
		return nil, ctx.Err()
	case err != nil:
		// The relay's stdout ends only once its process has, so its exit
		// is known.
		_ = c.Close()
		exit := c.exitErr()
		if exit == nil {
			exit = errors.New("exited 0")
		}
		return nil, fmt.Errorf("docker: relay to %s ended before it was ready: %w", addr, exit)
	case first[0] != guest.PipeReady:
		_ = c.Close()
		return nil, fmt.Errorf("docker: relay to %s answered %q before it was ready", addr, first[:])
	}
	return c, nil
}

// processConn is a running Exec as a net.Conn: writes are its stdin, reads its
// stdout, and CloseWrite ends its stdin. Each direction is a net.Pipe, so the
// conn has deadlines, which the process does not. Its stderr is kept (the
// first 4 KiB) to explain a relay that failed.
type processConn struct {
	p    machine.Process
	addr net.Addr

	// out is read from; Wait writes the process's stdout to outW.
	out, outW net.Conn
	// in is written to; a goroutine copies inR to the process's stdin.
	in, inR net.Conn

	errMu  sync.Mutex
	stderr bytes.Buffer

	// waited is closed once Wait has returned code and waitErr.
	waited  chan struct{}
	code    int
	waitErr error

	copies    sync.WaitGroup
	closeOnce sync.Once
}

func newProcessConn(p machine.Process, addr net.Addr) *processConn {
	c := &processConn{p: p, addr: addr, waited: make(chan struct{})}
	c.out, c.outW = net.Pipe()
	c.in, c.inR = net.Pipe()
	c.copies.Go(func() {
		defer close(c.waited)
		c.code, c.waitErr = p.Wait(c.outW, stderrWriter{c})
		_ = c.outW.Close()
	})
	c.copies.Go(func() {
		// in ends by CloseWrite, or by Close, which closes the process too.
		if _, err := io.Copy(processStdin{p}, c.inR); err == nil {
			_ = p.CloseStdin()
		}
		_ = c.inR.Close()
	})
	return c
}

// Read reads the process's stdout. Once it ends, a process that failed is an
// error rather than EOF.
func (c *processConn) Read(b []byte) (int, error) {
	n, err := c.out.Read(b)
	if errors.Is(err, io.EOF) {
		<-c.waited
		if exit := c.exitErr(); exit != nil {
			return n, exit
		}
	}
	return n, err
}

func (c *processConn) Write(b []byte) (int, error) { return c.in.Write(b) }

// CloseWrite ends the process's stdin once what was written is sent, which
// `disco-vm pipe` passes on to the daemon as a half-close; its stdout is
// still read.
func (c *processConn) CloseWrite() error { return c.in.Close() }

// Close ends the Exec session, which the agent answers by killing the
// process if it still runs and reaping it, and returns once nothing of the
// conn is left running here.
func (c *processConn) Close() error {
	c.closeOnce.Do(func() {
		_ = c.in.Close()
		_ = c.out.Close()
		_ = c.p.Close()
		c.copies.Wait()
	})
	return nil
}

// exitErr says why the process ended, or nil if it exited 0. It is called
// only once Wait has returned.
func (c *processConn) exitErr() error {
	c.errMu.Lock()
	stderr := bytes.TrimSpace(c.stderr.Bytes())
	c.errMu.Unlock()
	switch {
	case c.waitErr != nil:
		return fmt.Errorf("%w: %s", c.waitErr, stderr)
	case c.code != 0:
		return fmt.Errorf("exited %d: %s", c.code, stderr)
	}
	return nil
}

func (c *processConn) LocalAddr() net.Addr  { return c.addr }
func (c *processConn) RemoteAddr() net.Addr { return c.addr }

func (c *processConn) SetDeadline(t time.Time) error {
	return errors.Join(c.out.SetReadDeadline(t), c.in.SetWriteDeadline(t))
}
func (c *processConn) SetReadDeadline(t time.Time) error  { return c.out.SetReadDeadline(t) }
func (c *processConn) SetWriteDeadline(t time.Time) error { return c.in.SetWriteDeadline(t) }

// processStdin writes to a process's stdin.
type processStdin struct{ p machine.Process }

func (w processStdin) Write(b []byte) (int, error) { return w.p.Write(b) }

// stderrWriter keeps the first 4 KiB of a process's stderr.
type stderrWriter struct{ c *processConn }

func (w stderrWriter) Write(b []byte) (int, error) {
	w.c.errMu.Lock()
	defer w.c.errMu.Unlock()
	return (&limited{&w.c.stderr, 4096}).Write(b)
}

// relayAddr names a daemon address in a guest.
type relayAddr string

func (relayAddr) Network() string  { return "guest" }
func (a relayAddr) String() string { return string(a) }
