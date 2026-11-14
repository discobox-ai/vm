package machine_test

import (
	"errors"
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

// A strict URL request the driver cannot meet is refused; one it can, and any
// that is not strict, passes; a strict one with no mode is an error.
func TestURLRequestsCheck(t *testing.T) {
	caps := machine.URLCapabilities{Ingress: machine.URLOptional, Pool: machine.URLAlways}
	never := machine.URLCapabilities{Ingress: machine.URLAlways}
	on, off := machine.URLRequest{Mode: machine.URLOn, Strict: true}, machine.URLRequest{Mode: machine.URLOff, Strict: true}
	for _, c := range []struct {
		name        string
		caps        *machine.URLCapabilities
		req         machine.URLRequests
		unsupported bool
		invalid     bool
	}{
		{name: "the driver's default"},
		{name: "ingress off, strict", req: machine.URLRequests{Ingress: off}},
		{name: "ingress on, strict", req: machine.URLRequests{Ingress: on}},
		{name: "pool on, strict", req: machine.URLRequests{Pool: on}},
		{name: "pool off, strict, always given", req: machine.URLRequests{Pool: off}, unsupported: true},
		{name: "pool on, strict, never given", caps: &never, req: machine.URLRequests{Pool: on}, unsupported: true},
		{name: "ingress off, strict, always given", caps: &never, req: machine.URLRequests{Ingress: off}, unsupported: true},
		{name: "pool on, not strict, never given", caps: &never, req: machine.URLRequests{Pool: machine.URLRequest{Mode: machine.URLOn}}},
		{name: "pool off, not strict", req: machine.URLRequests{Pool: machine.URLRequest{Mode: machine.URLOff}}},
		{name: "strict with no mode", req: machine.URLRequests{Ingress: machine.URLRequest{Strict: true}}, invalid: true},
		{name: "an unknown mode", req: machine.URLRequests{Ingress: machine.URLRequest{Mode: "maybe"}}, invalid: true},
	} {
		reach := caps
		if c.caps != nil {
			reach = *c.caps
		}
		err := c.req.Check(reach)
		switch {
		case c.unsupported && !errors.Is(err, machine.ErrUnsupported):
			t.Errorf("%s: %v, want ErrUnsupported", c.name, err)
		case c.invalid && (err == nil || errors.Is(err, machine.ErrUnsupported)):
			t.Errorf("%s: %v, want an error that is not ErrUnsupported", c.name, err)
		case !c.unsupported && !c.invalid && err != nil:
			t.Errorf("%s: %v, want it passed", c.name, err)
		}
	}
}

// A scope is ingress or pool, and nothing else: not the scopes before them.
func TestParseURLScope(t *testing.T) {
	for _, scope := range machine.URLScopes {
		if got, err := machine.ParseURLScope(string(scope)); err != nil || got != scope {
			t.Errorf("parse %q = %q, %v", scope, got, err)
		}
	}
	for _, bad := range []string{"", "internal", "private", "public", "Ingress"} {
		if _, err := machine.ParseURLScope(bad); err == nil {
			t.Errorf("parse %q succeeded", bad)
		}
	}
}
