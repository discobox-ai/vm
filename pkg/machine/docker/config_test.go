package docker

import "testing"

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
