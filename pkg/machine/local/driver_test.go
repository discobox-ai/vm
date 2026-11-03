package local_test

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"testing"
	"time"

	"github.com/discobox-ai/vm/pkg/machine"
	"github.com/discobox-ai/vm/pkg/machine/fake"
	"github.com/discobox-ai/vm/pkg/machine/local"
)

// chained is the fake hypervisor as one whose disks chain, as hcs's do,
// recording the chain each Prepare was given.
type chained struct {
	*fake.Hypervisor
	prepared [][]string
}

func (*chained) Chains() bool { return true }

func (c *chained) Prepare(ctx context.Context, inst local.InstanceSpec) error {
	var ids []string
	for _, l := range inst.Chain {
		ids = append(ids, l.ID)
	}
	c.prepared = append(c.prepared, ids)
	return c.Hypervisor.Prepare(ctx, inst)
}

// A VM created from a committed VM created from an install, on a hypervisor
// that chains, is prepared over the whole chain, and each VM in it is
// refused removal and boot while a VM's disk differences over its own; a VM
// whose chain was lost behind the driver's back does not boot, and is still
// removed. On a hypervisor that does not chain, a VM created from another
// outlives it.
func TestChainsKeepADiskAnotherNeeds(t *testing.T) {
	ctx := context.Background()
	guestOS := machine.OS(runtime.GOOS)
	root := t.TempDir()
	hv := &chained{Hypervisor: &fake.Hypervisor{}}
	d, err := local.New(machine.Config{Dir: root}, hv)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := d.Create(ctx, machine.InstanceSpec{Name: "fresh"}); !errors.Is(err, machine.ErrUnsupported) {
		t.Fatalf("a fresh VM with no base: %v, want ErrUnsupported", err)
	}
	copyOf := func(from machine.Instance, name string) machine.Instance {
		t.Helper()
		inst, err := d.Create(ctx, machine.InstanceSpec{Name: name, From: &from})
		if err != nil {
			t.Fatal(err)
		}
		if inst.From != from.Name || inst.GuestOS != from.GuestOS {
			t.Fatalf("created %+v from %s", inst, from.Name)
		}
		return inst
	}
	commit := func(inst machine.Instance) machine.Instance {
		t.Helper()
		committed, err := d.Commit(ctx, inst)
		if err != nil {
			t.Fatal(err)
		}
		return committed
	}
	base, err := d.Install(ctx, machine.InstallSpec{GuestOS: guestOS, Media: "latest"}, "base")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := d.Install(ctx, machine.InstallSpec{GuestOS: guestOS, Media: "latest"}, "base"); !errors.Is(err, machine.ErrExists) {
		t.Fatalf("a second install named base: %v, want ErrExists", err)
	}
	first := commit(copyOf(base, "one"))
	second := commit(copyOf(first, "two"))
	third := copyOf(second, "three")
	if got := hv.prepared[len(hv.prepared)-1]; len(got) != 3 || got[0] != base.ID || got[1] != first.ID || got[2] != second.ID {
		t.Fatalf("prepared over %v, want the whole chain, base first", got)
	}

	for _, inst := range []machine.Instance{base, first, second} {
		if err := d.Destroy(ctx, inst); !errors.Is(err, machine.ErrInUse) {
			t.Fatalf("remove %s, which another VM's disk needs: %v, want ErrInUse", inst.Name, err)
		}
		if _, err := d.Boot(ctx, inst, machine.BootOptions{}); !errors.Is(err, machine.ErrInUse) {
			t.Fatalf("boot %s, which another VM's disk needs: %v, want ErrInUse", inst.Name, err)
		}
	}
	// Another project shares the directory and sees none of it.
	other, err := local.New(machine.Config{Dir: root, Project: "other"}, hv)
	if err != nil {
		t.Fatal(err)
	}
	if list, err := other.Instances(ctx); err != nil || len(list) != 0 {
		t.Fatalf("another project's VMs = %+v, %v", list, err)
	}

	// A VM of the chain removed behind the driver's back leaves the VMs
	// over it unbootable, and still removed.
	if err := os.RemoveAll(filepath.Join(root, "instances", first.ID)); err != nil {
		t.Fatal(err)
	}
	if _, err := d.Boot(ctx, third, machine.BootOptions{}); !errors.Is(err, machine.ErrNotFound) {
		t.Fatalf("boot a VM whose chain is gone: %v, want ErrNotFound", err)
	}
	// A VM being created from base, its disk made and its record not yet
	// written, holds base too.
	making := filepath.Join(root, "instances", "making")
	if err := os.MkdirAll(making, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(making, "from"), []byte(base.ID), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := d.Destroy(ctx, base); !errors.Is(err, machine.ErrInUse) {
		t.Fatalf("remove %s while a VM is being created from it: %v, want ErrInUse", base.Name, err)
	}
	// One a crashed Create left, past the stale-claim window, holds
	// nothing; the directory stays for the owner to remove.
	old := time.Now().Add(-time.Hour)
	if err := os.Chtimes(filepath.Join(making, "from"), old, old); err != nil {
		t.Fatal(err)
	}
	if _, err := d.Boot(ctx, base, machine.BootOptions{}); errors.Is(err, machine.ErrInUse) {
		t.Fatalf("boot %s with only a crashed create's leftover naming it: %v", base.Name, err)
	}
	if err := os.RemoveAll(making); err != nil {
		t.Fatal(err)
	}
	for _, inst := range []machine.Instance{third, second, base} {
		if err := d.Destroy(ctx, inst); err != nil {
			t.Fatalf("remove %s: %v", inst.Name, err)
		}
	}
	if list, err := d.Instances(ctx); err != nil || len(list) != 0 {
		t.Fatalf("VMs left = %+v, %v", list, err)
	}

	// The fake hypervisor's own copies are trees of their own.
	plain, err := local.New(machine.Config{Dir: t.TempDir()}, &fake.Hypervisor{})
	if err != nil {
		t.Fatal(err)
	}
	b, err := plain.Install(ctx, machine.InstallSpec{GuestOS: guestOS, Media: "latest"}, "b")
	if err != nil {
		t.Fatal(err)
	}
	inst, err := plain.Create(ctx, machine.InstanceSpec{Name: "four", From: &b})
	if err != nil {
		t.Fatal(err)
	}
	four, err := plain.Commit(ctx, inst)
	if err != nil {
		t.Fatal(err)
	}
	if err := plain.Destroy(ctx, b); err != nil {
		t.Fatalf("remove a VM another was created from, on a hypervisor that does not chain: %v", err)
	}
	if _, err := plain.Create(ctx, machine.InstanceSpec{Name: "five", From: &four}); err != nil {
		t.Fatalf("create from a committed VM whose own start is gone: %v", err)
	}
}

// An instance recorded before ADR 26-11-03-606, of an image or a base, is
// not read: not listed, not found, and so never created from or rewritten.
func TestRecordsFromBeforeAreNotRead(t *testing.T) {
	ctx := context.Background()
	root := t.TempDir()
	d, err := local.New(machine.Config{Dir: root}, &fake.Hypervisor{BaseName: "b"})
	if err != nil {
		t.Fatal(err)
	}
	guestOS := string(machine.OS(runtime.GOOS))
	for id, record := range map[string]string{
		"0000000000a1": `{"id":"0000000000a1","name":"of-image","image":"app","imageID":"aaaa","guestOS":"` + guestOS + `","project":"default"}`,
		"0000000000b2": `{"id":"0000000000b2","name":"of-base","image":"stock","base":{"name":"stock"},"guestOS":"` + guestOS + `","project":"default"}`,
		"0000000000c3": `{"id":"0000000000c3","name":"of-layer","layer":"aaaa","guestOS":"` + guestOS + `","project":"default"}`,
	} {
		dir := filepath.Join(root, "instances", id)
		if err := os.MkdirAll(dir, 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(dir, "instance.json"), []byte(record), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	if list, err := d.Instances(ctx); err != nil || len(list) != 0 {
		t.Fatalf("instances = %+v, %v; want none from before", list, err)
	}
	fresh, err := d.Create(ctx, machine.InstanceSpec{Name: "fresh"})
	if err != nil {
		t.Fatal(err)
	}
	if list, err := d.Instances(ctx); err != nil || len(list) != 1 || list[0].ID != fresh.ID {
		t.Fatalf("instances = %+v, %v; want the new VM alone", list, err)
	}
}
