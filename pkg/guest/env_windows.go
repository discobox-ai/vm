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
func processEnv() []string {
	var block *uint16
	// No token: the system's variables only, which is what SYSTEM gets.
	if err := windows.CreateEnvironmentBlock(&block, 0, false); err != nil {
		return os.Environ()
	}
	defer windows.DestroyEnvironmentBlock(block)
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
