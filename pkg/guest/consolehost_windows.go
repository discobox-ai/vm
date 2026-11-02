package guest

import (
	"io"
	"os"
	"os/exec"
	"strconv"
	"sync"
	"syscall"

	"golang.org/x/sys/windows"
)

// A pseudo console belongs to the account of the process that made it. One
// the agent makes is SYSTEM's, and a process of another account attached to it
// may write the screen but not read it back: ReadConsoleOutput fails with
// "Access is denied", and Windows PowerShell reads the screen to draw every
// progress bar, so Invoke-WebRequest fails. So a TTY session as another
// account gets its console from a console host, the agent's own binary run as
// that account (`disco-vm console-host`), which makes the console and starts
// the command on it.
//
// The host's stdin carries frames, FrameStdin and FrameResize as the session
// has them; its stdout is the console's output, raw; its exit code is the
// command's.

// startHostedPTY starts a console host as cmd's token, running cmd, and adopts
// the host as cmd.Process.
func startHostedPTY(cmd *exec.Cmd, rows, cols uint16) (ptyHandle, error) {
	self, err := os.Executable()
	if err != nil {
		return nil, err
	}
	cmdline := windows.ComposeCommandLine(cmd.Args)
	if cmd.SysProcAttr.CmdLine != "" {
		cmdline = cmd.SysProcAttr.CmdLine
	}
	inRead, inWrite, err := os.Pipe()
	if err != nil {
		return nil, err
	}
	outRead, outWrite, err := os.Pipe()
	if err != nil {
		_ = inRead.Close()
		_ = inWrite.Close()
		return nil, err
	}
	//nolint:gosec,noctx // G204: the agent's own binary; the session, not a ctx, bounds the process
	host := exec.Command(self, "console-host", strconv.Itoa(int(rows)), strconv.Itoa(int(cols)), cmd.Path, cmdline)
	host.Env, host.Dir = cmd.Env, cmd.Dir
	// The host is a console program; without CREATE_NO_WINDOW it would get a
	// console of its own that nothing shows.
	host.SysProcAttr = &syscall.SysProcAttr{Token: cmd.SysProcAttr.Token, CreationFlags: windows.CREATE_NO_WINDOW}
	// Its errors go to the screen, where the session shows them.
	host.Stdin, host.Stdout, host.Stderr = inRead, outWrite, outWrite
	err = host.Start()
	// The host holds its own ends; ours would keep its output from ending.
	_ = inRead.Close()
	_ = outWrite.Close()
	if err != nil {
		_ = inWrite.Close()
		_ = outRead.Close()
		return nil, err
	}
	cmd.Process = host.Process
	return &hostedPTY{in: inWrite, out: outRead}, nil
}

// hostedPTY is a console host's pipes, seen as the console itself.
type hostedPTY struct {
	mu  sync.Mutex
	in  *os.File
	out *os.File
}

func (p *hostedPTY) Read(b []byte) (int, error) { return p.out.Read(b) }

func (p *hostedPTY) Write(b []byte) (int, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if err := WriteFrame(p.in, FrameStdin, b); err != nil {
		return 0, err
	}
	return len(b), nil
}

func (p *hostedPTY) Resize(rows, cols uint16) error {
	p.mu.Lock()
	defer p.mu.Unlock()
	return WriteFrame(p.in, FrameResize, resizePayload(rows, cols))
}

func (p *hostedPTY) Close() error {
	_ = p.in.Close()
	return p.out.Close()
}

// ConsoleHost is `disco-vm console-host`: it runs cmdline (app is its
// program) on a pseudo console of its own, reading frames from in and writing
// the console's output to out, and returns the command's exit code. The input
// ending closes the console, as closing a terminal window does.
func ConsoleHost(rows, cols uint16, app, cmdline string, in io.Reader, out io.Writer) (int, error) {
	cmd := &exec.Cmd{Path: app, Args: []string{app}, SysProcAttr: &syscall.SysProcAttr{CmdLine: cmdline}}
	console, err := startConPTY(cmd, rows, cols)
	if err != nil {
		return -1, err
	}
	defer console.Close()
	go func() {
		for {
			typ, payload, err := ReadFrame(in)
			if err != nil {
				console.closeConsole()
				return
			}
			switch typ {
			case FrameStdin:
				_, _ = console.Write(payload)
			case FrameResize:
				if len(payload) == 4 {
					_ = console.Resize(uint16(payload[0])<<8|uint16(payload[1]), uint16(payload[2])<<8|uint16(payload[3]))
				}
			}
		}
	}()
	copied := make(chan struct{})
	go func() {
		_, _ = io.Copy(out, console)
		close(copied)
	}()
	err = cmd.Wait()
	<-copied
	return exitCode(err), nil
}
