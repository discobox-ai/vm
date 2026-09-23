//go:build !windows

package guest

import "os"

// processEnv is the environment a guest process starts with.
func processEnv() []string { return os.Environ() }
