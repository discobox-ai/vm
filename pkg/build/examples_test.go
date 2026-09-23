package build

import (
	"path/filepath"
	"testing"

	"github.com/discobox-ai/vm/pkg/machine"
)

// The shipped examples must stay valid specs that plan on both guest OSes.
func TestExamplesPlan(t *testing.T) {
	files, err := filepath.Glob("../../examples/*.yaml")
	if err != nil || len(files) == 0 {
		t.Fatalf("no examples: %v", err)
	}
	for _, file := range files {
		spec, err := Load(file)
		if err != nil {
			t.Errorf("%s: %v", file, err)
			continue
		}
		for _, os := range []machine.OS{machine.Windows, machine.Darwin} {
			if _, err := plan(spec, spec.Args, filepath.Dir(file), os); err != nil {
				t.Errorf("%s on %s: %v", file, os, err)
			}
		}
	}
}
