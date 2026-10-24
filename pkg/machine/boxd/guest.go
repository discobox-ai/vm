package boxd

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"strings"
	"sync"
	"time"

	"google.golang.org/grpc"

	"github.com/discobox-ai/vm/pkg/machine"
	"github.com/discobox-ai/vm/pkg/machine/boxd/internal/boxdapi"
)

// A boxd guest's operations are boxd's Exec, with nothing of disco-vm's in
// the guest. Exec runs a shell command as boxd's own user, who has
// passwordless sudo, so every operation is one command line: sudo to the
// account the request names (root by default, as an agent would run it),
// env for its directory and variables, and the request's argv, each word
// quoted for the shell. Copies are tar over the same stream, and a shutdown
// is systemd's, which the shutdown hook and the API finish.

// execStream is the boxd Exec call.
type execStream = grpc.BidiStreamingClient[boxdapi.ExecChunk, boxdapi.ExecChunk]

// maxChunk keeps each stdin message well under gRPC's 4 MiB message cap.
const maxChunk = 256 << 10

// The scripts the copies and Info run, with sh, as root. Each takes its path
// as $1.
const (
	// copyInScript extracts stdin into a directory it creates. What it
	// unpacks is root's, as an agent's is, and a directory already there
	// keeps its mode.
	copyInScript = `mkdir -p -- "$1" && exec tar -x -f - --no-same-owner --no-overwrite-dir -C "$1"`
	// copyOutScript writes copyOutReady once it has found the path, then a
	// tar of it rooted at its base name, so a missing path is an error before
	// any stream, not an empty one.
	copyOutScript = `if [ ! -e "$1" ] && [ ! -L "$1" ]; then echo "$1: no such file or directory" >&2; exit 1; fi
printf R
exec tar -c -f - -C "$(dirname -- "$1")" -- "$(basename -- "$1")"`
	copyOutReady = 'R'
	infoScript   = `uname -m; hostname`
)

// command is the line Exec runs for req.
func command(req machine.ExecRequest) string {
	user, dir := req.User, req.Dir
	if user == "" {
		user = "root"
	}
	if dir == "" {
		dir = "/"
	}
	// env's options end at "--", before the variables: once it has read one,
	// everything after is the variables and the command, and a "--" there
	// would be the command.
	words := append([]string{"sudo", "-n", "-H", "-u", user, "--", "env", "-C", dir, "--"}, req.Env...)
	words = append(words, req.Argv...)
	quoted := make([]string, len(words))
	for i, w := range words {
		quoted[i] = quote(w)
	}
	return strings.Join(quoted, " ")
}

// quote makes s one shell word.
func quote(s string) string { return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'" }

// script is the request that runs a script with sh, as root, with args after
// it as $1 and on.
func script(text string, args ...string) machine.ExecRequest {
	return machine.ExecRequest{Argv: append([]string{"sh", "-c", text, "sh"}, args...)}
}

func (m *vm) Exec(ctx context.Context, req machine.ExecRequest) (machine.Process, error) {
	if len(req.Argv) == 0 {
		return nil, errors.New("boxd: exec: argv is required")
	}
	select {
	case <-m.done:
		return nil, errors.New("boxd: guest is stopped")
	default:
	}
	return startExec(ctx, m.api, m.ref.ID, req)
}

// startExec opens an Exec stream for req. The stream is not bound to ctx,
// which only bounds starting it: the process lives until it exits or is
// closed.
func startExec(ctx context.Context, api boxdapi.BoxdApiClient, vm string, req machine.ExecRequest) (*process, error) {
	streamCtx, cancel := context.WithCancel(context.Background())
	stop := context.AfterFunc(ctx, cancel)
	defer stop()
	stream, err := api.Exec(streamCtx)
	if err != nil {
		cancel()
		return nil, fmt.Errorf("boxd: exec: %w", err)
	}
	first := &boxdapi.ExecChunk{VmId: vm, Command: command(req), Tty: req.TTY}
	if req.TTY {
		first.Rows, first.Cols = uint32(req.Rows), uint32(req.Cols)
	}
	if err := stream.Send(first); err != nil {
		cancel()
		return nil, fmt.Errorf("boxd: exec: %w", err)
	}
	if !stop() {
		cancel()
		return nil, ctx.Err()
	}
	return &process{stream: stream, cancel: cancel, tty: req.TTY}, nil
}

// process is one Exec stream.
type process struct {
	stream execStream
	cancel context.CancelFunc
	tty    bool

	sendMu sync.Mutex
}

var _ machine.Process = (*process)(nil)

func (p *process) Write(b []byte) (int, error) {
	p.sendMu.Lock()
	defer p.sendMu.Unlock()
	written := 0
	for len(b) > 0 {
		n := min(len(b), maxChunk)
		// Send copies before it returns, so b is not retained.
		if err := p.stream.Send(&boxdapi.ExecChunk{Data: b[:n], Stdin: true}); err != nil {
			return written, fmt.Errorf("boxd: exec stdin: %w", err)
		}
		written += n
		b = b[n:]
	}
	return written, nil
}

// CloseStdin half-closes the stream, which the command reads as EOF. A
// terminal has no end of input, as on the agent: its session ends when its
// process does.
func (p *process) CloseStdin() error {
	if p.tty {
		return nil
	}
	p.sendMu.Lock()
	defer p.sendMu.Unlock()
	return p.stream.CloseSend()
}

func (p *process) Resize(rows, cols uint16) error {
	if !p.tty {
		return nil
	}
	p.sendMu.Lock()
	defer p.sendMu.Unlock()
	return p.stream.Send(&boxdapi.ExecChunk{WindowChange: true, Rows: uint32(rows), Cols: uint32(cols)})
}

// Wait copies output until the stream ends, with the exit code in its last
// chunk.
func (p *process) Wait(stdout, stderr io.Writer) (int, error) {
	var code int32
	for {
		msg, err := p.stream.Recv()
		if errors.Is(err, io.EOF) {
			return int(code), nil
		}
		if err != nil {
			return -1, fmt.Errorf("boxd: exec: %w", err)
		}
		if msg.GetExitCode() != 0 {
			code = msg.GetExitCode()
		}
		if len(msg.GetData()) == 0 {
			continue
		}
		w := stdout
		if msg.GetIsStderr() {
			w = stderr
		}
		if w != nil {
			_, _ = w.Write(msg.GetData())
		}
	}
}

// Close cancels the call, which kills the command in the guest.
func (p *process) Close() error {
	p.cancel()
	return nil
}

func (m *vm) CopyTo(ctx context.Context, dir string, tarStream io.Reader) error {
	var stderr bytes.Buffer
	code, err := machine.Run(ctx, m, script(copyInScript, dir), tarStream, io.Discard, &stderr)
	if err == nil && code != 0 {
		err = fmt.Errorf("exit %d: %s", code, bytes.TrimSpace(stderr.Bytes()))
	}
	if err != nil {
		return fmt.Errorf("boxd: copy into %s: %w", dir, err)
	}
	return nil
}

func (m *vm) CopyFrom(ctx context.Context, path string) (io.ReadCloser, error) {
	p, err := m.Exec(ctx, script(copyOutScript, path))
	if err != nil {
		return nil, err
	}
	_ = p.CloseStdin()
	reader, writer := io.Pipe()
	go func() {
		var stderr bytes.Buffer
		code, err := p.Wait(writer, &stderr)
		if err == nil && code != 0 {
			err = fmt.Errorf("boxd: copy from %s: exit %d: %s", path, code, bytes.TrimSpace(stderr.Bytes()))
		}
		writer.CloseWithError(err)
	}()
	stream := &copyStream{PipeReader: reader, process: p}
	var ready [1]byte
	if _, err := io.ReadFull(reader, ready[:]); err != nil {
		_ = stream.Close()
		if errors.Is(err, io.ErrUnexpectedEOF) || errors.Is(err, io.EOF) {
			err = fmt.Errorf("boxd: copy from %s: the guest sent nothing", path)
		}
		return nil, err
	}
	if ready[0] != copyOutReady {
		_ = stream.Close()
		return nil, fmt.Errorf("boxd: copy from %s: the guest answered %q before the tar", path, ready[0])
	}
	return stream, nil
}

// copyStream is CopyFrom's tar, whose Close ends the command.
type copyStream struct {
	*io.PipeReader
	process machine.Process
}

func (s *copyStream) Close() error {
	_ = s.process.Close()
	return s.PipeReader.Close()
}

func (m *vm) Info(ctx context.Context) (machine.GuestInfo, error) {
	var out, stderr bytes.Buffer
	code, err := machine.Run(ctx, m, script(infoScript), nil, &out, &stderr)
	if err == nil && code != 0 {
		err = fmt.Errorf("exit %d: %s", code, bytes.TrimSpace(stderr.Bytes()))
	}
	if err != nil {
		return machine.GuestInfo{}, fmt.Errorf("boxd: info: %w", err)
	}
	lines := strings.Fields(out.String())
	if len(lines) != 2 {
		return machine.GuestInfo{}, fmt.Errorf("boxd: info: the guest answered %q", out.String())
	}
	arch := map[string]string{"x86_64": "amd64", "aarch64": "arm64"}[lines[0]]
	if arch == "" {
		arch = lines[0]
	}
	return machine.GuestInfo{OS: string(machine.Linux), Arch: arch, Hostname: lines[1]}, nil
}

// Shutdown is boxd's: a reboot is RebootVm, and a power-off flushes the
// guest's writes and then asks StopVm, returning once boxd has accepted the
// stop; Done closes when it says the machine stopped. Nothing in the guest is
// asked to power off, since boxd leaves a guest that halts itself running. A
// flush that fails is an error, and no StopVm is asked: the caller's fallback
// is Kill, and no snapshot is cut from the disk.
func (m *vm) Shutdown(ctx context.Context, reboot bool) error {
	if reboot {
		if _, err := m.api.RebootVm(ctx, &boxdapi.RebootVmRequest{VmId: m.ref.ID}); err != nil {
			return fmt.Errorf("boxd: reboot %s: %w", m.ref.Name, err)
		}
		return nil
	}
	if err := syncDisks(ctx, m.api, m.ref.ID); err != nil {
		return err
	}
	if _, err := m.api.StopVm(ctx, &boxdapi.StopVmRequest{VmId: m.ref.ID}); err != nil && !isNotFound(err) {
		return fmt.Errorf("boxd: stop %s: %w", m.ref.Name, err)
	}
	return nil
}

// runAsRoot runs req to completion and fails on any exit but 0, with what the
// command printed.
func runAsRoot(ctx context.Context, api boxdapi.BoxdApiClient, vm string, req machine.ExecRequest) error {
	p, err := startExec(ctx, api, vm, req)
	if err != nil {
		return err
	}
	defer p.Close()
	stop := context.AfterFunc(ctx, func() { _ = p.Close() })
	defer stop()
	_ = p.CloseStdin()
	var out bytes.Buffer
	code, err := p.Wait(&out, &out)
	if err != nil && ctx.Err() != nil {
		return ctx.Err()
	}
	if err == nil && code != 0 {
		err = fmt.Errorf("exit %d: %s", code, bytes.TrimSpace(out.Bytes()))
	}
	return err
}

// readyCommand returns once systemd has finished starting the guest, whether
// every unit came up or not.
var readyCommand = machine.ExecRequest{Argv: []string{"systemctl", "is-system-running", "--wait"}}

// waitGuest waits until the guest's operations answer: until boxd's Exec
// reaches the guest and systemd has finished starting it, so a command runs
// in a booted system, as one an agent started would. stopped, when it is
// closed, ends the wait.
func waitGuest(ctx context.Context, api boxdapi.BoxdApiClient, vm string, stopped <-chan struct{}) error {
	ctx, cancel := context.WithTimeout(ctx, 5*time.Minute)
	defer cancel()
	var last error
	for {
		attempt, cancelAttempt := context.WithTimeout(ctx, 2*time.Minute)
		p, err := startExec(attempt, api, vm, readyCommand)
		if err == nil {
			_ = p.CloseStdin()
			var out bytes.Buffer
			stop := context.AfterFunc(attempt, func() { _ = p.Close() })
			_, err = p.Wait(&out, &out)
			stop()
			_ = p.Close()
			if err == nil && out.Len() == 0 {
				err = errors.New("systemd said nothing")
			}
		}
		cancelAttempt()
		if err == nil {
			return nil
		}
		last = err
		select {
		case <-ctx.Done():
			return fmt.Errorf("boxd: %s did not answer: %w (last: %w)", vm, ctx.Err(), last)
		case <-stopped:
			return fmt.Errorf("boxd: %s stopped before it answered", vm)
		case <-time.After(time.Second):
		}
	}
}
