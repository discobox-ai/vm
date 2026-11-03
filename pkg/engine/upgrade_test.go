package engine_test

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/discobox-ai/vm/pkg/engine"
	"github.com/discobox-ai/vm/pkg/machine"
)

// otherOS is a driver that reports the VM name as a guest of another OS
// than it is.
type otherOS struct {
	machine.Driver
	name string
}

func (o otherOS) Instance(ctx context.Context, name string) (machine.Instance, error) {
	inst, err := o.Driver.Instance(ctx, name)
	if name == o.name {
		inst.GuestOS = "plan9"
	}
	return inst, err
}

// An upgrade needs a driver that attaches a data volume, a VM of the
// project, and a committed VM other than itself, of its guest OS; one from a committed VM
// keeps the VM's name and settings and records where its root is from, and
// a fresh one needs a base. (internal/e2e refuses a running one.)
func TestUpgrade(t *testing.T) {
	ctx := context.Background()
	driver := fakeDriver(t, t.TempDir(), "")
	e, err := engine.Open(driver, "")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := e.Install(ctx, "base", engine.InstallOptions{}); err != nil {
		t.Fatal(err)
	}
	dev, err := e.Create(ctx, engine.CreateOptions{From: "base", Name: "dev", CPUs: 3, Data: &machine.DataVolume{}})
	if err != nil {
		t.Fatal(err)
	}
	bare, err := engine.Open(noVolumes{driver}, "")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := bare.Upgrade(ctx, "dev", engine.UpgradeOptions{From: "base"}); !errors.Is(err, machine.ErrUnsupported) {
		t.Fatalf("an upgrade on a driver with no data volume: %v, want ErrUnsupported", err)
	}
	if _, err := e.Upgrade(ctx, "nothing", engine.UpgradeOptions{From: "base"}); !errors.Is(err, engine.ErrNotFound) {
		t.Fatalf("an upgrade of no VM: %v, want ErrNotFound", err)
	}
	if _, err := e.Upgrade(ctx, "dev", engine.UpgradeOptions{From: "nothing"}); !errors.Is(err, machine.ErrNotFound) {
		t.Fatalf("an upgrade from no VM: %v, want ErrNotFound", err)
	}
	if _, err := e.Upgrade(ctx, "dev", engine.UpgradeOptions{From: "dev"}); err == nil {
		t.Fatal("upgraded a VM from itself")
	}
	other, err := engine.Open(otherOS{driver, "base"}, "")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := other.Upgrade(ctx, "dev", engine.UpgradeOptions{From: "base"}); err == nil || !strings.Contains(err.Error(), "guest") {
		t.Fatalf("an upgrade from a committed VM of another guest OS: %v", err)
	}
	if _, err := e.Upgrade(ctx, "dev", engine.UpgradeOptions{}); !errors.Is(err, machine.ErrUnsupported) {
		t.Fatalf("a fresh root with no base: %v, want ErrUnsupported", err)
	}
	two, err := e.Create(ctx, engine.CreateOptions{From: "base", Name: "two"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := e.Commit(ctx, two); err != nil {
		t.Fatal(err)
	}
	upgraded, err := e.Upgrade(ctx, "dev", engine.UpgradeOptions{From: "two"})
	if err != nil {
		t.Fatal(err)
	}
	if upgraded.Name != "dev" || upgraded.From != "two" || upgraded.CPUs != 3 || upgraded.Data == nil {
		t.Fatalf("upgraded %+v from %+v", upgraded, dev)
	}
	if got, err := e.Get(ctx, "dev"); err != nil || got.From != "two" {
		t.Fatalf("the upgraded VM is found as %+v, %v", got, err)
	}
}
