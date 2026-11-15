package boxd

import (
	"encoding/json"
	"strconv"
	"strings"
	"testing"

	"github.com/discobox-ai/vm/pkg/machine"
)

// TestDecodeAppliesTheDefinitionsDefaults: --help and info show the
// definition's defaults, and a driver given only its region runs with them.
func TestDecodeAppliesTheDefinitionsDefaults(t *testing.T) {
	cfg, err := decode(json.RawMessage(`{"region":"fra"}`))
	if err != nil {
		t.Fatal(err)
	}
	got := map[string]string{"addr": cfg.Addr, "tokenUrl": cfg.TokenURL, "region": cfg.Region, "inMachine": strconv.FormatBool(cfg.InMachine)}
	for _, f := range Definition().ConfigFields {
		if f.Required {
			continue
		}
		if got[f.Key] != f.Default {
			t.Errorf("%s: decode gives %q, the definition %q", f.Key, got[f.Key], f.Default)
		}
	}
}

// TestRegionIsTheDriversConfiguration: a region is a field of boxd's own
// configuration, decoded as given and reaching the client, which calls it.
func TestRegionIsTheDriversConfiguration(t *testing.T) {
	d, err := New(machine.Config{DriverConfig: json.RawMessage(`{"region":"nyc"}`)})
	if err != nil {
		t.Fatal(err)
	}
	if d.api.region != "nyc" || d.api.addr != defaultAddr {
		t.Fatalf("region %q at %q, want nyc at %q", d.api.region, d.api.addr, defaultAddr)
	}
	if err := Validate(json.RawMessage(`{"region":"nyc"}`)); err != nil {
		t.Fatalf("Validate a region: %v", err)
	}
}

// TestRegionIsRequired: a configuration with no region is refused, by the
// driver and by the seam's Normalize, naming the field, so a pool never
// follows boxd's default region, which can change under it.
func TestRegionIsRequired(t *testing.T) {
	for _, config := range []string{``, `{}`, `{"region":""}`} {
		if err := Validate(json.RawMessage(config)); err == nil || !strings.Contains(err.Error(), "region is required") {
			t.Errorf("Validate(%s): %v, want region is required", config, err)
		}
		if _, err := New(machine.Config{DriverConfig: json.RawMessage(config)}); err == nil {
			t.Errorf("New(%s) made a driver with no region", config)
		}
	}
	if _, err := machine.Normalize("boxd", json.RawMessage(`{"inMachine":true}`)); err == nil || !strings.Contains(err.Error(), "region is required") {
		t.Errorf("Normalize with no region: %v, want region is required", err)
	}
	if f, ok := Definition().Field("region"); !ok || !f.Required || f.Default != "" {
		t.Errorf("region is %+v: required, with no default", f)
	}
}
