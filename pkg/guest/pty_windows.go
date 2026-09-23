package guest

import (
	"errors"
	"os/exec"
)

// ptyHandle is a started process's pseudo console.
//
// TODO(windows): ConPTY. CreatePseudoConsole over two pipes, then CreateProcess
// with EXTENDED_STARTUPINFO_PRESENT and PROC_THREAD_ATTRIBUTE_PSEUDOCONSOLE
// (x/sys/windows has both; exec.Cmd cannot pass the attribute, so the process
// is started by hand and cmd.Process adopted from its handle). Close must call
// ClosePseudoConsole after the process exits, or the output pipe never reaches
// EOF and the session's output copy never ends. sandboxw's guestexec OP_CONSOLE
// is the proven Rust version of exactly this.
type ptyHandle struct{}

func (ptyHandle) Read([]byte) (int, error)       { return 0, errPTY }
func (ptyHandle) Write([]byte) (int, error)      { return 0, errPTY }
func (ptyHandle) Close() error                   { return nil }
func (ptyHandle) Resize(rows, cols uint16) error { return errPTY }

var errPTY = errors.New("guest: a terminal (ConPTY) is not implemented in the Windows agent yet")

func startPTY(*exec.Cmd, uint16, uint16) (ptyHandle, error) { return ptyHandle{}, errPTY }
