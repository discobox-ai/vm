package vz

import (
	"bytes"
	"os"
	"path/filepath"
	"testing"
)

// The agent's disk is an ISO 9660 image named agentVolume: its primary
// volume descriptor, at sector 16, carries the name, and the agent is on it
// whole.
func TestWriteAgentISO(t *testing.T) {
	dir := t.TempDir()
	agent := filepath.Join(dir, "disco-vm")
	payload := bytes.Repeat([]byte("this host's disco-vm "), 4096)
	if err := os.WriteFile(agent, payload, 0o755); err != nil {
		t.Fatal(err)
	}
	iso := filepath.Join(dir, agentISOName)
	if err := os.WriteFile(iso, []byte("a stale disk"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := writeAgentISO(t.Context(), iso, agent); err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(iso)
	if err != nil {
		t.Fatal(err)
	}
	const sector = 2048
	if len(data)%sector != 0 || len(data) < 17*sector {
		t.Fatalf("the disk is %d bytes", len(data))
	}
	pvd := data[16*sector:]
	if pvd[0] != 1 || string(pvd[1:6]) != "CD001" {
		t.Fatalf("no primary volume descriptor: % x", pvd[:8])
	}
	if got := string(bytes.TrimRight(pvd[40:72], " \x00")); got != agentVolume {
		t.Fatalf("the volume is %q, not %q", got, agentVolume)
	}
	if !bytes.Contains(data, payload) {
		t.Fatal("the agent is not on the disk whole")
	}
	left, err := filepath.Glob(filepath.Join(dir, ".agent-iso-*"))
	if err != nil || len(left) != 0 {
		t.Fatalf("left behind: %v, %v", left, err)
	}
}
