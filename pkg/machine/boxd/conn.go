package boxd

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"sync"
	"time"

	"google.golang.org/grpc"

	"github.com/discobox-ai/vm/pkg/guest"
	"github.com/discobox-ai/vm/pkg/machine/boxd/internal/boxdapi"
)

// execStream is the boxd Exec call as the conn uses it.
type execStream = grpc.BidiStreamingClient[boxdapi.ExecChunk, boxdapi.ExecChunk]

// maxChunk keeps each stdin message well under gRPC's 4 MiB message cap.
const maxChunk = 256 << 10

// dialExec runs command in the VM over boxd's authenticated Exec stream and
// returns its stdin and stdout as a net.Conn. The command is a `disco-vm pipe`
// relay, which writes guest.PipeReady once it has reached the guest port, so
// a relay that failed is an error here rather than a conn that reads EOF.
//
// The stream is not bound to ctx, which only bounds reaching the port: like
// any dialed conn it lives until it is closed.
func dialExec(ctx context.Context, api boxdapi.BoxdApiClient, vm, command string, addr net.Addr) (net.Conn, error) {
	streamCtx, cancel := context.WithCancel(context.Background())
	stream, err := api.Exec(streamCtx)
	if err != nil {
		cancel()
		return nil, err
	}
	if err := stream.Send(&boxdapi.ExecChunk{VmId: vm, Command: command}); err != nil {
		cancel()
		return nil, fmt.Errorf("boxd: exec: %w", err)
	}
	c := &execConn{
		stream: stream, cancel: cancel, addr: addr,
		chunks: make(chan []byte), done: make(chan struct{}),
	}
	c.rdl.init()
	go c.pump()

	var first []byte
	select {
	case data, ok := <-c.chunks:
		if !ok {
			_ = c.Close()
			return nil, c.relayFailed()
		}
		first = data
	case <-ctx.Done():
		_ = c.Close()
		return nil, ctx.Err()
	}
	if first[0] != guest.PipeReady {
		_ = c.Close()
		return nil, fmt.Errorf("boxd: relay to %s answered %q before it was ready", addr, first[:min(len(first), 64)])
	}
	c.pending = first[1:]
	return c, nil
}

type execConn struct {
	stream execStream
	cancel context.CancelFunc
	addr   net.Addr

	// chunks carries stdout; pump closes it at the end of the stream, after
	// setting recvErr.
	chunks  chan []byte
	recvErr error
	pending []byte
	rdl     deadline

	errMu  sync.Mutex
	stderr bytes.Buffer
	exit   int32

	sendMu sync.Mutex
	once   sync.Once
	done   chan struct{}
}

func (c *execConn) pump() {
	defer close(c.chunks)
	for {
		msg, err := c.stream.Recv()
		if err != nil {
			c.recvErr = err
			return
		}
		if msg.GetIsStderr() {
			c.errMu.Lock()
			if c.stderr.Len() < 4096 {
				c.stderr.Write(msg.GetData())
			}
			c.errMu.Unlock()
			continue
		}
		if msg.GetExitCode() != 0 {
			c.errMu.Lock()
			c.exit = msg.GetExitCode()
			c.errMu.Unlock()
		}
		if len(msg.GetData()) == 0 {
			continue
		}
		select {
		case c.chunks <- msg.GetData():
		case <-c.done:
			c.recvErr = net.ErrClosed
			return
		}
	}
}

// relayFailed explains a stream that ended before the relay was ready.
func (c *execConn) relayFailed() error {
	c.errMu.Lock()
	defer c.errMu.Unlock()
	msg := bytes.TrimSpace(c.stderr.Bytes())
	if c.recvErr != nil && !errors.Is(c.recvErr, io.EOF) {
		return fmt.Errorf("boxd: relay to %s: %w", c.addr, c.recvErr)
	}
	return fmt.Errorf("boxd: relay to %s exited %d: %s", c.addr, c.exit, msg)
}

func (c *execConn) Read(p []byte) (int, error) {
	if len(c.pending) == 0 {
		select {
		case data, ok := <-c.chunks:
			if !ok {
				return 0, c.recvErr
			}
			c.pending = data
		case <-c.rdl.wait():
			return 0, os.ErrDeadlineExceeded
		case <-c.done:
			return 0, net.ErrClosed
		}
	}
	n := copy(p, c.pending)
	c.pending = c.pending[n:]
	return n, nil
}

func (c *execConn) Write(p []byte) (int, error) {
	c.sendMu.Lock()
	defer c.sendMu.Unlock()
	select {
	case <-c.done:
		return 0, net.ErrClosed
	default:
	}
	written := 0
	for len(p) > 0 {
		n := min(len(p), maxChunk)
		// The message keeps the slice until it is sent, and Send copies
		// before it returns, so p is not retained.
		if err := c.stream.Send(&boxdapi.ExecChunk{Data: p[:n], Stdin: true}); err != nil {
			return written, fmt.Errorf("boxd: relay to %s: %w", c.addr, err)
		}
		written += n
		p = p[n:]
	}
	return written, nil
}

// CloseWrite ends the relay's stdin, which it passes on to the guest port as
// a half-close.
func (c *execConn) CloseWrite() error {
	c.sendMu.Lock()
	defer c.sendMu.Unlock()
	return c.stream.CloseSend()
}

// Close cancels the call, which kills the relay in the guest.
func (c *execConn) Close() error {
	c.once.Do(func() {
		close(c.done)
		c.cancel()
	})
	return nil
}

func (c *execConn) LocalAddr() net.Addr  { return c.addr }
func (c *execConn) RemoteAddr() net.Addr { return c.addr }

func (c *execConn) SetDeadline(t time.Time) error { return c.SetReadDeadline(t) }

func (c *execConn) SetReadDeadline(t time.Time) error {
	c.rdl.set(t)
	return nil
}

// SetWriteDeadline is accepted and ignored: a write blocks only on gRPC flow
// control, and Close always unblocks it.
func (c *execConn) SetWriteDeadline(time.Time) error { return nil }

// deadline is a resettable timer whose channel is closed once it passes, as
// net.Pipe implements its deadlines.
type deadline struct {
	mu     sync.Mutex
	timer  *time.Timer
	cancel chan struct{}
}

func (d *deadline) init() { d.cancel = make(chan struct{}) }

func (d *deadline) set(t time.Time) {
	d.mu.Lock()
	defer d.mu.Unlock()
	if d.timer != nil && !d.timer.Stop() {
		<-d.cancel // the timer fired; wait for it to finish closing
	}
	d.timer = nil
	select {
	case <-d.cancel:
		d.cancel = make(chan struct{})
	default:
	}
	if t.IsZero() {
		return
	}
	if dur := time.Until(t); dur > 0 {
		cancel := d.cancel
		d.timer = time.AfterFunc(dur, func() { close(cancel) })
		return
	}
	close(d.cancel)
}

func (d *deadline) wait() chan struct{} {
	d.mu.Lock()
	defer d.mu.Unlock()
	return d.cancel
}

// vmAddr names a guest port on a boxd machine.
type vmAddr struct {
	vm   string
	port uint32
}

func (vmAddr) Network() string  { return "boxd" }
func (a vmAddr) String() string { return fmt.Sprintf("%s:%d", a.vm, a.port) }
