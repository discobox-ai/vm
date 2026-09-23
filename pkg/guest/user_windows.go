package guest

import (
	"errors"
	"os/exec"
)

// runAs runs cmd as another account.
//
// TODO(windows): the agent runs as SYSTEM, which is what most build steps
// want. Running as the interactive user needs that user's token: from its
// session (WTSQueryUserToken) when logged on, or LogonUser with credentials
// the image's unattend created. The token goes in cmd.SysProcAttr.Token.
func runAs(*exec.Cmd, string) ([]string, error) {
	return nil, errors.New("guest: running as another user is not implemented in the Windows agent yet")
}
