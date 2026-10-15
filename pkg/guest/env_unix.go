//go:build !windows && !darwin

package guest

import "os"

// processEnv is the environment a guest process starts with: the agent's own,
// and its account's login variables where a boot service has none.
func processEnv() []string { return withLogin(os.Environ()) }
