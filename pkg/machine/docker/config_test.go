package docker

import (
	"strings"
	"testing"

	"github.com/discobox-ai/vm/pkg/machine"
)

// TestDecodeAppliesTheDefinitionsDefaults: --help and info show the
// definition's defaults, and a driver given no configuration runs with them.
func TestDecodeAppliesTheDefinitionsDefaults(t *testing.T) {
	cfg, err := decode(nil)
	if err != nil {
		t.Fatal(err)
	}
	got := map[string]string{"host": cfg.Host, "root": cfg.Root, "agent": cfg.Agent}
	for _, f := range Definition().ConfigFields {
		if got[f.Key] != f.Default {
			t.Errorf("%s: decode gives %q, the definition %q", f.Key, got[f.Key], f.Default)
		}
	}
}

// A driver given no agent falls back to nothing, on any host: the command
// line names one (ADR 26-11-14-352 §3), and the driver refuses to keep a
// volume without it, naming where it is configured.
func TestNoAgentIsNoFallback(t *testing.T) {
	d := newDriver(machine.Config{}, Config{Root: DefaultRoot}, HostDaemon("unix:///nonexistent", nil))
	if d.Agent != "" {
		t.Fatalf("agent %q, want none", d.Agent)
	}
	if _, err := d.helper(t.Context()); err == nil || !strings.Contains(err.Error(), AgentEnv) {
		t.Fatalf("helper: %v, want a refusal naming $%s", err, AgentEnv)
	}
}
