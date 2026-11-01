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
// answers an HTTP request, and a stopped instance is refused.
func TestDial(t *testing.T) {
	ctx := context.Background()
	e, _ := fakeEngine(t)
	inst, err := e.Create(ctx, "app", engine.CreateOptions{Name: "dialed"})
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
	if _, err := e.Dial(ctx, inst, 0); err == nil {
		t.Fatal("dial port 0 was not refused")
	}
}

// A forward goes to exactly one of a host socket and a peer's port, never to
// the instance itself; a fence the driver does not hold is refused.
func TestCreateRefuses(t *testing.T) {
	ctx := context.Background()
	e, _ := fakeEngine(t)
	for _, c := range []struct {
		name string
		opts engine.CreateOptions
		want string
	}{
		{"both", engine.CreateOptions{Forwards: []machine.Forward{{Port: 1, Socket: "/s", Instance: "peer", InstancePort: 2}}}, "both a socket and instance"},
		{"no port", engine.CreateOptions{Forwards: []machine.Forward{{Port: 1, Instance: "peer"}}}, "with no port"},
		{"bad name", engine.CreateOptions{Forwards: []machine.Forward{{Port: 1, Instance: "Peer!", InstancePort: 2}}}, "not an instance name"},
		{"stray port", engine.CreateOptions{Forwards: []machine.Forward{{Port: 1, Socket: "/s", InstancePort: 2}}}, "port of no instance"},
		{"itself", engine.CreateOptions{Name: "self", Forwards: []machine.Forward{{Port: 1, Instance: "self", InstancePort: 2}}}, "itself"},
	} {
		if _, err := e.Create(ctx, "app", c.opts); err == nil || !strings.Contains(err.Error(), c.want) {
			t.Errorf("%s: %v, want an error saying %q", c.name, err, c.want)
		}
	}
	if _, err := e.Create(ctx, "app", engine.CreateOptions{Egress: &machine.Egress{}}); !errors.Is(err, machine.ErrUnsupported) {
		t.Errorf("a fence fake does not hold: %v, want ErrUnsupported", err)
	}
}
