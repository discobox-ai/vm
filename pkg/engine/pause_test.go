package engine_test

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/discobox-ai/vm/pkg/engine"
	"github.com/discobox-ai/vm/pkg/guest"
	"github.com/discobox-ai/vm/pkg/machine"
	"github.com/discobox-ai/vm/pkg/machine/fake"
)

// An instance its provider paused reads as paused, however often it is read,
// and Start alone resumes it: its guest and its ports are refused, naming
// start, and stay paused; remove refuses it unless forced, as a running one;
// and a stop resumes it to shut it down.
func TestPausedReadsAsPausedAndStartResumes(t *testing.T) {
	ctx := context.Background()
	e, _ := fakeEngine(t)
	inst, err := e.Create(ctx, engine.CreateOptions{From: "app", Name: "paused"})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = e.Remove(context.Background(), inst, true) })
	if err := fake.Pause(ctx, e.Driver, inst); !errors.Is(err, machine.ErrNotRunning) {
		t.Fatalf("pause a stopped instance: %v, want ErrNotRunning", err)
	}
	if err := e.Start(ctx, inst, engine.StartOptions{Timeout: 30 * time.Second}); err != nil {
		t.Fatal(err)
	}
	want := func(state engine.State, when string) {
		t.Helper()
		if got := e.State(ctx, inst); got != state {
			t.Fatalf("an instance %s is %s, want %s", when, got, state)
		}
	}
	if err := fake.Pause(ctx, e.Driver, inst); err != nil {
		t.Fatal(err)
	}
	want(engine.Paused, "paused")
	want(engine.Paused, "paused, read again")
	if err := e.Remove(ctx, inst, false); err == nil || !strings.Contains(err.Error(), "is paused") {
		t.Fatalf("remove a paused instance unforced: %v, want it refused as paused", err)
	}
	if err := e.Start(ctx, inst, engine.StartOptions{Timeout: 30 * time.Second}); err != nil {
		t.Fatalf("start a paused instance: %v", err)
	}
	want(engine.Running, "resumed by start")
	if err := e.Start(ctx, inst, engine.StartOptions{Timeout: 30 * time.Second}); err == nil || !strings.Contains(err.Error(), "already running") {
		t.Fatalf("start a running instance: %v, want it refused", err)
	}

	if err := fake.Pause(ctx, e.Driver, inst); err != nil {
		t.Fatal(err)
	}
	if _, err := e.Guest(ctx, inst); !errors.Is(err, machine.ErrPaused) || !strings.Contains(err.Error(), "start it") {
		t.Fatalf("the guest of a paused instance: %v, want ErrPaused naming start", err)
	}
	if _, err := e.Dial(ctx, inst, guest.AgentPort); !errors.Is(err, machine.ErrPaused) {
		t.Fatalf("dial a paused instance: %v, want ErrPaused", err)
	}
	if _, err := e.Driver.Boot(ctx, inst, machine.BootOptions{}); !errors.Is(err, machine.ErrPaused) {
		t.Fatalf("boot a paused instance: %v, want ErrPaused", err)
	}
	want(engine.Paused, "refused its guest, a port, and a boot")
	if err := e.Stop(ctx, inst, 30*time.Second); err != nil {
		t.Fatalf("stop a paused instance: %v", err)
	}
	want(engine.Stopped, "paused, then stopped")
	if err := e.Start(ctx, inst, engine.StartOptions{Timeout: 30 * time.Second}); err != nil {
		t.Fatalf("start an instance stopped while paused: %v", err)
	}
	want(engine.Running, "stopped while paused, then started")
}
