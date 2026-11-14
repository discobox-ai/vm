package guest

import (
	"crypto/rand"
	"os/exec"
	"strings"
	"testing"
)

// An account that is not there, as one with another password, does not have
// its name for a password; the check only tries a logon.
func TestPasswordIsNameOfNoSuchAccount(t *testing.T) {
	name := "dvm-" + rand.Text()[:12]
	same, err := PasswordIsName(name)
	if err != nil {
		t.Fatal(err)
	}
	if same {
		t.Fatalf("%s: its password is its name", name)
	}
}

// Running as an account neither its name nor a blank password logs on to,
// as an image installed with --option password has it, is refused, and the
// error says so.
func TestRunAsAnAccountWithAPasswordOfItsOwnIsUnsupported(t *testing.T) {
	name := "dvm-" + rand.Text()[:12]
	_, _, err := runAs(exec.CommandContext(t.Context(), "cmd.exe", "/c", "exit"), name)
	if err == nil || !strings.Contains(err.Error(), "unsupported") || !strings.Contains(err.Error(), "--option password") {
		t.Fatalf("runAs(%s) = %v, want it refused as unsupported", name, err)
	}
}
