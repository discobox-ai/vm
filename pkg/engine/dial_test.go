package engine_test

import (
	"context"
	"errors"
	"io"
	"strings"
	"testing"
	"time"

	"github.com/discobox-ai/vm/pkg/engine"
	"github.com/discobox-ai/vm/pkg/guest"
	"github.com/discobox-ai/vm/pkg/machine"
)

// A guest port of a running instance is dialed through its shim: the agent's
// answers an HTTP request, a stopped instance is refused, and so is any port
// but the agent's and the service's (ADR 26-11-03-247).
func TestDial(t *testing.T) {
	ctx := context.Background()
	e, _ := fakeEngine(t)
	inst, err := e.Create(ctx, engine.CreateOptions{From: "app", Name: "dialed"})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = e.Remove(context.Background(), inst, true) })
	if _, err := e.Dial(ctx, inst, guest.AgentPort); err == nil || !strings.Contains(err.Error(), "not running") {
		t.Fatalf("dial a stopped instance: %v, want it refused as not running", err)
	}
	if err := e.Start(ctx, inst, engine.StartOptions{Timeout: 30 * time.Second}); err != nil {
		t.Fatal(err)
	}
	conn, err := e.Dial(ctx, inst, guest.AgentPort)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	if _, err := io.WriteString(conn, "GET /v1/health HTTP/1.0\r\nHost: guest\r\n\r\n"); err != nil {
		t.Fatal(err)
	}
	answer, err := io.ReadAll(conn)
	if err != nil || !strings.Contains(string(answer), " 200 ") {
		t.Fatalf("the agent's port answered %q: %v", answer, err)
	}
	for _, port := range []uint32{0, guest.DisplayPort, 22} {
		if _, err := e.Dial(ctx, inst, port); !errors.Is(err, machine.ErrUnsupported) {
			t.Fatalf("dial port %d: %v, want ErrUnsupported", port, err)
		}
	}
}

// A forward the embedder sets goes to exactly one of a host socket and a
// peer's agent or service port, of a valid pool, never to the instance
// itself; a fence the driver does not hold is refused.
func TestCreateRefuses(t *testing.T) {
	ctx := context.Background()
	e, _ := fakeEngine(t)
	for _, c := range []struct {
		name     string
		vm       string
		forwards []machine.Forward
		want     string
	}{
		{"both", "", []machine.Forward{{Port: 1, Socket: "/s", Instance: "peer", InstancePort: 2}}, "both a socket and instance"},
		{"no port", "", []machine.Forward{{Port: 1, Instance: "peer"}}, "with no port"},
		{"bad name", "", []machine.Forward{{Port: 1, Instance: "Peer!", InstancePort: 2}}, "not an instance name"},
		{"stray port", "", []machine.Forward{{Port: 1, Socket: "/s", InstancePort: 2}}, "port of no instance"},
		{"stray pool", "", []machine.Forward{{Port: 1, Socket: "/s", Pool: "other"}}, "pool of no instance"},
		{"bad pool", "", []machine.Forward{{Port: 1, Instance: "peer", InstancePort: guest.AgentPort, Pool: "Other!"}}, "pool"},
		{"itself", "self", []machine.Forward{{Port: 1, Instance: "self", InstancePort: guest.AgentPort}}, "itself"},
		{"another port", "", []machine.Forward{{Port: 1, Instance: "peer", InstancePort: 22}}, "agent's port (7300) or its service's (7302) alone"},
	} {
		e.Forwards = c.forwards
		if _, err := e.Create(ctx, engine.CreateOptions{From: "app", Name: c.vm}); err == nil || !strings.Contains(err.Error(), c.want) {
			t.Errorf("%s: %v, want an error saying %q", c.name, err, c.want)
		}
	}
	e.Forwards = nil
	if _, err := e.Create(ctx, engine.CreateOptions{From: "app", Egress: &machine.Egress{}}); !errors.Is(err, machine.ErrUnsupported) {
		t.Errorf("a fence fake does not hold: %v, want ErrUnsupported", err)
	}
}
