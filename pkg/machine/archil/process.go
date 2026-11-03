package archil

import (
	"bytes"
	"context"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"sync"
	"sync/atomic"
	"time"

	"github.com/coder/websocket"

	"github.com/discobox-ai/vm/pkg/machine"
)

// Archil's process API is a WebSocket per process, at a short-lived URL the
// control plane signs (POST /api/sandboxes/{sid}/connections). Only Archil's
// MIT-licensed TypeScript SDK implements it (npm `disk`); this is a port of
// what it does:
//
//   - The client starts a process with a text frame, {"type": "start",
//     "command", "cwd", "env", "terminal", "timeout_seconds"}, and is answered
//     {"type": "started", "process_id"}. The command is one string, run by a
//     shell.
//   - Binary frames from the client are the process's stdin; a text frame
//     {"type": "close_stdin"} ends it.
//   - Binary frames from the server are output: a byte, 1 for stdout and 2
//     for stderr, then the offset of its bytes in the process's output, both
//     streams counted together, as a big-endian uint64, then the bytes.
//   - {"type": "exit", "status", "exit_code", "exit_reason", "cursor"} ends
//     the process.
//   - Kill and resize go over a connection of their own: {"type": "kill",
//     "process_id"} answered {"type": "killed"}, and {"type": "resize",
//     "process_id", "cols", "rows"} answered {"type": "resized"}. An error is
//     {"type": "error", "error", "message"}.

// stdinChunk is the largest stdin frame the SDK sends.
const stdinChunk = 1 << 20

// startRequest starts a process.
type startRequest struct {
	Type     string            `json:"type"`
	Command  string            `json:"command"`
	Cwd      string            `json:"cwd,omitempty"`
	Env      map[string]string `json:"env"`
	Terminal any               `json:"terminal,omitempty"`
}

// event is a text frame from the server.
type event struct {
	Type       string `json:"type"`
	ProcessID  string `json:"process_id"`
	Status     string `json:"status"`
	ExitCode   *int   `json:"exit_code"`
	ExitReason string `json:"exit_reason"`
	Error      string `json:"error"`
	Message    string `json:"message"`
	// Cursor is the end of the process's output, on exit.
	Cursor uint64 `json:"cursor"`
}

// process is a running Archil process: its stdin, its stdout and stderr, and
// its exit.
type process struct {
	api     *api
	sandbox string
	conn    *websocket.Conn
	id      string

	writeMu sync.Mutex

	stdout, stderr *pipeBuffer
	done           chan struct{}
	exit           event
	err            error
	// lost counts the output Archil dropped: it keeps a bounded buffer of
	// a process's output and skips past what a connection did not take in
	// time, which a frame past the cursor, or an exit cursor past it, shows.
	lost   uint64
	exact  bool
	exited atomic.Bool
}

// dialProcess opens the sandbox's process API.
func (a *api) dialProcess(ctx context.Context, sandbox string) (*websocket.Conn, error) {
	u, err := a.connection(ctx, sandbox)
	if err != nil {
		return nil, err
	}
	conn, resp, err := websocket.Dial(ctx, u, &websocket.DialOptions{HTTPClient: &http.Client{Transport: a.http.Transport}})
	if resp != nil && resp.Body != nil {
		_ = resp.Body.Close()
	}
	if err != nil {
		return nil, fmt.Errorf("archil: the process API of %s: %w", sandbox, err)
	}
	conn.SetReadLimit(-1)
	return conn, nil
}

// startProcess starts command in the sandbox, run by its shell. terminal is
// nil for a process with pipes, or {cols, rows} for one with a TTY. The
// process outlives ctx, which bounds only starting it.
//
// An exact process ends at the first output Archil dropped, with that as its
// error, for a reader that asks for its output a chunk at a time and would
// otherwise wait forever for the bytes; any other reports it once it exits.
func (a *api) startProcess(ctx context.Context, sandbox, command, cwd string, env map[string]string, terminal any, exact bool) (p *process, err error) {
	started := time.Now()
	defer func() { machine.DebugCall(started, err, "archil process %s", sandbox) }()
	conn, err := a.dialProcess(ctx, sandbox)
	if err != nil {
		return nil, err
	}
	if env == nil {
		env = map[string]string{}
	}
	req, err := json.Marshal(startRequest{Type: "start", Command: command, Cwd: cwd, Env: env, Terminal: terminal})
	if err != nil {
		_ = conn.CloseNow()
		return nil, err
	}
	if err := conn.Write(ctx, websocket.MessageText, req); err != nil {
		_ = conn.CloseNow()
		return nil, fmt.Errorf("archil: start a process: %w", err)
	}
	for {
		typ, data, err := conn.Read(ctx)
		if err != nil {
			_ = conn.CloseNow()
			return nil, fmt.Errorf("archil: start a process: %w", err)
		}
		if typ != websocket.MessageText {
			continue
		}
		var ev event
		if err := json.Unmarshal(data, &ev); err != nil {
			_ = conn.CloseNow()
			return nil, fmt.Errorf("archil: start a process: %w", err)
		}
		switch ev.Type {
		case "started":
			p := &process{api: a, sandbox: sandbox, conn: conn, id: ev.ProcessID, stdout: newPipeBuffer(), stderr: newPipeBuffer(), done: make(chan struct{}), exact: exact}
			go p.read() //nolint:gosec // G118: the process outlives the ctx that started it, by design
			return p, nil
		case "error":
			_ = conn.CloseNow()
			return nil, fmt.Errorf("archil: start a process: %s: %s", ev.Error, ev.Message)
		}
	}
}

// read copies output until the process exits or the connection ends.
func (p *process) read() {
	defer close(p.done)
	defer p.stdout.close()
	defer p.stderr.close()
	// cursor is the end of the output read so far, both streams together,
	// as the SDK's SandboxProcess keeps it.
	var cursor uint64
	for {
		typ, data, err := p.conn.Read(context.Background())
		if err != nil {
			p.err = fmt.Errorf("archil: process %s: the connection ended before it exited: %w", p.id, err)
			return
		}
		if typ == websocket.MessageBinary {
			if len(data) < 9 || (data[0] != 1 && data[0] != 2) {
				p.err = errors.New("archil: an output frame that is not one")
				_ = p.conn.CloseNow()
				return
			}
			offset := binary.BigEndian.Uint64(data[1:9])
			payload := data[9:]
			// A frame resent from before the cursor is skipped, and its part
			// already read cut off, as the SDK does; a frame past it moves it.
			end := offset + uint64(len(payload))
			if end <= cursor {
				continue
			}
			if offset < cursor {
				payload = payload[cursor-offset:]
			} else if offset > cursor {
				p.lost += offset - cursor
				if p.exact {
					p.err = p.lostError()
					_ = p.conn.CloseNow()
					return
				}
			}
			cursor = end
			if data[0] == 1 {
				p.stdout.write(payload)
			} else {
				p.stderr.write(payload)
			}
			continue
		}
		var ev event
		if json.Unmarshal(data, &ev) != nil {
			continue
		}
		switch ev.Type {
		case "exit":
			p.exit = ev
			p.exited.Store(true)
			if ev.Cursor > cursor {
				p.lost += ev.Cursor - cursor
			}
			_ = p.conn.Close(websocket.StatusNormalClosure, "")
			return
		case "error":
			p.err = fmt.Errorf("archil: process %s: %s: %s", p.id, ev.Error, ev.Message)
			_ = p.conn.CloseNow()
			return
		}
	}
}

// Write sends bytes to the process's stdin.
func (p *process) Write(b []byte) (int, error) {
	p.writeMu.Lock()
	defer p.writeMu.Unlock()
	n := 0
	for len(b) > 0 {
		chunk := b[:min(len(b), stdinChunk)]
		if err := p.conn.Write(context.Background(), websocket.MessageBinary, chunk); err != nil {
			return n, err
		}
		n += len(chunk)
		b = b[len(chunk):]
	}
	return n, nil
}

// CloseStdin ends the process's stdin.
func (p *process) CloseStdin() error {
	p.writeMu.Lock()
	defer p.writeMu.Unlock()
	return p.conn.Write(context.Background(), websocket.MessageText, []byte(`{"type":"close_stdin"}`))
}

// control sends kill or resize on a connection of its own and waits for the
// answer.
func (p *process) control(ctx context.Context, req map[string]any, want string) error {
	conn, err := p.api.dialProcess(ctx, p.sandbox)
	if err != nil {
		return err
	}
	defer func() { _ = conn.CloseNow() }()
	raw, err := json.Marshal(req)
	if err != nil {
		return err
	}
	if err := conn.Write(ctx, websocket.MessageText, raw); err != nil {
		return err
	}
	_, data, err := conn.Read(ctx)
	if err != nil {
		return err
	}
	var ev event
	if err := json.Unmarshal(data, &ev); err != nil {
		return err
	}
	switch ev.Type {
	case want:
		return nil
	case "error":
		return fmt.Errorf("archil: %s: %s: %s", req["type"], ev.Error, ev.Message)
	}
	return fmt.Errorf("archil: %s answered %q", req["type"], ev.Type)
}

// Resize sets the terminal's size.
func (p *process) Resize(rows, cols uint16) error {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	return p.control(ctx, map[string]any{"type": "resize", "process_id": p.id, "cols": cols, "rows": rows}, "resized")
}

// Kill ends the process, and its connection. A process whose connection
// ended without its exit (a gap in an exact process's output, an error) may
// still run in the guest, so only an exit spares it the kill.
func (p *process) Kill() error {
	if p.exited.Load() {
		return nil
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	err := p.control(ctx, map[string]any{"type": "kill", "process_id": p.id}, "killed")
	_ = p.conn.CloseNow()
	p.stdout.abort()
	p.stderr.abort()
	return err
}

// Wait copies the process's output to stdout and stderr until it exits, and
// returns its exit code.
func (p *process) Wait(stdout, stderr io.Writer) (int, error) {
	if stdout == nil {
		stdout = io.Discard
	}
	if stderr == nil {
		stderr = io.Discard
	}
	var wg sync.WaitGroup
	wg.Add(2)
	// A writer that fails is read past, so the process's output never
	// waits on it.
	go func() { defer wg.Done(); _, _ = io.Copy(stdout, p.stdout); _, _ = io.Copy(io.Discard, p.stdout) }()
	go func() { defer wg.Done(); _, _ = io.Copy(stderr, p.stderr); _, _ = io.Copy(io.Discard, p.stderr) }()
	wg.Wait()
	return p.result()
}

// result waits for the process to end and returns its exit code. Its output
// must be read, or the process's reader waits on it.
func (p *process) result() (int, error) {
	<-p.done
	if p.err != nil {
		return -1, p.err
	}
	if p.lost > 0 {
		return -1, p.lostError()
	}
	if p.exit.ExitCode == nil {
		return -1, fmt.Errorf("archil: process %s ended %s: %s", p.id, p.exit.Status, p.exit.ExitReason)
	}
	return *p.exit.ExitCode, nil
}

func (p *process) lostError() error {
	return fmt.Errorf("archil: process %s: Archil dropped %d bytes of its output, which came faster than it was sent", p.id, p.lost)
}

// run runs command to its end, in cwd (the guest's default when empty), with
// stdin when it is not nil, and returns its exit code, stdout, and stderr.
func (a *api) run(ctx context.Context, sandbox, command, cwd string, stdin io.Reader) (int, []byte, []byte, error) {
	p, err := a.startProcess(ctx, sandbox, command, cwd, nil, nil, false)
	if err != nil {
		return -1, nil, nil, err
	}
	stop := context.AfterFunc(ctx, func() { _ = p.Kill() })
	defer stop()
	go func() {
		if stdin != nil {
			_, _ = io.Copy(p, stdin)
		}
		_ = p.CloseStdin()
	}()
	var out, errOut bytes.Buffer
	code, err := p.Wait(&out, &errOut)
	if err != nil && ctx.Err() != nil {
		err = ctx.Err()
	}
	return code, out.Bytes(), errOut.Bytes(), err
}

// pipeBuffer is output that arrives as frames and is read as a stream. It
// holds at most pipeLimit bytes: write blocks the connection's reader until
// Read makes room, so a slow reader slows the process's output rather than
// filling the host's memory, and abort ends that wait.
type pipeBuffer struct {
	mu      sync.Mutex
	cond    *sync.Cond
	buf     bytes.Buffer
	closed  bool
	aborted bool
}

// pipeLimit is the most output a pipeBuffer holds unread.
const pipeLimit = 4 << 20

func newPipeBuffer() *pipeBuffer {
	b := &pipeBuffer{}
	b.cond = sync.NewCond(&b.mu)
	return b
}

func (b *pipeBuffer) write(p []byte) {
	b.mu.Lock()
	defer b.mu.Unlock()
	for b.buf.Len() > 0 && b.buf.Len()+len(p) > pipeLimit && !b.aborted {
		b.cond.Wait()
	}
	if !b.aborted {
		b.buf.Write(p)
	}
	b.cond.Broadcast()
}

func (b *pipeBuffer) close() {
	b.mu.Lock()
	b.closed = true
	b.mu.Unlock()
	b.cond.Broadcast()
}

// abort drops what is unread and every later write, and ends a write's wait
// for room: the process is being killed, and nobody will read the rest.
func (b *pipeBuffer) abort() {
	b.mu.Lock()
	b.aborted, b.closed = true, true
	b.buf.Reset()
	b.mu.Unlock()
	b.cond.Broadcast()
}

func (b *pipeBuffer) Read(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	for b.buf.Len() == 0 && !b.closed {
		b.cond.Wait()
	}
	if b.buf.Len() == 0 {
		return 0, io.EOF
	}
	n, err := b.buf.Read(p)
	b.cond.Broadcast()
	return n, err
}
