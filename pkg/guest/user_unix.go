//go:build !windows

package guest

import (
	"fmt"
	"os/exec"
	"os/user"
	"strconv"
	"syscall"
)

// runAs makes cmd run as the named account and returns the login environment
// a tool like Homebrew checks for. The agent must be root to switch.
func runAs(cmd *exec.Cmd, name string) ([]string, error) {
	u, err := user.Lookup(name)
	if err != nil {
		return nil, fmt.Errorf("guest: user %q: %w", name, err)
	}
	uid, err := strconv.ParseUint(u.Uid, 10, 32)
	if err != nil {
		return nil, fmt.Errorf("guest: user %q has uid %q", name, u.Uid)
	}
	gid, err := strconv.ParseUint(u.Gid, 10, 32)
	if err != nil {
		return nil, fmt.Errorf("guest: user %q has gid %q", name, u.Gid)
	}
	if cmd.SysProcAttr == nil {
		cmd.SysProcAttr = &syscall.SysProcAttr{}
	}
	cmd.SysProcAttr.Credential = &syscall.Credential{Uid: uint32(uid), Gid: uint32(gid)}
	return []string{"HOME=" + u.HomeDir, "USER=" + u.Username, "LOGNAME=" + u.Username}, nil
}
