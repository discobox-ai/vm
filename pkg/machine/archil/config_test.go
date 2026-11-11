package archil

import (
	"encoding/json"
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
	got := map[string]string{"region": cfg.Region, "baseUrl": cfg.BaseURL}
	for _, f := range Definition().ConfigFields {
		if got[f.Key] != f.Default {
			t.Errorf("%s: decode gives %q, the definition %q", f.Key, got[f.Key], f.Default)
		}
	}
}

// TestRegionPicksTheControlPlane: the configuration's region is where the
// driver calls, the default's when it names none, and a baseUrl wins over it.
func TestRegionPicksTheControlPlane(t *testing.T) {
	for _, c := range []struct {
		name, config, region, base string
	}{
		{"default", `{}`, DefaultRegion, controlPlanes[DefaultRegion]},
		{"another region", `{"region":"aws-eu-west-1"}`, "aws-eu-west-1", controlPlanes["aws-eu-west-1"]},
		{"baseUrl wins", `{"region":"aws-eu-west-1","baseUrl":"http://127.0.0.1:8080/"}`, "aws-eu-west-1", "http://127.0.0.1:8080"},
	} {
		t.Run(c.name, func(t *testing.T) {
			d, err := New(machine.Config{DriverConfig: json.RawMessage(c.config)})
			if err != nil {
				t.Fatal(err)
			}
			if d.api.region != c.region || d.api.base != c.base {
				t.Fatalf("region %q at %q, want %q at %q", d.api.region, d.api.base, c.region, c.base)
			}
		})
	}
	if controlPlanes["aws-eu-west-1"] == controlPlanes[DefaultRegion] {
		t.Fatal("two regions share a control plane, so the test proves nothing")
	}
}
