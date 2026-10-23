//go:build !windows

package proc

import (
	"os/exec"
	"syscall"
)

// Detach puts the shim in its own session, so a Ctrl-C at the terminal that
// started it reaches the CLI and not the VM.
func Detach(cmd *exec.Cmd) {
	cmd.SysProcAttr = &syscall.SysProcAttr{Setsid: true}
}

// Alive reports whether a process of the PID runs.
func Alive(pid int) bool {
	if pid <= 0 {
		return false
	}
	err := syscall.Kill(pid, 0)
	return err == nil || err == syscall.EPERM
}
