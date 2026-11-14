package guest

import (
	"crypto/rand"
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
