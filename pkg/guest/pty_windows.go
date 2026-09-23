package guest

import (
	"errors"
	"os"
	"os/exec"
	"strings"
	"sync"
	"sync/atomic"
	"time"
	"unicode/utf16"
	"unsafe"

	"golang.org/x/sys/windows"
)

// ptyHandle is a started process's pseudo console: reading it is the screen,
// writing it is the keyboard.
//
// A pseudo console's output pipe reaches EOF only once the console is closed,
// not when its process exits, so a watcher closes it when the process is done.
// It waits for the output to go quiet first: the console renders
// asynchronously, and closing it drops whatever it had not written yet, which
// for a short command is all of it. guestexec's OP_CONSOLE in sandboxi is the
// proven Rust version of this.
type ptyHandle struct {
	*conPTY
}

type conPTY struct {
	console   windows.Handle
	in        *os.File
	out       *os.File
	lastRead  atomic.Int64
	closeOnce sync.Once
}

func (p *conPTY) Read(b []byte) (int, error) {
	n, err := p.out.Read(b)
	if n > 0 {
		p.lastRead.Store(time.Now().UnixNano())
	}
	// The pipe breaking is how a closed console ends its output.
	if errors.Is(err, windows.ERROR_BROKEN_PIPE) {
		err = os.ErrClosed
	}
	return n, err
}

func (p *conPTY) Write(b []byte) (int, error) { return p.in.Write(b) }

func (p *conPTY) Resize(rows, cols uint16) error {
	return windows.ResizePseudoConsole(p.console, windows.Coord{X: int16(cols), Y: int16(rows)})
}

// closeConsole ends the console, which ends its output.
func (p *conPTY) closeConsole() {
	p.closeOnce.Do(func() { windows.ClosePseudoConsole(p.console) })
}

func (p *conPTY) Close() error {
	p.closeConsole()
	_ = p.in.Close()
	return p.out.Close()
}

// drainThenClose waits for output to stop arriving (quiet for 250 ms, at most
// 2 s), then closes the console.
func (p *conPTY) drainThenClose() {
	const quiet, most = 250 * time.Millisecond, 2 * time.Second
	deadline := time.Now().Add(most)
	for time.Now().Before(deadline) {
		if time.Since(time.Unix(0, p.lastRead.Load())) >= quiet {
			break
		}
		time.Sleep(25 * time.Millisecond)
	}
	p.closeConsole()
}

// startPTY starts cmd attached to a new pseudo console. exec.Cmd cannot hand a
// process a pseudo console, so the process is created here and adopted as
// cmd.Process, which is all cmd.Wait needs.
func startPTY(cmd *exec.Cmd, rows, cols uint16) (ptyHandle, error) {
	if cmd.Err != nil {
		return ptyHandle{}, cmd.Err
	}
	if rows == 0 || cols == 0 {
		rows, cols = 24, 80
	}
	var inRead, inWrite, outRead, outWrite windows.Handle
	if err := windows.CreatePipe(&inRead, &inWrite, nil, 0); err != nil {
		return ptyHandle{}, err
	}
	if err := windows.CreatePipe(&outRead, &outWrite, nil, 0); err != nil {
		windows.CloseHandle(inRead)
		windows.CloseHandle(inWrite)
		return ptyHandle{}, err
	}
	var console windows.Handle
	err := windows.CreatePseudoConsole(windows.Coord{X: int16(cols), Y: int16(rows)}, inRead, outWrite, 0, &console)
	// The console holds its own references; ours would keep the pipes from
	// ever reporting EOF.
	windows.CloseHandle(inRead)
	windows.CloseHandle(outWrite)
	if err != nil {
		windows.CloseHandle(inWrite)
		windows.CloseHandle(outRead)
		return ptyHandle{}, err
	}
	p := &conPTY{console: console, in: os.NewFile(uintptr(inWrite), "conpty-in"), out: os.NewFile(uintptr(outRead), "conpty-out")}
	p.lastRead.Store(time.Now().UnixNano())

	process, err := createConsoleProcess(cmd, console)
	if err != nil {
		_ = p.Close()
		return ptyHandle{}, err
	}
	go func() {
		_, _ = windows.WaitForSingleObject(process, windows.INFINITE)
		windows.CloseHandle(process)
		p.drainThenClose()
	}()
	return ptyHandle{p}, nil
}

// createConsoleProcess creates cmd's process on console, as cmd's token when
// it has one, and sets cmd.Process. It returns a handle to the process that
// the caller closes.
func createConsoleProcess(cmd *exec.Cmd, console windows.Handle) (windows.Handle, error) {
	attrs, err := windows.NewProcThreadAttributeList(1)
	if err != nil {
		return 0, err
	}
	defer attrs.Delete()
	// The attribute's value is the console handle itself, not a pointer to it.
	if err := attrs.Update(windows.PROC_THREAD_ATTRIBUTE_PSEUDOCONSOLE, *(*unsafe.Pointer)(unsafe.Pointer(&console)), unsafe.Sizeof(console)); err != nil {
		return 0, err
	}
	si := &windows.StartupInfoEx{ProcThreadAttributeList: attrs.List()}
	si.Cb = uint32(unsafe.Sizeof(*si))
	// Null std handles, explicitly: otherwise a child of a process whose own
	// std handles are redirected (a service, a test) starts on those instead
	// of the console and fails to initialize (0xC0000142).
	si.Flags |= windows.STARTF_USESTDHANDLES

	cmdline := windows.ComposeCommandLine(cmd.Args)
	if cmd.SysProcAttr != nil && cmd.SysProcAttr.CmdLine != "" {
		cmdline = cmd.SysProcAttr.CmdLine
	}
	cmdlinep, err := windows.UTF16PtrFromString(cmdline)
	if err != nil {
		return 0, err
	}
	app, err := windows.UTF16PtrFromString(cmd.Path)
	if err != nil {
		return 0, err
	}
	var dir *uint16
	if cmd.Dir != "" {
		if dir, err = windows.UTF16PtrFromString(cmd.Dir); err != nil {
			return 0, err
		}
	}
	var env *uint16
	if cmd.Env != nil {
		env = environmentBlock(cmd.Env)
	}
	flags := uint32(windows.EXTENDED_STARTUPINFO_PRESENT | windows.CREATE_UNICODE_ENVIRONMENT)
	var pi windows.ProcessInformation
	if cmd.SysProcAttr != nil && cmd.SysProcAttr.Token != 0 {
		err = windows.CreateProcessAsUser(windows.Token(cmd.SysProcAttr.Token), app, cmdlinep, nil, nil, false, flags, env, dir, &si.StartupInfo, &pi)
	} else {
		err = windows.CreateProcess(app, cmdlinep, nil, nil, false, flags, env, dir, &si.StartupInfo, &pi)
	}
	if err != nil {
		return 0, &os.PathError{Op: "CreateProcess", Path: cmd.Path, Err: err}
	}
	windows.CloseHandle(pi.Thread)
	// The handle stays open until after FindProcess, so the PID cannot have
	// been reused by the time it is looked up.
	proc, err := os.FindProcess(int(pi.ProcessId))
	if err != nil {
		_ = windows.TerminateProcess(pi.Process, 1)
		windows.CloseHandle(pi.Process)
		return 0, err
	}
	cmd.Process = proc
	return pi.Process, nil
}

// environmentBlock is env as CreateProcess wants it: NUL-separated UTF-16,
// ending in an empty string, with one entry per name (the last one wins, as
// exec.Cmd does it, and names compare without case).
func environmentBlock(env []string) *uint16 {
	seen := map[string]bool{}
	var kept []string
	for i := len(env) - 1; i >= 0; i-- {
		name, _, _ := strings.Cut(env[i], "=")
		if env[i] != "" && strings.HasPrefix(env[i], "=") {
			// "=C:=C:\dir" entries carry per-drive directories; keep them.
			name = env[i]
		}
		key := strings.ToUpper(name)
		if seen[key] {
			continue
		}
		seen[key] = true
		kept = append(kept, env[i])
	}
	var block []uint16
	for i := len(kept) - 1; i >= 0; i-- {
		block = append(block, utf16.Encode([]rune(kept[i]))...)
		block = append(block, 0)
	}
	block = append(block, 0)
	if len(kept) == 0 {
		block = append(block, 0)
	}
	return &block[0]
}
