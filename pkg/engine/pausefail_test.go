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

// breaking is a driver whose Start fails as told: it does not resume
// (resume), or the machine it returns fails a copy into its guest (write),
// an orderly shutdown (shutdown), or a kill (kill), as a provider that
// stopped answering would. Everything else is its Driver's.
type breaking struct {
	machine.Driver
	resume, write, shutdown, kill error
}

func (d *breaking) Start(ctx context.Context, inst machine.Instance, opts machine.BootOptions) (machine.Machine, error) {
	if d.resume != nil {
		return nil, d.resume
	}
	m, err := d.Driver.Start(ctx, inst, opts)
	if err != nil {
		return nil, err
	}
	return &broken{Machine: m, d: d}, nil
}

// broken is a machine of breaking's.
type broken struct {
	machine.Machine
	d *breaking
}

func (m *broken) CopyTo(ctx context.Context, dir string, tar io.Reader) error {
	if m.d.write != nil {
		return m.d.write
	}
	return m.Machine.CopyTo(ctx, dir, tar)
}

func (m *broken) Shutdown(ctx context.Context, reboot bool) error {
	if m.d.shutdown != nil {
		return m.d.shutdown
	}
	return m.Machine.Shutdown(ctx, reboot)
}

func (m *broken) Kill(ctx context.Context) error {
	if m.d.kill != nil {
		return m.d.kill
	}
	return m.Machine.Kill(ctx)
}

// pausedBehind starts a fake instance and pauses it, and returns an engine
// on a breaking driver over the same fake, which the test sets to fail.
func pausedBehind(t *testing.T, name string) (*engine.Engine, *breaking, engine.Instance) {
	t.Helper()
	ctx := context.Background()
	e, _ := fakeEngine(t)
	inst, err := e.Create(ctx, engine.CreateOptions{From: "app", Name: name})
	if err != nil {
		t.Fatal(err)
	}
	base := e.Driver
	t.Cleanup(func() {
		if m, err := base.Attach(context.Background(), inst); err == nil {
			_ = m.Kill(context.Background())
		}
		_ = base.Destroy(context.Background(), inst)
	})
	if err := e.Start(ctx, inst, engine.StartOptions{Timeout: 30 * time.Second}); err != nil {
		t.Fatal(err)
	}
	if err := fake.Pause(ctx, base, inst); err != nil {
		t.Fatal(err)
	}
	d := &breaking{Driver: base}
	e.Driver = d
	return e, d, inst
}

var (
	errWrite = errors.New("the copy into the guest failed")
	errKill  = errors.New("the provider refused the kill")
	errDown  = errors.New("the provider refused the shutdown")
	errStuck = errors.New("the provider would not resume it")
)

// A resume whose service file cannot be written, and whose kill fails too,
// says plainly that the instance may still be running, with both errors.
func TestResumeThatCannotTellItsServiceNorStopSaysSo(t *testing.T) {
	e, d, inst := pausedBehind(t, "unkillable")
	d.write, d.kill = errWrite, errKill
	err := e.Start(context.Background(), inst, engine.StartOptions{Timeout: 30 * time.Second})
	want := "instance unkillable resumed, but its service file could not be written, and stopping it failed, so it may still be running; stop it: "
	if err == nil || !strings.HasPrefix(err.Error(), want) || !errors.Is(err, errWrite) || !errors.Is(err, errKill) {
		t.Fatalf("a resume that can be neither told nor stopped: %v, want %q wrapping both errors", err, want)
	}
	if got := e.State(context.Background(), inst); got != engine.Running {
		t.Fatalf("an instance whose kill failed is %s; this test expects it left running", got)
	}
}

// Stop of a paused instance resumes it and fails closed: one that cannot be
// shut down in order is forced off and ends stopped; one that can be
// neither says plainly it was resumed and may still be running.
func TestStopOfAPausedInstanceFailsClosed(t *testing.T) {
	ctx := context.Background()
	t.Run("forced off", func(t *testing.T) {
		e, d, inst := pausedBehind(t, "forced")
		d.shutdown = errDown
		if err := e.Stop(ctx, inst, time.Second); err == nil || !strings.Contains(err.Error(), "forced off") {
			t.Fatalf("stop of a paused instance that would not shut down: %v, want it forced off", err)
		}
		if got := e.State(ctx, inst); got != engine.Stopped {
			t.Fatalf("a paused instance forced off is %s, want stopped", got)
		}
	})
	t.Run("neither", func(t *testing.T) {
		e, d, inst := pausedBehind(t, "stuck")
		d.shutdown, d.kill = errDown, errKill
		err := e.Stop(ctx, inst, time.Second)
		want := "instance stuck was resumed to stop it, but it could not be stopped, so it may still be running; stop it again: "
		if err == nil || !strings.HasPrefix(err.Error(), want) || !errors.Is(err, errKill) {
			t.Fatalf("stop of a paused instance that can be neither shut down nor killed: %v, want %q", err, want)
		}
		if got := e.State(ctx, inst); got != engine.Running {
			t.Fatalf("an instance whose kill failed is %s; this test expects it left running", got)
		}
	})
}

// Remove with force of a paused instance that does not resume destroys it as
// it is; one resumed that could not be stopped is refused, and kept.
func TestRemoveForceOfAPausedInstance(t *testing.T) {
	ctx := context.Background()
	t.Run("does not resume", func(t *testing.T) {
		e, d, inst := pausedBehind(t, "frozen")
		d.resume = errStuck
		if err := e.Remove(ctx, inst, true); err != nil {
			t.Fatalf("remove --force of a paused instance that does not resume: %v", err)
		}
		if _, err := e.Get(ctx, inst.Name); !errors.Is(err, engine.ErrNotFound) {
			t.Fatalf("a paused instance removed with force: %v, want ErrNotFound", err)
		}
	})
	t.Run("resumed and not stopped", func(t *testing.T) {
		e, d, inst := pausedBehind(t, "stubborn")
		d.shutdown, d.kill = errDown, errKill
		if err := e.Remove(ctx, inst, true); err == nil || !strings.Contains(err.Error(), "may still be running") {
			t.Fatalf("remove --force of an instance resumed and not stopped: %v, want it refused", err)
		}
		if _, err := e.Get(ctx, inst.Name); err != nil {
			t.Fatalf("an instance remove refused is gone: %v", err)
		}
	})
}
