package machine_test

import (
	"testing"

	"github.com/discobox-ai/vm/pkg/machine"
	_ "github.com/discobox-ai/vm/pkg/machine/drivers"
)

// TestSizes holds which drivers declare that they size a machine
// (Definition.Sizes): every one, today. The conformance suite holds a
// declaring driver to it, reading back the size its provider applied
// (machinetest.Config.Size); one that stops applying a size is taken out
// of this list, and an embedder then refuses a size for it.
func TestSizes(t *testing.T) {
	sizes := map[string]bool{"archil": true, "boxd": true, "docker": true, "fake": true, "hcs": true, "libkrun": true, "vz": true}
	for _, name := range machine.Names() {
		if name == testDriver {
			continue
		}
		def, err := machine.DefinitionOf(name)
		if err != nil {
			t.Fatal(err)
		}
		if def.Sizes != sizes[name] {
			t.Errorf("%s: sizes a machine %t, want %t", name, def.Sizes, sizes[name])
		}
	}
}
