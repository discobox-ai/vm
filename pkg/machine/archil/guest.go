package archil

import (
	"bufio"
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/discobox-ai/vm/pkg/machine"
)

// An archil guest's operations are Archil's process API, with nothing of
// disco-vm's in the guest (ADR 26-11-03-676): the image is the caller's. A
// process runs as root, in the request's directory, with its variables, its
// argv one quoted command line; another account is runuser's. Copies are tar
// over a process, and a shutdown is Archil's stop.

// The scripts the copies and Info run, with sh, as root. Each takes its path
// as $1.
const (
	// copyInScript extracts stdin into a directory it creates. What it
	// unpacks is root's. GNU tar keeps the mode of a directory already
	// there; BusyBox's tar, which has no --no-overwrite-dir, is given only
	// what it takes.
	copyInScript = `mkdir -p -- "$1" || exit
if tar --no-overwrite-dir --help >/dev/null 2>&1; then exec tar -x -f - --no-same-owner --no-overwrite-dir -C "$1"; fi
exec tar -x -o -f - -C "$1"`
	// copyOutScript tars the path into a file of its own, then writes
	// copyOutReady, so a missing path or a failed tar is an error before any
	// stream. It sends the tar a chunk at a time, as Archil's SDK downloads a
	// file: each line of stdin asks for up to that many bytes, answered by a
	// line with the count sent and then the bytes, and a short chunk is the
	// last. Archil drops output that outruns its connection, so nothing is
	// sent that was not asked for. A chunk is read with dd from the file, not
	// from tar's pipe: dd reads a regular file in full blocks, and a pipe
	// read through stdio (BusyBox's head) takes more than it was asked for.
	// The file is in /var/tmp, since /tmp may be memory, and goes when the
	// script ends, killed or not, short of SIGKILL.
	copyOutScript = `if [ ! -e "$1" ] && [ ! -L "$1" ]; then echo "$1: no such file or directory" >&2; exit 1; fi
t=$(mktemp -p /var/tmp disco-vm-copy.XXXXXX) || exit 1
trap 'rm -f "$t" "$t.chunk"' EXIT
trap 'exit 129' HUP
trap 'exit 130' INT
trap 'exit 143' TERM
tar -c -f "$t" -C "$(dirname -- "$1")" -- "$(basename -- "$1")" || exit
printf R
exec 3< "$t"
while IFS= read -r count; do
	dd bs="$count" count=1 <&3 > "$t.chunk" 2>/dev/null
	size=$(wc -c < "$t.chunk")
	printf '%s\n' "$size"
	cat "$t.chunk"
	[ "$size" -eq "$count" ] || break
done`
	copyOutReady = 'R'
	infoScript   = `uname -m; hostname`
)

// quote makes s one shell word.
func quote(s string) string { return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'" }

func quoteAll(words []string) string {
	quoted := make([]string, len(words))
	for i, w := range words {
		quoted[i] = quote(w)
	}
	return strings.Join(quoted, " ")
}

// command is the line a process runs for req: its argv, or runuser's of it
// for another account, with that account's HOME.
func command(req machine.ExecRequest) string {
	if req.User == "" {
		return "exec " + quoteAll(req.Argv)
	}
	return `exec runuser -u ` + quote(req.User) + ` -- env HOME="$(getent passwd ` + quote(req.User) + ` | cut -d: -f6)" ` + quoteAll(req.Argv)
}

// envOf is a request's variables as the process API takes them.
func envOf(req machine.ExecRequest) map[string]string {
	env := map[string]string{}
	for _, kv := range req.Env {
		if k, v, ok := strings.Cut(kv, "="); ok {
			env[k] = v
		}
	}
	return env
}

// script is the request that runs a script with sh, with args after it as $1
// and on.
func script(text string, args ...string) machine.ExecRequest {
	return machine.ExecRequest{Argv: append([]string{"sh", "-c", text, "sh"}, args...)}
}

// guestMachine is a running sandbox. Nothing ties it to this process: Archil
// runs it, and any number of processes may hold one (Attach). It watches the
// sandbox's state only once something asks whether it stopped.
type guestMachine struct {
	api      *api
	id, name string
	done     chan struct{}
	err      error
	once     sync.Once
	watching sync.Once
	ctx      context.Context
	stop     context.CancelFunc
	killed   atomic.Bool
	// paused is set for a sandbox Archil paused, which awake resumes.
	paused atomic.Bool
	waking sync.Mutex
}

var _ machine.Machine = (*guestMachine)(nil)

func newMachine(a *api, id, name string) *guestMachine {
	ctx, stop := context.WithCancel(context.Background())
	return &guestMachine{api: a, id: id, name: name, done: make(chan struct{}), ctx: ctx, stop: stop}
}

// awake resumes the sandbox if Attach found it paused (or pausing), and
// waits until it runs. Shutdown and Kill don't: Archil stops a paused
// sandbox as it is.
func (m *guestMachine) awake(ctx context.Context) error {
	if !m.paused.Load() {
		return nil
	}
	m.waking.Lock()
	defer m.waking.Unlock()
	if !m.paused.Load() {
		return nil
	}
	s, err := m.api.waitFor(ctx, m.id, "paused", "running")
	if err != nil {
		return err
	}
	if s.Status == "paused" {
		if err := m.api.resume(ctx, m.id); err != nil {
			return fmt.Errorf("archil: resume %s: %w", m.name, err)
		}
		if _, err := m.api.waitFor(ctx, m.id, "running"); err != nil {
			return err
		}
	}
	m.paused.Store(false)
	return nil
}

// Exec starts req through the process API. The process is not bound to ctx,
// which only bounds starting it.
func (m *guestMachine) Exec(ctx context.Context, req machine.ExecRequest) (machine.Process, error) {
	if len(req.Argv) == 0 {
		return nil, errors.New("archil: exec of nothing")
	}
	select {
	case <-m.done:
		return nil, fmt.Errorf("archil: %s is stopped", m.name)
	default:
	}
	if err := m.awake(ctx); err != nil {
		return nil, err
	}
	var terminal any
	if req.TTY {
		rows, cols := req.Rows, req.Cols
		if rows == 0 || cols == 0 {
			rows, cols = 24, 80
		}
		terminal = map[string]uint16{"rows": rows, "cols": cols}
	}
	dir := req.Dir
	if dir == "" {
		dir = "/"
	}
	p, err := m.api.startProcess(ctx, m.id, command(req), dir, envOf(req), terminal, false)
	if err != nil {
		return nil, err
	}
	return &execProcess{p: p}, nil
}

// execProcess is a process as machine.Process.
type execProcess struct {
	p      *process
	closed atomic.Bool
}

func (e *execProcess) Write(b []byte) (int, error)                { return e.p.Write(b) }
func (e *execProcess) CloseStdin() error                          { return e.p.CloseStdin() }
func (e *execProcess) Resize(rows, cols uint16) error             { return e.p.Resize(rows, cols) }
func (e *execProcess) Wait(stdout, stderr io.Writer) (int, error) { return e.p.Wait(stdout, stderr) }

// Close kills the process if it still runs, and ends its connection.
func (e *execProcess) Close() error {
	if e.closed.Swap(true) {
		return nil
	}
	return e.p.Kill()
}

func (m *guestMachine) CopyTo(ctx context.Context, dir string, tarStream io.Reader) error {
	var stderr bytes.Buffer
	code, err := machine.Run(ctx, m, script(copyInScript, dir), tarStream, io.Discard, &stderr)
	if err == nil && code != 0 {
		err = fmt.Errorf("exit %d: %s", code, bytes.TrimSpace(stderr.Bytes()))
	}
	if err != nil {
		return fmt.Errorf("archil: copy into %s: %w", dir, err)
	}
	return nil
}

// copyChunk is how much of a tar CopyFrom asks for at once, as Archil's SDK
// asks for a file.
const copyChunk = 512 << 10

func (m *guestMachine) CopyFrom(ctx context.Context, path string) (io.ReadCloser, error) {
	if err := m.awake(ctx); err != nil {
		return nil, err
	}
	proc, err := m.api.startProcess(ctx, m.id, command(script(copyOutScript, path)), "/", nil, nil, true)
	if err != nil {
		return nil, err
	}
	out := bufio.NewReader(proc.stdout)
	var stderr bytes.Buffer
	stderrDone := make(chan struct{})
	go func() { defer close(stderrDone); _, _ = io.Copy(&stderr, proc.stderr) }()
	// finish reads what is left and returns how the process ended, with
	// its stderr when it failed.
	finish := func(err error) error {
		if err != nil {
			_ = proc.Kill()
		}
		_, _ = io.Copy(io.Discard, out)
		<-stderrDone
		// The process's own error (output Archil dropped, say) says more
		// than the chunk read it cut short.
		switch code, waitErr := proc.result(); {
		case waitErr != nil:
			err = waitErr
		case err == nil && code != 0:
			err = fmt.Errorf("exit %d", code)
		}
		if err == nil {
			return nil
		}
		if msg := bytes.TrimSpace(stderr.Bytes()); len(msg) > 0 {
			return fmt.Errorf("archil: copy from %s: %w: %s", path, err, msg)
		}
		return fmt.Errorf("archil: copy from %s: %w", path, err)
	}
	ready, err := out.ReadByte()
	switch {
	case err != nil:
		if err := finish(nil); err != nil {
			return nil, err
		}
		return nil, fmt.Errorf("archil: copy from %s: the guest sent nothing", path)
	case ready != copyOutReady:
		return nil, finish(fmt.Errorf("the guest answered %q before the tar", ready))
	}
	reader, writer := io.Pipe()
	go func() { writer.CloseWithError(finish(copyChunks(proc, out, writer))) }()
	return &copyStream{PipeReader: reader, process: &execProcess{p: proc}}, nil
}

// copyChunks asks copyOutScript for its tar a chunk at a time and writes it
// to w, until a chunk comes back short.
func copyChunks(p *process, out *bufio.Reader, w io.Writer) error {
	ask := []byte(strconv.Itoa(copyChunk) + "\n")
	for {
		if _, err := p.Write(ask); err != nil {
			return err
		}
		line, err := out.ReadString('\n')
		if err != nil {
			return fmt.Errorf("the guest's chunk: %w", err)
		}
		n, err := strconv.Atoi(strings.TrimSpace(line))
		if err != nil || n < 0 || n > copyChunk {
			return fmt.Errorf("the guest answered %q for a chunk", line)
		}
		if _, err := io.CopyN(w, out, int64(n)); err != nil {
			return err
		}
		if n < copyChunk {
			// The script ends after a short chunk; its stdin needs no
			// closing, and a close could race its exit.
			return nil
		}
	}
}

// copyStream is CopyFrom's tar, whose Close ends the process.
type copyStream struct {
	*io.PipeReader
	process machine.Process
}

func (s *copyStream) Close() error {
	_ = s.process.Close()
	return s.PipeReader.Close()
}

func (m *guestMachine) Info(ctx context.Context) (machine.GuestInfo, error) {
	var out, stderr bytes.Buffer
	code, err := machine.Run(ctx, m, script(infoScript), nil, &out, &stderr)
	if err == nil && code != 0 {
		err = fmt.Errorf("exit %d: %s", code, bytes.TrimSpace(stderr.Bytes()))
	}
	if err != nil {
		return machine.GuestInfo{}, fmt.Errorf("archil: info: %w", err)
	}
	lines := strings.Fields(out.String())
	if len(lines) != 2 {
		return machine.GuestInfo{}, fmt.Errorf("archil: info: the guest answered %q", out.String())
	}
	arch := map[string]string{"x86_64": "amd64", "aarch64": "arm64"}[lines[0]]
	if arch == "" {
		arch = lines[0]
	}
	return machine.GuestInfo{OS: string(machine.Linux), Arch: arch, Hostname: lines[1]}, nil
}

// Shutdown is Archil's stop, which shuts the guest down and checkpoints its
// disk; it returns once Archil has accepted it, and Done closes when Archil
// says the sandbox stopped. A reboot is refused: Archil has none, and a stop
// and a start would leave the start in a goroutine of this process.
func (m *guestMachine) Shutdown(ctx context.Context, reboot bool) error {
	if reboot {
		return fmt.Errorf("archil: a reboot: %w", machine.ErrUnsupported)
	}
	return m.stopSandbox(ctx)
}

// stopSandbox asks Archil to stop the sandbox, as it is: running, or
// paused. One still pausing is waited for, since Archil refuses its stop.
func (m *guestMachine) stopSandbox(ctx context.Context) error {
	if s, err := m.api.sandbox(ctx, m.id); err == nil && s.Status == "pausing" {
		if _, err := m.api.waitFor(ctx, m.id, "paused"); err != nil {
			return err
		}
	}
	if err := m.api.stop(ctx, m.id); err != nil && !isNotFound(err) && !isConflict(err) {
		return fmt.Errorf("archil: stop %s: %w", m.name, err)
	}
	return nil
}

// waitReady waits until a process runs in the sandbox, so a command run after
// Start reaches it. A sandbox that stops first ends the wait. It asks
// Archil itself rather than through Done, which would leave the machine
// watched.
func (m *guestMachine) waitReady(ctx context.Context) error {
	ctx, cancel := context.WithTimeout(ctx, 2*time.Minute)
	defer cancel()
	var last error
	for {
		code, _, stderr, err := m.api.run(ctx, m.id, "true", "", nil)
		if err == nil && code != 0 {
			err = fmt.Errorf("exit %d: %s", code, bytes.TrimSpace(stderr))
		}
		if err == nil {
			return nil
		}
		last = err
		select {
		case <-ctx.Done():
			return fmt.Errorf("archil: %s did not answer: %w (last: %w)", m.name, ctx.Err(), last)
		case <-time.After(time.Second):
		}
		if s, err := m.api.sandbox(ctx, m.id); err == nil && s.Status != "running" && s.Status != "pending" {
			return fmt.Errorf("archil: %s is %s before it answered", m.name, s.Status)
		}
	}
}

func (m *guestMachine) watched() <-chan struct{} {
	m.watching.Do(func() { go m.watch() })
	return m.done
}

// watch polls the sandbox's state until it is not running. A remote API that
// stops answering gives the machine up after a while rather than hold its
// caller forever.
func (m *guestMachine) watch() {
	failing := time.Time{}
	for {
		select {
		case <-m.ctx.Done():
			return
		case <-time.After(2 * time.Second):
		}
		s, err := m.api.sandbox(m.ctx, m.id)
		switch {
		case isNotFound(err):
			m.finish(fmt.Errorf("archil: sandbox of %s was deleted", m.name))
			return
		case err != nil:
			if failing.IsZero() {
				failing = time.Now()
			} else if time.Since(failing) > 5*time.Minute {
				m.finish(fmt.Errorf("archil: gave up on %s: %w", m.name, err))
				return
			}
			continue
		}
		failing = time.Time{}
		switch s.Status {
		case "stopped", "exited":
			m.finish(nil)
			return
		case "failed", "deleted", "deleting":
			m.finish(fmt.Errorf("archil: %s is %s: %s", m.name, s.Status, s.ExitReason))
			return
		}
	}
}

func (m *guestMachine) finish(err error) {
	m.once.Do(func() {
		if !m.killed.Load() {
			m.err = err
		}
		m.stop()
		close(m.done)
	})
}

// Kill stops the sandbox through Archil and waits for it to stop.
func (m *guestMachine) Kill(ctx context.Context) error {
	m.watched()
	m.killed.Store(true)
	if err := m.stopSandbox(ctx); err != nil {
		return err
	}
	select {
	case <-m.done:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

func (m *guestMachine) Done() <-chan struct{} { return m.watched() }

func (m *guestMachine) Err() error {
	<-m.watched()
	return m.err
}

// Dial is unsupported: Archil reaches no port of a sandbox but the ones it
// exposes publicly.
func (m *guestMachine) Dial(context.Context, uint32) (net.Conn, error) {
	return nil, fmt.Errorf("archil: a guest port: %w", machine.ErrUnsupported)
}
