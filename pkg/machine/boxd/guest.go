package boxd

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"slices"
	"strings"
	"sync"
	"time"

	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

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
		return nil, execError(err, false)
	}
	first := &boxdapi.ExecChunk{VmId: vm, Command: command(req), Tty: req.TTY}
	if req.TTY {
		first.Rows, first.Cols = uint32(req.Rows), uint32(req.Cols)
	}
	if err := stream.Send(first); err != nil {
		cancel()
		return nil, execError(err, false)
	}
	if !stop() {
		cancel()
		return nil, ctx.Err()
	}
	return &process{stream: stream, cancel: cancel, tty: req.TTY}, nil
}

// execError is an Exec stream's err. gRPC's Unavailable before the guest
// has answered anything (no output, no exit code) is the guest, or boxd's
// API, not reached: machine.ErrUnreachable, which a caller straight after a
// start tries again. gRPC gives it for a connection it cannot make; boxd has
// not been seen to give one itself: an Exec sent while a machine boots is
// held until the guest answers, and one to a stopped machine is
// FailedPrecondition. A stream cut after the command ran, before its exit
// code, reads the same, which is why only an operation that can be repeated
// is tried again.
func execError(err error, answered bool) error {
	if !answered && status.Code(err) == codes.Unavailable {
		return fmt.Errorf("boxd: exec: %w: %w", machine.ErrUnreachable, err)
	}
	return fmt.Errorf("boxd: exec: %w", err)
}

// process is one Exec stream.
type process struct {
	stream execStream
	cancel context.CancelFunc
	tty    bool
	// answered is set once the guest has sent anything, Wait's alone.
	answered bool

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
			return -1, execError(err, p.answered)
		}
		p.answered = true
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

// readyCommand asks systemd how far the guest has booted, without waiting:
// waitGuest asks it again until systemd says it has finished starting the
// guest, whether every unit came up or not (settled). Commit waits for that,
// so a snapshot is of a settled guest, and so does Create's power-off of a
// new machine, so its disk is one its first boot finished. Nothing in the
// guest waits: `systemctl is-system-running --wait` sent early in a boot has
// hung until its attempt's timeout though the guest booted seconds in, and
// has returned at once with "offline" or a bus error while systemd was still
// initializing; `systemctl start default.target` sent then failed at once on
// the bus, or returned while systemd was still starting (sandbox image, nyc,
// 2026-11-16). A start waits for nothing in the guest (Boot).
var readyCommand = machine.ExecRequest{Argv: []string{"systemctl", "is-system-running"}}

// settled is what readyCommand prints once systemd has finished starting the
// guest: everything up, some unit failed, or the rescue or emergency target.
// Anything else, or nothing (systemd's bus not up yet), is a guest still
// booting, or stopping.
var settled = []string{"running", "degraded", "maintenance"}

const (
	// readyAttempt bounds each readyCommand, which answers at once, so that
	// an Exec the guest does not answer is asked again.
	readyAttempt = 5 * time.Second
	// readyPoll is the pause between readyCommands.
	readyPoll = 200 * time.Millisecond
)

// waitGuest waits until boxd's Exec reaches the guest and systemd has
// finished starting it.
func waitGuest(ctx context.Context, api boxdapi.BoxdApiClient, vm string) error {
	ctx, cancel := context.WithTimeout(ctx, 5*time.Minute)
	defer cancel()
	for {
		last := guestSettled(ctx, api, vm)
		if last == nil {
			return nil
		}
		select {
		case <-ctx.Done():
			return fmt.Errorf("boxd: %s did not settle: %w (last: %w)", vm, ctx.Err(), last)
		case <-time.After(readyPoll):
		}
	}
}

// guestSettled asks readyCommand once, within readyAttempt, and fails unless
// it prints a settled state, with what it printed, its exit code, and its
// stderr: a guest that is still booting, or a failure that will not clear
// (sudo refused, no systemctl), says which.
func guestSettled(ctx context.Context, api boxdapi.BoxdApiClient, vm string) error {
	ctx, cancel := context.WithTimeout(ctx, readyAttempt)
	defer cancel()
	p, err := startExec(ctx, api, vm, readyCommand)
	if err != nil {
		return err
	}
	defer p.Close()
	stop := context.AfterFunc(ctx, func() { _ = p.Close() })
	defer stop()
	_ = p.CloseStdin()
	var out, stderr bytes.Buffer
	code, err := p.Wait(&out, &stderr)
	if err != nil {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		return err
	}
	// Its exit code is 0 for running alone, so only what it prints decides.
	state := strings.TrimSpace(out.String())
	if slices.Contains(settled, state) {
		return nil
	}
	return fmt.Errorf("systemd is %q (exit %d: %s)", state, code, bytes.TrimSpace(stderr.Bytes()))
}
