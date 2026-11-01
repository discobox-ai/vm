package machine_test

import (
	"testing"

	"github.com/discobox-ai/vm/pkg/machine"
)

func TestFences(t *testing.T) {
	none := machine.Capabilities{Egress: []machine.EgressFence{machine.EgressNone}}
	both := machine.Capabilities{Egress: []machine.EgressFence{machine.EgressNone, machine.EgressAllow}}
	var neither machine.Capabilities
	nothing, list := &machine.Egress{}, &machine.Egress{Allow: []string{"example.com"}}
	for _, c := range []struct {
		name string
		caps machine.Capabilities
		e    *machine.Egress
		want bool
	}{
		{"default on neither", neither, nil, true},
		{"nothing on neither", neither, nothing, false},
		{"nothing on none", none, nothing, true},
		{"a list on none", none, list, false},
		{"a list on both", both, list, true},
	} {
		if got := c.caps.Fences(c.e); got != c.want {
			t.Errorf("%s: Fences = %v, want %v", c.name, got, c.want)
		}
	}
}

func TestInstalls(t *testing.T) {
	caps := machine.Capabilities{GuestOS: []machine.OS{machine.Darwin, machine.Linux}, Install: []machine.OS{machine.Darwin}}
	if !caps.Installs(machine.Darwin) || caps.Installs(machine.Linux) || !caps.SupportsOS(machine.Linux) {
		t.Fatalf("a driver that boots darwin and linux and installs darwin: %+v", caps)
	}
}
