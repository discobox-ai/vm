package boxd

import (
	"encoding/json"
	"strconv"
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
	got := map[string]string{"addr": cfg.Addr, "tokenUrl": cfg.TokenURL, "region": cfg.Region, "inMachine": strconv.FormatBool(cfg.InMachine)}
	for _, f := range Definition().ConfigFields {
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
