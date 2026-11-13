package engine_test

import (
	"context"
	"testing"

	"github.com/discobox-ai/vm/pkg/engine"
	"github.com/discobox-ai/vm/pkg/machine"
	"github.com/discobox-ai/vm/pkg/machine/fake"
	"github.com/discobox-ai/vm/pkg/machine/local"
)

// An install records the release its media resolved to (vm#6, discobox ADR
// 26-10-09-106 §5), and an embedder reads it from the engine: on the
// committed VM, found again later, on every VM created from it, on what that
// one commits, and on a VM upgraded to it; a fresh VM has none, nor does a
// VM upgraded to a fresh root.
func TestInstallRecordsItsRelease(t *testing.T) {
	ctx := context.Background()
	driver, err := local.New(machine.Config{Dir: t.TempDir()}, &fake.Hypervisor{BaseName: "stock"})
	if err != nil {
		t.Fatal(err)
	}
	e, err := engine.Open(driver, "")
	if err != nil {
		t.Fatal(err)
	}
	want := fake.FakeRelease
	has := func(what string, inst engine.Instance, want *machine.Release) {
		t.Helper()
		switch {
		case want == nil && inst.Release != nil:
			t.Fatalf("%s has release %v, want none", what, *inst.Release)
		case want != nil && (inst.Release == nil || *inst.Release != *want):
			t.Fatalf("%s has release %v, want %v", what, inst.Release, *want)
		}
	}
	base, err := e.Install(ctx, "base", engine.InstallOptions{Media: "latest"})
	if err != nil {
		t.Fatal(err)
	}
	has("the install", base, &want)
	if got, err := e.Get(ctx, "base"); err != nil {
		t.Fatal(err)
	} else {
		has("the install, found again", got, &want)
	}
	one, err := e.Create(ctx, engine.CreateOptions{From: "base", Name: "one", Data: &machine.DataVolume{}})
	if err != nil {
		t.Fatal(err)
	}
	has("a VM created from it", one, &want)
	committed, err := e.Commit(ctx, one)
	if err != nil {
		t.Fatal(err)
	}
	has("that VM, committed", committed, &want)
	two, err := e.Create(ctx, engine.CreateOptions{From: "one", Name: "two"})
	if err != nil {
		t.Fatal(err)
	}
	has("a VM of the committed copy", two, &want)
	fresh, err := e.Create(ctx, engine.CreateOptions{Name: "fresh", Data: &machine.DataVolume{}})
	if err != nil {
		t.Fatal(err)
	}
	has("a fresh VM", fresh, nil)
	upgraded, err := e.Upgrade(ctx, "fresh", engine.UpgradeOptions{From: "base"})
	if err != nil {
		t.Fatal(err)
	}
	has("a fresh VM upgraded to the install", upgraded, &want)
	if got, err := e.Get(ctx, "fresh"); err != nil {
		t.Fatal(err)
	} else {
		has("the upgraded VM, found again", got, &want)
	}
	back, err := e.Upgrade(ctx, "fresh", engine.UpgradeOptions{})
	if err != nil {
		t.Fatal(err)
	}
	has("a VM upgraded to a fresh root", back, nil)
}
