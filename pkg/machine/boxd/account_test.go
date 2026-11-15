package boxd

import (
	"errors"
	"slices"
	"strings"
	"testing"

	"github.com/discobox-ai/vm/pkg/machine"
)

// TestCheckRefusesHostBoundSecrets: Check fails, naming each secret, its
// scope, and its hosts, when a host-bound secret of the key's org reaches
// the driver's machines (scopes all and private, and none, as a personal one
// may be listed), with a key and with a machine's own token; and passes with
// plain secrets anywhere, and host-bound ones only where they reach no
// machine of the driver's (shared, unattached, private-unattached).
func TestCheckRefusesHostBoundSecrets(t *testing.T) {
	type secret struct {
		name, scope string
		domains     []string
	}
	for _, c := range []struct {
		name    string
		secrets []secret
		// refused are what the error names; none is a pass.
		refused []string
	}{
		{"none", nil, nil},
		{"plain secrets only", []secret{
			{"PLAIN_ALL", "all", nil},
			{"PLAIN_PRIVATE", "private", nil},
			{"PLAIN_NONE", "", nil},
		}, nil},
		{"host-bound where no machine of the driver's is", []secret{
			{"SHARED", "shared", []string{"api.example.com"}},
			{"UNATTACHED", "unattached", []string{"api.example.com"}},
			{"MINE_UNATTACHED", "private-unattached", []string{"api.example.com"}},
		}, nil},
		{"all", []secret{
			{"STRIPE_KEY", "all", []string{"api.stripe.com"}},
			{"PLAIN", "all", nil},
		}, []string{"STRIPE_KEY (scope all; hosts api.stripe.com)"}},
		{"the owner's private", []secret{
			{"GH_TOKEN", "private", []string{"api.github.com", "*.githubusercontent.com"}},
		}, []string{"GH_TOKEN (scope private; hosts api.github.com, *.githubusercontent.com)"}},
		{"no scope", []secret{
			{"NPM_TOKEN", "", []string{"registry.npmjs.org"}},
			{"SHARED", "shared", []string{"api.example.com"}},
		}, []string{"NPM_TOKEN (scope none listed; set one explicitly; hosts registry.npmjs.org)"}},
		{"every one named", []secret{
			{"A", "all", []string{"a.example.com"}},
			{"B", "private", []string{"b.example.com"}},
		}, []string{"org fake-org", "A (scope all", "B (scope private"}},
	} {
		t.Run(c.name, func(t *testing.T) {
			keyed, fake := fakeDriver(t, t.TempDir())
			for _, s := range c.secrets {
				fake.setSecret(s.name, s.scope, s.domains...)
			}
			adapter := newFakeAdapter(t, fake)
			for _, d := range map[string]*Driver{"key": keyed, "machine token": fakeInMachine(t, fake, adapter.URL+metadataPath)} {
				err := d.Check(t.Context())
				if len(c.refused) == 0 {
					if err != nil {
						t.Fatalf("check: %v", err)
					}
					continue
				}
				if err == nil {
					t.Fatalf("check passed with %v", c.refused)
				}
				for _, want := range c.refused {
					if !strings.Contains(err.Error(), want) {
						t.Errorf("check: %v\nnames no %q", err, want)
					}
				}
				for _, s := range c.secrets {
					named := slices.ContainsFunc(c.refused, func(r string) bool { return strings.HasPrefix(r, s.name+" ") })
					if !named && strings.Contains(err.Error(), s.name+" (") {
						t.Errorf("check: %v\nnames %s, which reaches no machine of the driver's", err, s.name)
					}
				}
			}
		})
	}
}

// TestALimitNotReadIsNoCap: a credential boxd does not let read the org's
// machine limit (a machine's token, which boxd does not document reading
// it) passes Check with no cap reported, and its secrets still checked.
func TestALimitNotReadIsNoCap(t *testing.T) {
	d, fake := fakeDriver(t, t.TempDir())
	fake.limitRefused.Store(true)
	if err := d.Check(t.Context()); err != nil {
		t.Fatalf("check with the limit refused: %v", err)
	}
	if caps := d.Capabilities(); caps.MaxMachines != nil {
		t.Fatalf("MaxMachines with the limit refused: %v, want none", caps.MaxMachines)
	}
	fake.setSecret("STRIPE_KEY", "all", "api.stripe.com")
	if err := d.Check(t.Context()); err == nil || !strings.Contains(err.Error(), "STRIPE_KEY (scope all") || !strings.Contains(err.Error(), "this key's org") {
		t.Fatalf("check with the limit refused and a host-bound secret: %v", err)
	}
}

// TestMaxMachinesIsTheOrgsLimit: a driver reports no cap until Check has
// read the org's machine limit, and then reports it as MaxMachines; a create
// past it, which boxd refuses, is ErrCapacity, stopped machines counted, and
// a destroy makes room.
func TestMaxMachinesIsTheOrgsLimit(t *testing.T) {
	d, fake := fakeDriver(t, t.TempDir())
	const limit = 3
	fake.maxVMs.Store(limit)
	ctx := t.Context()

	if caps := d.Capabilities(); caps.MaxMachines != nil {
		t.Fatalf("MaxMachines before Check: %v, want none", caps.MaxMachines)
	}
	if err := d.Check(ctx); err != nil {
		t.Fatal(err)
	}
	if got := d.Capabilities().MaxMachines[machine.Linux]; got != limit {
		t.Fatalf("MaxMachines[linux] = %d, want the org's limit, %d", got, limit)
	}

	// A machine of the org's that is not the pool's counts.
	fake.machines.Add(1)
	var made []machine.Instance
	for _, name := range []string{"one", "two"} {
		inst, err := d.Create(ctx, machine.InstanceSpec{Name: name})
		if err != nil {
			t.Fatalf("create %s: %v", name, err)
		}
		made = append(made, inst)
	}
	// Every VM the driver made is stopped, and still counts.
	_, err := d.Create(ctx, machine.InstanceSpec{Name: "three"})
	if !errors.Is(err, machine.ErrCapacity) {
		t.Fatalf("a create past the org's limit: %v, want ErrCapacity", err)
	}
	if !strings.Contains(err.Error(), "fake-org has 3 machines") {
		t.Errorf("a create past the org's limit: %v, names no org and count", err)
	}
	if _, err := d.Instance(ctx, "three"); !errors.Is(err, machine.ErrNotFound) {
		t.Fatalf("a refused create left %v", err)
	}
	if err := d.Destroy(ctx, made[0]); err != nil {
		t.Fatal(err)
	}
	if _, err := d.Create(ctx, machine.InstanceSpec{Name: "three"}); err != nil {
		t.Fatalf("create once a destroy made room: %v", err)
	}
}
