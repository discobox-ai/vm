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
)

// A create asked to start returns its VM running, its guest told where its
// service listens, as Start leaves one (ADR 26-11-23-458 §4).
func TestCreateStarted(t *testing.T) {
	ctx := context.Background()
	e, _ := fakeEngine(t)
	inst, err := e.Create(ctx, engine.CreateOptions{From: "app", Name: "started", Start: &engine.StartOptions{Timeout: 30 * time.Second}})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = e.Remove(context.Background(), inst, true) })
	if state := e.State(ctx, inst); state != engine.Running {
		t.Fatalf("a VM created started: %s, want running", state)
	}
	listen, err := e.Driver.Listen(ctx, inst)
	if err != nil {
		t.Fatal(err)
	}
	if got := serviceAddress(ctx, t, e, inst); got != listen {
		t.Errorf("the service file of a VM created started: %q, want %q", got, listen)
	}
	if err := e.Stop(ctx, inst, 30*time.Second); err != nil {
		t.Fatal(err)
	}
	if state := e.State(ctx, inst); state != engine.Stopped {
		t.Errorf("a VM created started, then stopped: %s, want stopped", state)
	}
}

// A create asked to start with a window, of a driver with no display, is
// refused before the driver is asked, and makes nothing.
func TestCreateStartedRefusesAWindow(t *testing.T) {
	ctx := context.Background()
	e, _ := fakeEngine(t)
	if _, err := e.Create(ctx, engine.CreateOptions{From: "app", Name: "gui", Start: &engine.StartOptions{GUI: true}}); !errors.Is(err, machine.ErrUnsupported) {
		t.Errorf("a create started with a window: %v, want ErrUnsupported", err)
	}
	if _, err := e.Get(ctx, "gui"); !errors.Is(err, engine.ErrNotFound) {
		t.Errorf("a refused create: %v, want nothing made", err)
	}
}

// unannounced is a driver whose attached machines fail every copy into
// their guest, so a guest is never told where its service listens.
type unannounced struct {
	machine.Driver
	destroy error
}

func (d *unannounced) Attach(ctx context.Context, inst machine.Instance) (machine.Machine, error) {
	m, err := d.Driver.Attach(ctx, inst)
	if err != nil {
		return nil, err
	}
	return &uncopied{Machine: m}, nil
}

func (d *unannounced) Destroy(ctx context.Context, inst machine.Instance) error {
	if d.destroy != nil {
		return d.destroy
	}
	return d.Driver.Destroy(ctx, inst)
}

type uncopied struct{ machine.Machine }

func (*uncopied) CopyTo(context.Context, string, io.Reader) error {
	return errors.New("the guest refused the copy")
}

// A create asked to start whose guest cannot be told where its service
// listens removes the VM, and says so; where the removal fails too, it says
// the VM may still be there (ADR 26-11-23-458 §§3-4).
func TestCreateStartedThatCannotTellItsServiceIsRemoved(t *testing.T) {
	ctx := context.Background()
	e, _ := fakeEngine(t)
	base := e.Driver
	d := &unannounced{Driver: base}
	e.Driver = d
	_, err := e.Create(ctx, engine.CreateOptions{From: "app", Name: "untold", Start: &engine.StartOptions{Timeout: 30 * time.Second}})
	if err == nil || !strings.Contains(err.Error(), "so it was removed") || !strings.Contains(err.Error(), "refused the copy") {
		t.Fatalf("a create started whose service file is refused: %v, want it removed with the copy's error", err)
	}
	if _, err := base.Instance(ctx, "untold"); !errors.Is(err, machine.ErrNotFound) {
		t.Errorf("a create started whose service file is refused: %v, want nothing left", err)
	}

	d.destroy = errors.New("the provider did not answer")
	_, err = e.Create(ctx, engine.CreateOptions{From: "app", Name: "kept", Start: &engine.StartOptions{Timeout: 30 * time.Second}})
	if err == nil || !strings.Contains(err.Error(), "may still be there") || !strings.Contains(err.Error(), "did not answer") {
		t.Fatalf("a create started that could neither tell nor be removed: %v, want it to say so", err)
	}
	inst, err := base.Instance(ctx, "kept")
	if err != nil {
		t.Fatal(err)
	}
	if m, err := base.Attach(ctx, inst); err == nil {
		_ = m.Kill(ctx)
	}
	_ = base.Destroy(ctx, inst)
}
