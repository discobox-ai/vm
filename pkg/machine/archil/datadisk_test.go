package archil

import (
	"context"
	"errors"
	"runtime"
	"testing"

	"github.com/discobox-ai/vm/pkg/machine"
)

// TestAVMKeepsItsDataDiskWhateverTheSetting: a VM made with a data disk, by
// a driver configured for one, is listed with it, started with it mounted,
// upgraded with it, and destroyed with it by a driver that is not; and turning
// the setting off or on changes no VM (ADR 26-11-28-860 §2).
func TestAVMKeepsItsDataDiskWhateverTheSetting(t *testing.T) {
	if runtime.GOOS != "linux" {
		t.Skip("the fake Archil's guests run on the host, and Archil's guests are linux")
	}
	f := newFakeAPI(t, t.TempDir())
	ctx := context.Background()
	on, off := f.driver(t, "p", "debian:sid", true), f.driver(t, "p", "debian:sid", false)
	if !on.Capabilities().Attaches(machine.VolumeData) || off.Capabilities().Attaches(machine.VolumeData) {
		t.Fatalf("capabilities: on %v, off %v; want VolumeData on alone", on.Capabilities().Volumes, off.Capabilities().Volumes)
	}
	inst, err := on.Create(ctx, machine.InstanceSpec{Name: "dev", Data: &machine.DataVolume{}})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = off.Destroy(context.Background(), inst) })
	disk, err := off.dataDisk(ctx, "dev")
	if err != nil || disk == "" {
		t.Fatalf("the data disk: %q, %v", disk, err)
	}
	if got, err := off.Instance(ctx, "dev"); err != nil || got.Data == nil {
		t.Fatalf("a VM with a data disk, read by a driver configured for none: %+v, %v; want its data volume", got, err)
	}

	if _, err := off.Start(ctx, inst, machine.BootOptions{}); err != nil {
		t.Fatal(err)
	}
	if mounts := f.mountsOf(inst.ID); len(mounts) != 1 || mounts[0].DiskID != disk || mounts[0].Path != machine.VolumePath(machine.Linux, machine.VolumeData) {
		t.Fatalf("a VM with a data disk, started by a driver configured for none: mounts %+v; want %s at the data volume's path", mounts, disk)
	}
	if err := off.stopAndWait(ctx, inst.ID); err != nil {
		t.Fatal(err)
	}

	upgraded, err := off.Upgrade(ctx, inst, nil)
	if err != nil {
		t.Fatalf("an upgrade of a VM with a data disk, by a driver configured for none: %v", err)
	}
	if upgraded.ID == inst.ID || upgraded.Data == nil {
		t.Fatalf("upgraded %+v; want a new root, with its data volume", upgraded)
	}
	if again, err := off.dataDisk(ctx, "dev"); err != nil || again != disk {
		t.Fatalf("the data disk after an upgrade: %q, %v; want %s", again, err, disk)
	}
	if _, err := off.Start(ctx, upgraded, machine.BootOptions{}); err != nil {
		t.Fatal(err)
	}
	if mounts := f.mountsOf(upgraded.ID); len(mounts) != 1 || mounts[0].DiskID != disk {
		t.Fatalf("the upgraded VM started: mounts %+v; want %s", mounts, disk)
	}

	if err := off.Destroy(ctx, upgraded); err != nil {
		t.Fatal(err)
	}
	if again, err := off.dataDisk(ctx, "dev"); err != nil || again != "" {
		t.Fatalf("the data disk after a destroy: %q, %v; want none", again, err)
	}
}

// TestANewVMKeepsItsDataOnItsRoot: a driver configured for no data disk,
// the default, refuses a data volume before it makes anything, makes a new
// VM, fresh or a copy, with none, a copy asked to start booted once; and an
// upgrade of a VM with no data disk is refused, ErrUnsupported, and changes
// nothing, however the driver is configured, since a new root would delete
// its data (ADR 26-11-28-860 §§1, 3, 4).
func TestANewVMKeepsItsDataOnItsRoot(t *testing.T) {
	if runtime.GOOS != "linux" {
		t.Skip("the fake Archil's guests run on the host, and Archil's guests are linux")
	}
	f := newFakeAPI(t, t.TempDir())
	ctx := context.Background()
	on, off := f.driver(t, "p", "debian:sid", true), f.driver(t, "p", "debian:sid", false)

	if _, err := off.Create(ctx, machine.InstanceSpec{Name: "data", Data: &machine.DataVolume{}, Start: &machine.BootOptions{}}); !errors.Is(err, machine.ErrUnsupported) {
		t.Fatalf("a data volume on a driver configured for none: %v, want ErrUnsupported", err)
	}
	if live, disks := f.liveSandboxes(), f.diskNames(); len(live) != 0 || len(disks) != 0 {
		t.Fatalf("a refused data volume left sandboxes %q and disks %q", live, disks)
	}

	src, err := off.Create(ctx, machine.InstanceSpec{Name: "src"})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = off.Destroy(context.Background(), src) })
	committed, err := off.Commit(ctx, src)
	if err != nil {
		t.Fatal(err)
	}
	copied, err := off.Create(ctx, machine.InstanceSpec{Name: "copy", From: &committed, Start: &machine.BootOptions{}})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = off.Destroy(context.Background(), copied) })
	if s := f.sandboxByID(copied.ID); s.boots != 1 || s.stops != 0 || s.status != "running" {
		t.Fatalf("a copy asked to start, with no data disk: %s, %d boots, %d stops; want running, booted once", s.status, s.boots, s.stops)
	}
	if copied.Data != nil {
		t.Fatalf("a copy on a driver configured for no data disk: %+v; want no data volume", copied)
	}
	if disks := f.diskNames(); len(disks) != 0 {
		t.Fatalf("a driver configured for no data disk made disks %q", disks)
	}
	if err := off.stopAndWait(ctx, copied.ID); err != nil {
		t.Fatal(err)
	}

	plain, err := on.Create(ctx, machine.InstanceSpec{Name: "plain"})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = on.Destroy(context.Background(), plain) })
	for _, c := range []struct {
		name string
		d    *Driver
		inst machine.Instance
	}{{"off", off, copied}, {"on", on, plain}} {
		if _, err := c.d.Upgrade(ctx, c.inst, nil); !errors.Is(err, machine.ErrUnsupported) {
			t.Errorf("an upgrade of a VM with no data disk, by a driver configured %s: %v, want ErrUnsupported", c.name, err)
		}
		if got, err := c.d.Instance(ctx, c.inst.Name); err != nil || got.ID != c.inst.ID {
			t.Errorf("a refused upgrade, configured %s, changed the VM: %+v, %v; was %s", c.name, got, err, c.inst.ID)
		}
	}
}

// mountsOf is what the fake has mounted in a sandbox, by its ID.
func (f *fakeAPI) mountsOf(id string) []mount {
	return f.sandboxByID(id).mounts
}

// sandboxByID is a copy of the fake's sandbox of an ID.
func (f *fakeAPI) sandboxByID(id string) fakeSandbox {
	f.mu.Lock()
	defer f.mu.Unlock()
	for _, s := range f.sandboxes {
		if s.id == id {
			return *s
		}
	}
	return fakeSandbox{}
}

// diskNames are the fake's disks, by name.
func (f *fakeAPI) diskNames() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	var names []string
	for _, d := range f.disks {
		names = append(names, d.name)
	}
	return names
}
