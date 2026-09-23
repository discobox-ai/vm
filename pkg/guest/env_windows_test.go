package guest

import (
	"strings"
	"testing"
)

func TestProcessEnvReadsTheMachineEnvironment(t *testing.T) {
	env := processEnv()
	var path, systemRoot bool
	for _, entry := range env {
		name, _, _ := strings.Cut(entry, "=")
		switch strings.ToUpper(name) {
		case "PATH":
			path = true
		case "SYSTEMROOT":
			systemRoot = true
		}
	}
	if !path || !systemRoot {
		t.Fatalf("machine environment lacks PATH or SystemRoot: %v", env)
	}
}
