package docker

import (
	"bufio"
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"sync"
	"time"

	"github.com/discobox-ai/vm/pkg/guest"
)

// dialExec runs a `disco-vm pipe` relay in the container through docker exec
// and returns its stdin and stdout as a net.Conn. The relay writes
// guest.PipeReady once it has reached the guest port, so a relay that failed
// is an error here rather than a conn that reads EOF.
//
// The conn is not bound to ctx, which only bounds reaching the port.
func dialExec(ctx context.Context, a *api, container string, cmd []string, addr net.Addr) (net.Conn, error) {
	raw, br, execID, err := a.exec(ctx, container, cmd)
	if err != nil {
		return nil, err
	}
	c := &execConn{Conn: raw, r: br, addr: addr}
	stop := context.AfterFunc(ctx, func() { _ = raw.SetReadDeadline(time.Unix(1, 0)) })
	var first [1]byte
	_, err = io.ReadFull(c, first[:])
	if !stop() {
		err = errors.Join(err, ctx.Err())
	}
	_ = raw.SetReadDeadline(time.Time{})
	if err != nil {
		_ = raw.Close()
		if ctx.Err() != nil {
			return nil, ctx.Err()
		}
		code, _, _ := a.execExitCode(context.Background(), execID)
		return nil, fmt.Errorf("docker: relay to %s exited %d: %s", addr, code, bytes.TrimSpace(c.stderrBytes()))
	}
	if first[0] != guest.PipeReady {
		_ = raw.Close()
		return nil, fmt.Errorf("docker: relay to %s answered %q before it was ready", addr, first[:])
	}
	return c, nil
}

// execConn is a hijacked exec stream: writes are the relay's stdin, and reads
// are its stdout, taken out of Docker's multiplexed frames. Its stderr is kept
// (the first 4 KiB) to explain a relay that failed.
type execConn struct {
	net.Conn
	r    *bufio.Reader
	addr net.Addr

	// left is what remains of the stdout frame being read.
	left int64

	errMu  sync.Mutex
	stderr bytes.Buffer
}

func (c *execConn) Read(p []byte) (int, error) {
	for c.left == 0 {
		var hdr [8]byte
		if _, err := io.ReadFull(c.r, hdr[:]); err != nil {
			if errors.Is(err, io.ErrUnexpectedEOF) {
				err = io.EOF
			}
			return 0, err
		}
		n := int64(hdr[4])<<24 | int64(hdr[5])<<16 | int64(hdr[6])<<8 | int64(hdr[7])
		if hdr[0] != 2 {
			c.left = n
			continue
		}
		c.errMu.Lock()
		_, err := io.CopyN(&limited{&c.stderr, 4096}, c.r, n)
		c.errMu.Unlock()
		if err != nil {
			return 0, err
		}
	}
	if int64(len(p)) > c.left {
		p = p[:c.left]
	}
	n, err := c.r.Read(p)
	c.left -= int64(n)
	return n, err
}

func (c *execConn) stderrBytes() []byte {
	c.errMu.Lock()
	defer c.errMu.Unlock()
	return bytes.Clone(c.stderr.Bytes())
}

// CloseWrite ends the relay's stdin, which it passes on to the guest port as
// a half-close.
func (c *execConn) CloseWrite() error {
	if half, ok := c.Conn.(interface{ CloseWrite() error }); ok {
		return half.CloseWrite()
	}
	return errors.New("docker: the daemon connection cannot half-close")
}

func (c *execConn) LocalAddr() net.Addr  { return c.addr }
func (c *execConn) RemoteAddr() net.Addr { return c.addr }

// limited keeps the first n bytes written and drops the rest.
type limited struct {
	w *bytes.Buffer
	n int
}

func (l *limited) Write(p []byte) (int, error) {
	if room := l.n - l.w.Len(); room > 0 {
		l.w.Write(p[:min(len(p), room)])
	}
	return len(p), nil
}

// guestAddr names a guest port in a container.
type guestAddr struct {
	container string
	port      uint32
}

func (guestAddr) Network() string  { return "docker" }
func (a guestAddr) String() string { return fmt.Sprintf("%s:%d", a.container, a.port) }
