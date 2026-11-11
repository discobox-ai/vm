package boxd

import (
	"context"
	"errors"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/discobox-ai/vm/pkg/machine"
	"github.com/discobox-ai/vm/pkg/machine/boxd/internal/boxdapi"
)

// TestRegionsKeepPoolsApart checks that a driver makes and finds its VMs and
// committed VMs in its region alone: a pool in another region sees none of
// them and can use their names, and keys are still the home endpoint's.
func TestRegionsKeepPoolsApart(t *testing.T) {
	if runtime.GOOS != "linux" {
		t.Skip("the fake boxd's guests run on the host, and boxd guests are linux")
	}
	ctx := context.Background()
	dir := t.TempDir()
	fra, home := fakeDriver(t, dir)
	nycAPI := fakeRegion(t, home, dir, "nyc", "enabled")
	nyc := fakeIn(t, home, "nyc", machine.DefaultPool, fra.api.keys)

	inst, err := nyc.Create(ctx, machine.InstanceSpec{Name: "web"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := nyc.Instance(ctx, "web"); err != nil {
		t.Fatalf("nyc's own VM: %v", err)
	}
	if list, err := fra.Instances(ctx); err != nil || len(list) != 0 {
		t.Fatalf("fra lists %v, %v; want none of nyc's", list, err)
	}
	if _, err := fra.Instance(ctx, "web"); !errors.Is(err, machine.ErrNotFound) {
		t.Fatalf("fra gets nyc's VM: %v, want ErrNotFound", err)
	}
	// A name is the region's, as boxd's machine names are.
	there, err := fra.Create(ctx, machine.InstanceSpec{Name: "web"})
	if err != nil {
		t.Fatalf("fra makes a VM of nyc's VM's name: %v", err)
	}
	if err := fra.Destroy(ctx, there); err != nil {
		t.Fatal(err)
	}

	committed, err := nyc.Commit(ctx, inst)
	if err != nil {
		t.Fatal(err)
	}
	nycAPI.mu.Lock()
	snapshots := len(nycAPI.snapshots)
	nycAPI.mu.Unlock()
	home.mu.Lock()
	homeSnapshots := len(home.snapshots)
	home.mu.Unlock()
	if snapshots != 1 || homeSnapshots != 0 {
		t.Fatalf("a commit in nyc made %d snapshots there and %d in fra", snapshots, homeSnapshots)
	}
	if list, err := fra.Instances(ctx); err != nil || len(list) != 0 {
		t.Fatalf("fra lists %v, %v; want none of nyc's committed VMs", list, err)
	}
	if _, err := fra.Create(ctx, machine.InstanceSpec{Name: "copy", From: &committed}); err == nil {
		t.Fatal("fra made a VM from nyc's committed VM")
	}
	copied, err := nyc.Create(ctx, machine.InstanceSpec{Name: "copy", From: &committed})
	if err != nil {
		t.Fatalf("nyc makes a VM from its committed VM: %v", err)
	}

	// Keys are the account's, at the home endpoint, whatever the region.
	if _, err := nyc.IssueKey(ctx, machine.IssueRequest{Name: "agent", TTL: time.Hour}); err != nil {
		t.Fatalf("issue a key from nyc: %v", err)
	}
	if keys, err := fra.IssuedKeys(ctx, "agent"); err != nil || len(keys) != 1 {
		t.Fatalf("fra's keys of the name: %v, %v; want nyc's one", keys, err)
	}

	for _, i := range []machine.Instance{copied, committed} {
		if err := nyc.Destroy(ctx, i); err != nil {
			t.Fatal(err)
		}
	}
}

// TestRegionRefused checks that a region boxd does not have, or the key's
// org has not enabled, fails Check and every call, naming why, and that the
// driver asks again rather than keep the failure.
func TestRegionRefused(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	fra, home := fakeDriver(t, dir)
	fakeRegion(t, home, dir, "nyc", "available")

	unknown := fakeIn(t, home, "sfo", machine.DefaultPool, fra.api.keys)
	if err := unknown.Check(ctx); err == nil || !strings.Contains(err.Error(), `no region "sfo"`) || !strings.Contains(err.Error(), "fra, nyc") {
		t.Fatalf("Check in an unknown region: %v", err)
	}
	if _, err := unknown.Instances(ctx); err == nil {
		t.Fatal("listed an unknown region")
	}

	nyc := fakeIn(t, home, "nyc", machine.DefaultPool, fra.api.keys)
	if err := nyc.Check(ctx); err == nil || !strings.Contains(err.Error(), "not enabled") {
		t.Fatalf("Check in a region the org has not enabled: %v", err)
	}
	if _, err := nyc.Create(ctx, machine.InstanceSpec{Name: "web"}); err == nil {
		t.Fatal("created in a region the org has not enabled")
	}
	home.mu.Lock()
	home.regions[1].OrgState = "enabled"
	home.mu.Unlock()
	if err := nyc.Check(ctx); err != nil {
		t.Fatalf("Check once the org has the region: %v", err)
	}

	// An endpoint that does not answer fails Check, not the first create.
	home.mu.Lock()
	home.regions = append(home.regions, &boxdapi.Region{Id: "gone", ApiEndpoint: "127.0.0.1:1", OrgState: "enabled"})
	home.mu.Unlock()
	short, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	if err := fakeIn(t, home, "gone", machine.DefaultPool, fra.api.keys).Check(short); err == nil || !strings.Contains(err.Error(), "region gone") {
		t.Fatalf("Check with a region endpoint that does not answer: %v", err)
	}

	// Naming the home region is its endpoint, as no region is.
	named := fakeIn(t, home, "fra", machine.DefaultPool, fra.api.keys)
	if err := named.Check(ctx); err != nil {
		t.Fatalf("Check in the home region by name: %v", err)
	}
}

func TestEndpointAddr(t *testing.T) {
	for _, c := range []struct{ endpoint, want string }{
		{"nyc.boxd.sh:9443", "nyc.boxd.sh:9443"},
		{"nyc.boxd.sh", "nyc.boxd.sh:9443"},
		{"https://nyc.boxd.sh", "nyc.boxd.sh:443"},
		{"https://nyc.boxd.sh:443", "nyc.boxd.sh:443"},
		{"https://127.0.0.1:1234/", "127.0.0.1:1234"},
	} {
		if got, err := endpointAddr(c.endpoint, "boxd.sh:9443"); err != nil || got != c.want {
			t.Errorf("endpointAddr(%q) = %q, %v; want %q", c.endpoint, got, err, c.want)
		}
	}
	for _, endpoint := range []string{"", "https://", "grpcs://nyc.boxd.sh"} {
		if got, err := endpointAddr(endpoint, "boxd.sh:9443"); err == nil {
			t.Errorf("endpointAddr(%q) = %q, want an error", endpoint, got)
		}
	}
}
