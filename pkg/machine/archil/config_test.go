package archil

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/discobox-ai/vm/pkg/machine"
)

// TestDecodeAppliesTheDefinitionsDefaults: --help and info show the
// definition's defaults, and a driver given only its region runs with them.
func TestDecodeAppliesTheDefinitionsDefaults(t *testing.T) {
	cfg, err := decode(json.RawMessage(`{"region":"aws-us-east-1"}`))
	if err != nil {
		t.Fatal(err)
	}
	got := map[string]string{"region": cfg.Region, "baseUrl": cfg.BaseURL}
	for _, f := range Definition().ConfigFields {
		if f.Required {
			continue
		}
		if got[f.Key] != f.Default {
			t.Errorf("%s: decode gives %q, the definition %q", f.Key, got[f.Key], f.Default)
		}
	}
}

// TestRegionPicksTheControlPlane: the configuration's region is where the
// driver calls, and a baseUrl wins over it.
func TestRegionPicksTheControlPlane(t *testing.T) {
	for _, c := range []struct {
		name, config, region, base string
	}{
		{"aws-us-east-1", `{"region":"aws-us-east-1"}`, "aws-us-east-1", controlPlanes["aws-us-east-1"]},
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
	if controlPlanes["aws-eu-west-1"] == controlPlanes["aws-us-east-1"] {
		t.Fatal("two regions share a control plane, so the test proves nothing")
	}
}

// TestRegionIsRequired: a configuration with no region is refused, by the
// driver and by the seam's Normalize, naming the field and where a pool made
// before it was required keeps its VMs, so a pool never lists a region other
// than the one it made its VMs in. One that runs no sandboxes is refused too.
func TestRegionIsRequired(t *testing.T) {
	for _, config := range []string{``, `{}`, `{"region":""}`, `{"baseUrl":"http://127.0.0.1:8080"}`} {
		if err := Validate(json.RawMessage(config)); err == nil || !strings.Contains(err.Error(), "region is required") || !strings.Contains(err.Error(), legacyRegion) {
			t.Errorf("Validate(%s): %v, want region is required, naming %s", config, err, legacyRegion)
		}
		if _, err := New(machine.Config{DriverConfig: json.RawMessage(config)}); err == nil {
			t.Errorf("New(%s) made a driver with no region", config)
		}
	}
	if _, err := machine.Normalize("archil", nil); err == nil || !strings.Contains(err.Error(), "region is required") {
		t.Errorf("Normalize with no region: %v, want region is required", err)
	}
	if err := Validate(json.RawMessage(`{"region":"gcp-us-central1"}`)); err == nil || !strings.Contains(err.Error(), "runs sandboxes") {
		t.Errorf("a region with no sandboxes: %v", err)
	}
	if f, ok := Definition().Field("region"); !ok || !f.Required || f.Default != "" || f.Placeholder != legacyRegion {
		t.Errorf("region is %+v: required, with no default, and %s as its placeholder", f, legacyRegion)
	}
}
