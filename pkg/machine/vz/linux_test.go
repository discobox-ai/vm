package vz

import (
	"strings"
	"testing"
)

func TestCheckAgent(t *testing.T) {
	if err := checkAgent("", "arm64"); err == nil || !strings.Contains(err.Error(), AgentEnv) {
		t.Fatalf("no agent: %v, want it to name %s", err, AgentEnv)
	}
	if _, err := parseLinuxBase(t.TempDir()); err == nil || !strings.Contains(err.Error(), "no vmlinux") {
		t.Fatalf("a directory with no kernel: %v", err)
	}
}
