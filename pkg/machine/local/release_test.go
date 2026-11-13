package local_test

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"runtime"
	"testing"

	"github.com/discobox-ai/vm/pkg/machine"
	"github.com/discobox-ai/vm/pkg/machine/fake"
	"github.com/discobox-ai/vm/pkg/machine/local"
)

// keeps is the fake hypervisor as one that keeps a VM's release with its
// files, as vz keeps it in a bundle's vz.json.
type keeps struct {
	*fake.Hypervisor
	release machine.Release
}

func (k keeps) Release(local.Layer) *machine.Release { return &k.release }

// A record written before Install returned the release, which has none, is
// given the one the hypervisor keeps with the VM: an install made before the
// change, and a VM created from it, have their release with no reinstall.
func TestReleaseFilledFromTheHypervisor(t *testing.T) {
	ctx := context.Background()
	root := t.TempDir()
	plain, err := local.New(machine.Config{Dir: root}, &fake.Hypervisor{})
	if err != nil {
		t.Fatal(err)
	}
	base, err := plain.Install(ctx, machine.InstallSpec{GuestOS: machine.OS(runtime.GOOS), Media: "latest"}, "base")
	if err != nil {
		t.Fatal(err)
	}
	// As a record from before: no release in it.
	record := filepath.Join(root, "instances", base.ID, "instance.json")
	data, err := os.ReadFile(record)
	if err != nil {
		t.Fatal(err)
	}
	var fields map[string]any
	if err := json.Unmarshal(data, &fields); err != nil {
		t.Fatal(err)
	}
	if _, ok := fields["release"]; !ok {
		t.Fatalf("the install's record holds no release: %s", data)
	}
	delete(fields, "release")
	if data, err = json.Marshal(fields); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(record, data, 0o644); err != nil {
		t.Fatal(err)
	}
	if got, err := plain.Instance(ctx, "base"); err != nil || got.Release != nil {
		t.Fatalf("an old record on a hypervisor that keeps none: %+v, %v", got.Release, err)
	}

	kept := machine.Release{Version: "27.0.0", Build: "26A428"}
	d, err := local.New(machine.Config{Dir: root}, keeps{&fake.Hypervisor{}, kept})
	if err != nil {
		t.Fatal(err)
	}
	got, err := d.Instance(ctx, "base")
	if err != nil || got.Release == nil || *got.Release != kept {
		t.Fatalf("an old record: release %v, %v; want the hypervisor's %v", got.Release, err, kept)
	}
	one, err := d.Create(ctx, machine.InstanceSpec{Name: "one", From: &got})
	if err != nil || one.Release == nil || *one.Release != kept {
		t.Fatalf("a VM of the old install: release %v, %v", one.Release, err)
	}
	list, err := d.Instances(ctx)
	if err != nil || len(list) != 2 || list[0].Release == nil || list[1].Release == nil {
		t.Fatalf("listed: %+v, %v", list, err)
	}
}
