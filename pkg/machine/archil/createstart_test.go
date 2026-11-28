package archil

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/discobox-ai/vm/pkg/guest"
	"github.com/discobox-ai/vm/pkg/machine"
)

// TestCreateStartedStartsOnce: a create asked to start leaves its sandbox
// running, systemd launched: a fresh one, its data disk made first and
// mounted by its create, is started once and never stopped; a fork with no
// data volume is started once too; a fork with one is stopped and started
// once with it mounted, since a fork takes no mounts; and a create not
// asked to start is stopped as before (ADR 26-11-23-458 §2).
func TestCreateStartedStartsOnce(t *testing.T) {
	if runtime.GOOS != "linux" {
		t.Skip("the fake Archil's guests run on the host, and Archil's guests are linux")
	}
	f := newFakeAPI(t, t.TempDir())
	ctx := context.Background()
	d := f.driver(t, "p", "debian:sid", true)
	sandboxOf := func(name string) fakeSandbox {
		t.Helper()
		f.mu.Lock()
		defer f.mu.Unlock()
		for _, s := range f.sandboxes {
			if s.name == d.sandboxName(name) {
				return *s
			}
		}
		t.Fatalf("no sandbox of %s", name)
		return fakeSandbox{}
	}
	start := &machine.BootOptions{}
	data := &machine.DataVolume{}

	src, err := d.Create(ctx, machine.InstanceSpec{Name: "src"})
	if err != nil {
		t.Fatal(err)
	}
	if s := sandboxOf("src"); s.status != "stopped" || s.boots != 1 || s.stops != 1 {
		t.Fatalf("a create not asked to start: %s, %d boots, %d stops; want stopped, one of each", s.status, s.boots, s.stops)
	}
	committed, err := d.Commit(ctx, src)
	if err != nil {
		t.Fatal(err)
	}

	for _, c := range []struct {
		name         string
		spec         machine.InstanceSpec
		boots, stops int
	}{
		{"fresh", machine.InstanceSpec{Start: start}, 1, 0},
		{"fresh-data", machine.InstanceSpec{Start: start, Data: data}, 1, 0},
		{"fork", machine.InstanceSpec{Start: start, From: &committed}, 1, 0},
		{"fork-data", machine.InstanceSpec{Start: start, From: &committed, Data: data}, 2, 1},
	} {
		c.spec.Name = c.name
		inst, err := d.Create(ctx, c.spec)
		if err != nil {
			t.Fatalf("create %s started: %v", c.name, err)
		}
		t.Cleanup(func() { _ = d.Destroy(context.Background(), inst) })
		s := sandboxOf(c.name)
		if s.status != "running" || s.boots != c.boots || s.stops != c.stops {
			t.Errorf("create %s started: %s, %d boots, %d stops; want running, %d and %d", c.name, s.status, s.boots, s.stops, c.boots, c.stops)
		}
		if state, err := d.State(ctx, inst); err != nil || state != machine.StateRunning {
			t.Errorf("create %s started: state %s, %v", c.name, state, err)
		}
		if (inst.Data != nil) != (c.spec.Data != nil) {
			t.Errorf("create %s started: data volume %+v, asked %+v", c.name, inst.Data, c.spec.Data)
		}
		if c.spec.Data != nil {
			mounted := guest.Resolve(s.root, machine.VolumePath(machine.Linux, machine.VolumeData))
			if _, err := os.Stat(filepath.Join(mounted, ".")); err != nil {
				t.Errorf("create %s started: its data volume is not mounted: %v", c.name, err)
			}
		}
	}
}

// TestCreateStartedMakesNothingWhenRefused: a fresh create asked to start
// with a data volume, of a name a VM has, is refused before its disk is
// made; one whose systemd never comes up, and one Archil refuses, leave
// neither a sandbox nor a disk; and a window is refused before anything is
// made (ADR 26-11-23-458 §§3, 7).
func TestCreateStartedMakesNothingWhenRefused(t *testing.T) {
	if runtime.GOOS != "linux" {
		t.Skip("the fake Archil's guests run on the host, and Archil's guests are linux")
	}
	f := newFakeAPI(t, t.TempDir())
	ctx := context.Background()
	d := f.driver(t, "p", "debian:sid", true)
	counts := func() (int, int) {
		f.mu.Lock()
		defer f.mu.Unlock()
		live := 0
		for _, s := range f.sandboxes {
			if s.status != "deleted" {
				live++
			}
		}
		return live, len(f.disks)
	}
	start := &machine.BootOptions{}

	if _, err := d.Create(ctx, machine.InstanceSpec{Name: "window", Start: &machine.BootOptions{GUI: true}}); !errors.Is(err, machine.ErrUnsupported) {
		t.Fatalf("a create started with a window: %v, want ErrUnsupported", err)
	}

	taken, err := d.Create(ctx, machine.InstanceSpec{Name: "taken"})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = d.Destroy(context.Background(), taken) })
	if _, err := d.Create(ctx, machine.InstanceSpec{Name: "taken", Start: start, Data: &machine.DataVolume{}}); !errors.Is(err, machine.ErrExists) {
		t.Fatalf("a create started of a name taken: %v, want ErrExists", err)
	}
	if sandboxes, disks := counts(); sandboxes != 1 || disks != 0 {
		t.Fatalf("a refused create left %d sandboxes and %d disks; want the VM of the name's alone and no disk", sandboxes, disks)
	}
	if inst, err := d.Instance(ctx, "taken"); err != nil || inst.Data != nil {
		t.Fatalf("the VM of the name: %+v, %v; want it with no data volume", inst, err)
	}

	nosystemd := f.driver(t, "p", "alpine:3", true)
	if _, err := nosystemd.Create(ctx, machine.InstanceSpec{Name: "nosystemd", Start: start, Data: &machine.DataVolume{}}); err == nil || !strings.Contains(err.Error(), "/lib/systemd/systemd") {
		t.Fatalf("a create started of a base without systemd: %v, want it refused", err)
	}
	if sandboxes, disks := counts(); sandboxes != 1 || disks != 0 {
		t.Errorf("failed creates left %d sandboxes and %d disks; want the VM of the name's alone and no disk", sandboxes, disks)
	}
}
