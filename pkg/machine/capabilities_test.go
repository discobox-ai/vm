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
	caps := machine.URLCapabilities{Private: machine.URLAlways, Public: machine.URLOptional}
	on, off := machine.URLRequest{Mode: machine.URLOn, Strict: true}, machine.URLRequest{Mode: machine.URLOff, Strict: true}
	for _, c := range []struct {
		name        string
		req         machine.URLRequests
		unsupported bool
		invalid     bool
	}{
		{name: "the driver's default"},
		{name: "public off, strict", req: machine.URLRequests{Public: off}},
		{name: "public on, strict", req: machine.URLRequests{Public: on}},
		{name: "private on, strict", req: machine.URLRequests{Private: on}},
		{name: "private off, strict, always given", req: machine.URLRequests{Private: off}, unsupported: true},
		{name: "internal on, strict, never given", req: machine.URLRequests{Internal: on}, unsupported: true},
		{name: "internal on, not strict", req: machine.URLRequests{Internal: machine.URLRequest{Mode: machine.URLOn}}},
		{name: "private off, not strict", req: machine.URLRequests{Private: machine.URLRequest{Mode: machine.URLOff}}},
		{name: "strict with no mode", req: machine.URLRequests{Public: machine.URLRequest{Strict: true}}, invalid: true},
		{name: "an unknown mode", req: machine.URLRequests{Public: machine.URLRequest{Mode: "maybe"}}, invalid: true},
	} {
		err := c.req.Check(caps)
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
