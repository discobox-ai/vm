package machine_test

import (
	"testing"

	"github.com/discobox-ai/vm/pkg/machine"
	_ "github.com/discobox-ai/vm/pkg/machine/drivers"
)

// TestInMachineAuth holds which drivers can authenticate as the machine they
// run on, and the field that turns it on: a driver that adds one is in this
// list or argued out of it, and an embedder learns of it from the
// definition.
func TestInMachineAuth(t *testing.T) {
	want := map[string]string{"boxd": "inMachine"}
	for _, name := range machine.Names() {
		if name == testDriver {
			continue
		}
		def, err := machine.DefinitionOf(name)
		if err != nil {
			t.Fatal(err)
		}
		field := ""
		if def.InMachine != nil {
			field = def.InMachine.Field
		}
		if field != want[name] {
			t.Errorf("%s: in-machine auth turned on by %q, want %q", name, field, want[name])
		}
	}
}
