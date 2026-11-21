package engine_test

import (
	"context"
	"errors"
	"io"
	"strings"
	"testing"
	"time"

	"github.com/discobox-ai/vm/pkg/engine"
	"github.com/discobox-ai/vm/pkg/machine"
	"github.com/discobox-ai/vm/pkg/machine/fake"
)

// An instance its provider paused reads as paused, however often it is read;
// Start resumes it, and so does its first exec; remove refuses it unless
// forced, as a running one.
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
	g, err := e.Guest(ctx, inst)
	if err != nil {
		t.Fatalf("the guest of a paused instance: %v", err)
	}
	want(engine.Paused, "paused and attached")
	// Whether the command runs is the guest's: the exec's asking resumes it.
	if p, err := g.Exec(ctx, machine.ExecRequest{Argv: []string{"true"}}); err == nil {
		_, _ = p.Wait(io.Discard, io.Discard)
		_ = p.Close()
	}
	want(engine.Running, "resumed by an exec")
}
