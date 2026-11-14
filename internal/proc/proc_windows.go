package proc

import (
	"os/exec"
	"syscall"

	"golang.org/x/sys/windows"
)

// Detach starts the shim without a console and outside the CLI's process
// group, so closing the terminal or a Ctrl-C there does not reach the VM.
func Detach(cmd *exec.Cmd) {
	cmd.SysProcAttr = &syscall.SysProcAttr{
		CreationFlags: windows.CREATE_NEW_PROCESS_GROUP | windows.DETACHED_PROCESS,
		HideWindow:    true,
	}
}

// NoWindow starts a console program with a console of its own that shows no
// window. A console program whose parent has none (a shim, which Detach
// starts with no console) is otherwise given a new console, and where
// Windows Terminal is the default terminal that is a window, left open on
// a non-zero exit: one for every fake guest a test kills.
func NoWindow(cmd *exec.Cmd) {
	if cmd.SysProcAttr == nil {
		cmd.SysProcAttr = &syscall.SysProcAttr{}
	}
	cmd.SysProcAttr.CreationFlags |= windows.CREATE_NO_WINDOW
}

// Alive reports whether a process of the PID runs.
func Alive(pid int) bool {
	if pid <= 0 {
		return false
	}
	h, err := windows.OpenProcess(windows.PROCESS_QUERY_LIMITED_INFORMATION, false, uint32(pid))
	if err != nil {
		return false
	}
	defer func() { _ = windows.CloseHandle(h) }()
	var code uint32
	if err := windows.GetExitCodeProcess(h, &code); err != nil {
		return false
	}
	const stillActive = 259
	return code == stillActive
}
