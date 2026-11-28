package archil

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"testing"

	"github.com/discobox-ai/vm/pkg/machine"
)

// liveSandboxes are the fake's sandboxes not deleted, by name.
func (f *fakeAPI) liveSandboxes() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	var names []string
	for _, s := range f.sandboxes {
		if s.status != "deleted" {
			names = append(names, s.name)
		}
	}
	return names
}

// TestAFailedUpgradeLeavesTheVM: an upgrade whose new root fails, a base
// without systemd, removes the new sandbox and leaves the VM as it was, its
// sandbox and data volume (ADR 26-11-24-905 §4).
func TestAFailedUpgradeLeavesTheVM(t *testing.T) {
	if runtime.GOOS != "linux" {
		t.Skip("the fake Archil's guests run on the host, and Archil's guests are linux")
	}
	f := newFakeAPI(t, t.TempDir())
	ctx := context.Background()
	d := f.driver(t, "p", "debian:sid", true)
	inst, err := d.Create(ctx, machine.InstanceSpec{Name: "dev", Data: &machine.DataVolume{}})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = d.Destroy(context.Background(), inst) })
	disk, err := d.dataDisk(ctx, "dev")
	if err != nil || disk == "" {
		t.Fatalf("the data volume: %q, %v", disk, err)
	}
	if _, err := f.driver(t, "p", "alpine:3", true).Upgrade(ctx, inst, nil); err == nil {
		t.Fatal("an upgrade to a base without systemd: nil error")
	}
	got, err := d.Instance(ctx, "dev")
	if err != nil || got.ID != inst.ID || got.Data == nil {
		t.Fatalf("the VM after a failed upgrade: %+v, %v; want it as it was, %s with its data volume", got, err, inst.ID)
	}
	if again, err := d.dataDisk(ctx, "dev"); err != nil || again != disk {
		t.Fatalf("the data volume after a failed upgrade: %q, %v; want %s", again, err, disk)
	}
	if live := f.liveSandboxes(); len(live) != 1 {
		t.Fatalf("sandboxes after a failed upgrade: %q; want the VM's alone", live)
	}
}

// TestAnInterruptedUpgradeIsNoVM: a new sandbox an upgrade left, labeled
// as replacing the VM's, which still lives, is not the VM; the next upgrade
// removes it and finishes; and a destroy removes every sandbox of the VM
// (ADR 26-11-24-905 §§3-4).
func TestAnInterruptedUpgradeIsNoVM(t *testing.T) {
	if runtime.GOOS != "linux" {
		t.Skip("the fake Archil's guests run on the host, and Archil's guests are linux")
	}
	f := newFakeAPI(t, t.TempDir())
	ctx := context.Background()
	d := f.driver(t, "p", "debian:sid", true)
	inst, err := d.Create(ctx, machine.InstanceSpec{Name: "dev", Data: &machine.DataVolume{}})
	if err != nil {
		t.Fatal(err)
	}
	leftover := func() {
		t.Helper()
		labels := d.labels(machine.InstanceSpec{Name: "dev"})
		labels[labelReplaces] = inst.ID
		if _, err := d.api.createSandbox(ctx, createSandbox{Name: d.upgradeName("dev"), Labels: labels}); err != nil {
			t.Fatal(err)
		}
	}
	leftover()
	if got, err := d.Instance(ctx, "dev"); err != nil || got.ID != inst.ID {
		t.Fatalf("a VM with an upgrade's leftover: %+v, %v; want %s", got, err, inst.ID)
	}
	if all, err := d.Instances(ctx); err != nil || len(all) != 1 {
		t.Fatalf("instances with an upgrade's leftover: %+v, %v; want the one VM", all, err)
	}
	upgraded, err := d.Upgrade(ctx, inst, nil)
	if err != nil {
		t.Fatal(err)
	}
	if live := f.liveSandboxes(); len(live) != 1 {
		t.Fatalf("sandboxes after an upgrade: %q; want the new root alone", live)
	}
	if got, err := d.Instance(ctx, "dev"); err != nil || got.ID != upgraded.ID || got.ID == inst.ID {
		t.Fatalf("the VM after an upgrade: %+v, %v; want its new root, %s", got, err, upgraded.ID)
	}
	inst = upgraded
	leftover()
	if err := d.Destroy(ctx, inst); err != nil {
		t.Fatal(err)
	}
	if live := f.liveSandboxes(); len(live) != 0 {
		t.Fatalf("sandboxes after a destroy: %q; want none", live)
	}
}

// TestAStaleDataDiskIsMadeAgain: a data disk of a name left by a VM removed
// midway, which Archil answers a create with (200), is deleted and made
// again, so a new VM of the name sees none of the old one's data (ADR
// 26-11-24-905 §5), started or not.
func TestAStaleDataDiskIsMadeAgain(t *testing.T) {
	if runtime.GOOS != "linux" {
		t.Skip("the fake Archil's guests run on the host, and Archil's guests are linux")
	}
	f := newFakeAPI(t, t.TempDir())
	ctx := context.Background()
	d := f.driver(t, "p", "debian:sid", true)
	off := f.driver(t, "p", "debian:sid", false)
	for _, start := range []*machine.BootOptions{nil, {}} {
		// A VM with no data volume, of either setting, removes the disk
		// left of its name rather than take it as its own.
		for _, maker := range []*Driver{d, off} {
			stale, made, err := d.api.createDisk(ctx, d.dataDiskName("dev"))
			if err != nil || !made {
				t.Fatalf("a disk: %q, made %t, %v", stale, made, err)
			}
			inst, err := maker.Create(ctx, machine.InstanceSpec{Name: "dev", Start: start})
			if err != nil {
				t.Fatal(err)
			}
			if got, err := maker.Instance(ctx, "dev"); err != nil || got.Data != nil {
				t.Fatalf("a VM with no data volume, whose name had a disk left: %+v, %v; want no data volume", got, err)
			}
			if disk, err := d.dataDisk(ctx, "dev"); err != nil || disk != "" {
				t.Fatalf("a disk left of a VM's name, after a create with no data volume: %q, %v; want it removed", disk, err)
			}
			if err := maker.Destroy(ctx, inst); err != nil {
				t.Fatal(err)
			}
		}
		stale, made, err := d.api.createDisk(ctx, d.dataDiskName("dev"))
		if err != nil || !made {
			t.Fatalf("a disk: %q, made %t, %v", stale, made, err)
		}
		f.mu.Lock()
		dir := f.disk(stale).dir
		f.mu.Unlock()
		if err := os.WriteFile(filepath.Join(dir, "old"), []byte("another VM's"), 0o600); err != nil {
			t.Fatal(err)
		}
		inst, err := d.Create(ctx, machine.InstanceSpec{Name: "dev", Data: &machine.DataVolume{}, Start: start})
		if err != nil {
			t.Fatal(err)
		}
		disk, err := d.dataDisk(ctx, "dev")
		if err != nil || disk == "" || disk == stale {
			t.Fatalf("the data volume of a VM whose name had a disk left: %q, %v; want a new one, not %s", disk, err, stale)
		}
		f.mu.Lock()
		entries, _ := os.ReadDir(f.disk(disk).dir)
		f.mu.Unlock()
		if len(entries) != 0 {
			t.Fatalf("a new VM's data volume holds %d entries; want none of the old VM's", len(entries))
		}
		if err := d.Destroy(ctx, inst); err != nil {
			t.Fatal(err)
		}
	}
}

// TestACreateThatRacesGivesWay: of two creates of a name that both passed
// their checks, the one whose sandbox is younger removes it and is
// ErrExists, and leaves the other's VM (ADR 26-11-24-905 §3).
func TestACreateThatRacesGivesWay(t *testing.T) {
	if runtime.GOOS != "linux" {
		t.Skip("the fake Archil's guests run on the host, and Archil's guests are linux")
	}
	f := newFakeAPI(t, t.TempDir())
	ctx := context.Background()
	d := f.driver(t, "p", "debian:sid", false)
	// The other create's sandbox lands first, under an upgrade's name, as
	// a VM upgraded since holds it, once this create has checked.
	f.beforeCreate = func(body createSandbox) {
		if body.Name != d.sandboxName("dev") {
			return
		}
		f.beforeCreate = nil
		if _, err := d.api.createSandbox(ctx, createSandbox{Name: d.upgradeName("dev"), Labels: d.labels(machine.InstanceSpec{Name: "dev"})}); err != nil {
			t.Error(err)
		}
	}
	if _, err := d.Create(ctx, machine.InstanceSpec{Name: "dev"}); !errors.Is(err, machine.ErrExists) {
		t.Fatalf("a create that lost its race: %v, want ErrExists", err)
	}
	live := f.liveSandboxes()
	if len(live) != 1 || live[0] == d.sandboxName("dev") {
		t.Fatalf("sandboxes after a lost race: %q; want the other's alone", live)
	}
}

// TestAnUpgradeWhoseDeleteWasTakenFinishes: an upgrade whose delete of the
// old sandbox Archil took, though its answer failed, keeps the new root,
// which is the VM once the old is gone, rather than remove it and leave the
// VM no sandbox at all.
func TestAnUpgradeWhoseDeleteWasTakenFinishes(t *testing.T) {
	if runtime.GOOS != "linux" {
		t.Skip("the fake Archil's guests run on the host, and Archil's guests are linux")
	}
	f := newFakeAPI(t, t.TempDir())
	ctx := context.Background()
	d := f.driver(t, "p", "debian:sid", true)
	inst, err := d.Create(ctx, machine.InstanceSpec{Name: "dev", Data: &machine.DataVolume{}})
	if err != nil {
		t.Fatal(err)
	}
	f.mu.Lock()
	f.lostDeletes = 1
	f.mu.Unlock()
	upgraded, err := d.Upgrade(ctx, inst, nil)
	if err != nil {
		t.Fatalf("an upgrade whose delete was taken: %v", err)
	}
	got, err := d.Instance(ctx, "dev")
	if err != nil || got.ID != upgraded.ID || got.ID == inst.ID || got.Data == nil {
		t.Fatalf("the VM after an upgrade whose delete was taken: %+v, %v; want its new root, %s, with its data volume", got, err, upgraded.ID)
	}
	if err := d.Destroy(ctx, got); err != nil {
		t.Fatal(err)
	}
}

// TestARacingStartedCreateLeavesTheOthersDisk: a create asked to start with
// a data volume that passed its checks while another create of the name
// was making its VM finds that create's disk (200), so it makes its sandbox
// first, which Archil refuses: ErrExists, and the other VM's disk, mounted
// and written, is left as it is (ADR 26-11-24-905 §5).
func TestARacingStartedCreateLeavesTheOthersDisk(t *testing.T) {
	if runtime.GOOS != "linux" {
		t.Skip("the fake Archil's guests run on the host, and Archil's guests are linux")
	}
	f := newFakeAPI(t, t.TempDir())
	ctx := context.Background()
	d := f.driver(t, "p", "debian:sid", true)
	spec := machine.InstanceSpec{Name: "dev", Data: &machine.DataVolume{}, Start: &machine.BootOptions{}}
	first, err := d.Create(ctx, spec)
	if err != nil {
		t.Fatal(err)
	}
	disk, err := d.dataDisk(ctx, "dev")
	if err != nil || disk == "" {
		t.Fatalf("the first VM's data volume: %q, %v", disk, err)
	}
	f.mu.Lock()
	dir := f.disk(disk).dir
	f.mu.Unlock()
	if err := os.WriteFile(filepath.Join(dir, "mine"), []byte("the first VM's"), 0o600); err != nil {
		t.Fatal(err)
	}
	// The second create, past the check that would have refused it.
	m := &made{d: d}
	if _, err := d.create(ctx, spec, m); !errors.Is(err, machine.ErrExists) {
		t.Fatalf("a racing create: %v, want ErrExists", err)
	}
	m.undo()
	if again, err := d.dataDisk(ctx, "dev"); err != nil || again != disk {
		t.Fatalf("the first VM's data volume after a racing create: %q, %v; want %s", again, err, disk)
	}
	if _, err := os.Stat(filepath.Join(dir, "mine")); err != nil {
		t.Fatalf("the first VM's data after a racing create: %v", err)
	}
	if got, err := d.Instance(ctx, "dev"); err != nil || got.ID != first.ID {
		t.Fatalf("the VM after a racing create: %+v, %v; want %s", got, err, first.ID)
	}
}
