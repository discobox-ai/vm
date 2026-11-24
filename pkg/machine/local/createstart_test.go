package local_test

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"testing"

	"github.com/discobox-ai/vm/pkg/machine"
	"github.com/discobox-ai/vm/pkg/machine/fake"
	"github.com/discobox-ai/vm/pkg/machine/local"
)

// A create asked to start returns its VM running under a shim, as a create
// then a start would (ADR 26-11-23-458 §2).
func TestCreateAskedToStartRuns(t *testing.T) {
	ctx := context.Background()
	c := capped{t: t, root: t.TempDir(), agent: buildAgent(t), limit: 1}
	d := c.driver("a")
	inst, err := d.Create(ctx, machine.InstanceSpec{Name: "one", Start: &machine.BootOptions{}})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = d.Destroy(context.Background(), inst) })
	if state, err := d.State(ctx, inst); err != nil || state != machine.StateRunning {
		t.Fatalf("a VM created started: %s, %v, want running", state, err)
	}
	m, err := d.Attach(ctx, inst)
	if err != nil {
		t.Fatal(err)
	}
	kill(t, m)
}

// A create asked to start that its start refuses, or whose start fails once
// its disk is made, makes nothing: its name is free, and its directory gone
// (ADR 26-11-23-458 §3).
func TestCreateAskedToStartThatFailsMakesNothing(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("the failing shim is a shell script")
	}
	ctx := context.Background()
	c := capped{t: t, root: t.TempDir(), agent: buildAgent(t), limit: 1}
	d := c.driver("a")

	// A window from a driver with no display is refused before anything is
	// made.
	if _, err := d.Create(ctx, machine.InstanceSpec{Name: "gui", Start: &machine.BootOptions{GUI: true}}); !errors.Is(err, machine.ErrUnsupported) {
		t.Errorf("a create asked to start with a window: %v, want ErrUnsupported", err)
	}

	// A start past the cap is ErrCapacity, as Start's is.
	running := c.create(d, "running")
	m, err := d.Start(ctx, running, machine.BootOptions{})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = m.Kill(context.Background()) })
	if _, err := d.Create(ctx, machine.InstanceSpec{Name: "full", Start: &machine.BootOptions{}}); !errors.Is(err, machine.ErrCapacity) {
		t.Errorf("a create asked to start past the cap: %v, want ErrCapacity", err)
	}

	// A shim that exits before it boots fails the start once the disk is
	// made.
	script := filepath.Join(t.TempDir(), "failing-shim")
	if err := os.WriteFile(script, []byte("#!/bin/sh\necho no boot >&2\nexit 1\n"), 0o755); err != nil { //nolint:gosec // G306: the script is run
		t.Fatal(err)
	}
	failing, err := local.New(machine.Config{Dir: c.root, Pool: "b", ShimCommand: []string{script}}, &fake.Hypervisor{Agent: c.agent, BaseName: "stock"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := failing.Create(ctx, machine.InstanceSpec{Name: "broken", Start: &machine.BootOptions{}}); err == nil {
		t.Error("a create whose shim fails: nil error, want the start's")
	}

	for _, name := range []string{"gui", "full"} {
		if _, err := d.Instance(ctx, name); !errors.Is(err, machine.ErrNotFound) {
			t.Errorf("instance %s after its create failed: %v, want ErrNotFound", name, err)
		}
	}
	if _, err := failing.Instance(ctx, "broken"); !errors.Is(err, machine.ErrNotFound) {
		t.Errorf("instance broken after its start failed: %v, want ErrNotFound", err)
	}
	entries, err := os.ReadDir(filepath.Join(c.root, "instances"))
	if err != nil {
		t.Fatal(err)
	}
	var dirs []string
	for _, e := range entries {
		if e.Name() != ".names" {
			dirs = append(dirs, e.Name())
		}
	}
	if len(dirs) != 1 || dirs[0] != running.ID {
		t.Errorf("instance directories after the failed creates: %q, want only running's, %s", dirs, running.ID)
	}
	// Each name is free again.
	for _, name := range []string{"gui", "full"} {
		c.create(d, name)
	}
	c.create(failing, "broken")
}
