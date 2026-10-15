//go:build !windows

package guest

import (
	"os/user"
	"slices"
	"testing"
)

func TestWithLogin(t *testing.T) {
	u, err := user.Current()
	if err != nil {
		t.Skip(err)
	}
	// A boot service's environment has no HOME; one that has it keeps it.
	env := withLogin([]string{"PATH=/bin", "USER=kept"})
	for _, want := range []string{"PATH=/bin", "USER=kept", "HOME=" + u.HomeDir, "LOGNAME=" + u.Username} {
		if !slices.Contains(env, want) {
			t.Errorf("env %v lacks %s", env, want)
		}
	}
	if len(env) != 4 {
		t.Errorf("env %v, want 4 entries", env)
	}
}
