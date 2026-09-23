//go:build !windows

package engine

import (
	"os/exec"
	"syscall"
)

// detach puts the shim in its own session, so a Ctrl-C at the terminal that
// started it reaches the CLI and not the VM.
func detach(cmd *exec.Cmd) {
	cmd.SysProcAttr = &syscall.SysProcAttr{Setsid: true}
}

func processAlive(pid int) bool {
	if pid <= 0 {
		return false
	}
	err := syscall.Kill(pid, 0)
	return err == nil || err == syscall.EPERM
}
