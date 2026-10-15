//go:build !windows

package guest

import (
	"fmt"
	"os/exec"
	"os/user"
	"slices"
	"strconv"
	"strings"
	"syscall"
)

// withLogin adds the HOME, USER, and LOGNAME of the agent's own account to env
// where it has none. The agent is a boot service (a systemd unit, a launchd
// daemon), which starts with none of them, and an installer that runs under
// `set -u` or writes to ~ fails without HOME.
func withLogin(env []string) []string {
	u, err := user.Current()
	if err != nil {
		return env
	}
	for _, kv := range [][2]string{{"HOME", u.HomeDir}, {"USER", u.Username}, {"LOGNAME", u.Username}} {
		if !slices.ContainsFunc(env, func(e string) bool { return strings.HasPrefix(e, kv[0]+"=") }) {
			env = append(env, kv[0]+"="+kv[1])
		}
	}
	return env
}

// runAs makes cmd run as the named account and returns the login environment
// a tool like Homebrew checks for, and a func to call once the process has
// exited. The agent must be root to switch.
func runAs(cmd *exec.Cmd, name string) ([]string, func(), error) {
	u, err := user.Lookup(name)
	if err != nil {
		return nil, nil, fmt.Errorf("guest: user %q: %w", name, err)
	}
	uid, err := strconv.ParseUint(u.Uid, 10, 32)
	if err != nil {
		return nil, nil, fmt.Errorf("guest: user %q has uid %q", name, u.Uid)
	}
	gid, err := strconv.ParseUint(u.Gid, 10, 32)
	if err != nil {
		return nil, nil, fmt.Errorf("guest: user %q has gid %q", name, u.Gid)
	}
	if cmd.SysProcAttr == nil {
		cmd.SysProcAttr = &syscall.SysProcAttr{}
	}
	cmd.SysProcAttr.Credential = &syscall.Credential{Uid: uint32(uid), Gid: uint32(gid)}
	return []string{"HOME=" + u.HomeDir, "USER=" + u.Username, "LOGNAME=" + u.Username}, func() {}, nil
}
