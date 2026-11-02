package guest

import (
	"os"
	"unsafe"

	"golang.org/x/sys/windows"
)

// processEnv is the environment a guest process starts with: the machine's
// environment as the registry holds it now, not the agent's own. The agent is
// a service started at boot, so its environment is frozen at that moment; a
// build step that installs Go or Node updates the machine PATH in the
// registry, and the next step must see it, as a new logon would.
//
// The block is made with the agent's own token: without one it holds the
// system's variables only, with no USERPROFILE, APPDATA or LOCALAPPDATA, and
// PowerShellGet, among others, fails to load without them.
func processEnv() []string {
	var token windows.Token
	if err := windows.OpenProcessToken(windows.CurrentProcess(), windows.TOKEN_QUERY|windows.TOKEN_DUPLICATE, &token); err != nil {
		return os.Environ()
	}
	defer token.Close()
	var block *uint16
	if err := windows.CreateEnvironmentBlock(&block, token, false); err != nil {
		return os.Environ()
	}
	defer func() { _ = windows.DestroyEnvironmentBlock(block) }()
	env := parseEnvironmentBlock(block)
	if len(env) == 0 {
		return os.Environ()
	}
	return env
}

// parseEnvironmentBlock reads a block CreateEnvironmentBlock made.
func parseEnvironmentBlock(block *uint16) []string {
	var env []string
	for p := unsafe.Pointer(block); ; {
		entry := windows.UTF16PtrToString((*uint16)(p))
		if entry == "" {
			break
		}
		env = append(env, entry)
		p = unsafe.Add(p, (len(windows.StringToUTF16(entry)))*2)
	}
	return env
}
