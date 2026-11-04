package engine_test

import (
	"context"
	"errors"
	"runtime"
	"strings"
	"testing"

	"github.com/discobox-ai/vm/pkg/engine"
	"github.com/discobox-ai/vm/pkg/machine"
	"github.com/discobox-ai/vm/pkg/machine/fake"
	"github.com/discobox-ai/vm/pkg/machine/local"
)

// fakeDriver is the fake driver for a pool, keeping its state in root.
func fakeDriver(t *testing.T, root, pool string) machine.Driver {
	t.Helper()
	d, err := local.New(machine.Config{Dir: root, Pool: pool}, &fake.Hypervisor{})
	if err != nil {
		t.Fatal(err)
	}
	return d
}

// A VM is installed committed, created from, committed in turn, and created
// from again once the first is removed; a fresh VM needs a base, and a VM
// from one the driver does not have is not found.
func TestInstallCommitAndCreateFrom(t *testing.T) {
	ctx := context.Background()
	e, err := engine.Open(fakeDriver(t, t.TempDir(), ""), "")
	if err != nil {
		t.Fatal(err)
	}
	base, err := e.Install(ctx, "base", engine.InstallOptions{})
	if err != nil || base.Name != "base" || base.GuestOS != machine.OS(runtime.GOOS) {
		t.Fatalf("install = %+v, %v", base, err)
	}
	if _, err := e.Install(ctx, "base", engine.InstallOptions{}); !errors.Is(err, machine.ErrExists) {
		t.Fatalf("a second install named base: %v, want ErrExists", err)
	}
	one, err := e.Create(ctx, engine.CreateOptions{From: "base", Name: "one"})
	if err != nil || one.From != "base" {
		t.Fatalf("create = %+v, %v", one, err)
	}
	committed, err := e.Commit(ctx, one)
	if err != nil || committed.Name != "one" {
		t.Fatalf("commit = %+v, %v", committed, err)
	}
	if err := e.Remove(ctx, base, false); err != nil {
		t.Fatalf("remove base, which the fake does not chain: %v", err)
	}
	two, err := e.Create(ctx, engine.CreateOptions{From: "one", Name: "two"})
	if err != nil || two.From != "one" {
		t.Fatalf("create from a committed VM = %+v, %v", two, err)
	}
	if list, err := e.List(ctx); err != nil || len(list) != 2 {
		t.Fatalf("VMs = %+v, %v", list, err)
	}
	if _, err := e.Create(ctx, engine.CreateOptions{From: "nothing"}); !errors.Is(err, engine.ErrNotFound) {
		t.Fatalf("create from no VM: %v, want ErrNotFound", err)
	}
	if _, err := e.Create(ctx, engine.CreateOptions{Name: "fresh"}); !errors.Is(err, machine.ErrUnsupported) {
		t.Fatalf("a fresh VM with no base: %v, want ErrUnsupported", err)
	}
}

func TestOpenRefusesABadPool(t *testing.T) {
	for _, pool := range []string{"Foo", "a--b", "-a", "a-", "a_b", strings.Repeat("a", 33)} {
		if _, err := engine.Open(fakeDriver(t, t.TempDir(), ""), pool); err == nil {
			t.Errorf("pool %q was accepted", pool)
		}
	}
}

// A fence is refused at once by an engine whose driver cannot fence, and
// nothing is made.
func TestEgressNeedsADriverThatFences(t *testing.T) {
	ctx := context.Background()
	e, err := engine.Open(fakeDriver(t, t.TempDir(), ""), "")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := e.Install(ctx, "base", engine.InstallOptions{}); err != nil {
		t.Fatal(err)
	}
	if _, err := e.Create(ctx, engine.CreateOptions{From: "base", Name: "fenced", Egress: &machine.Egress{}}); !errors.Is(err, machine.ErrUnsupported) {
		t.Fatalf("a fence on the fake driver: %v, want ErrUnsupported", err)
	}
	if list, err := e.List(ctx); err != nil || len(list) != 1 {
		t.Fatalf("VMs after a refused fence, the installed one alone: %+v, %v", list, err)
	}
}

// withFrom is opts, creating from the committed VM from.
func withFrom(opts engine.CreateOptions, from string) engine.CreateOptions {
	opts.From = from
	return opts
}

// noCommit is a driver that does not report Capabilities.Commit, over one
// that would commit, so a refusal proves the engine never asked it.
type noCommit struct{ machine.Driver }

func (n noCommit) Capabilities() machine.Capabilities {
	caps := n.Driver.Capabilities()
	caps.Commit, caps.Install = false, nil
	return caps
}

// An engine whose driver does not report Commit refuses a commit and a
// copy before it reaches the driver: the VM keeps its data volume, and
// nothing is made (ADR 26-11-03-868).
func TestCommitNeedsADriverThatCommits(t *testing.T) {
	ctx := context.Background()
	d, err := local.New(machine.Config{Dir: t.TempDir()}, &fake.Hypervisor{BaseName: "stock"})
	if err != nil {
		t.Fatal(err)
	}
	e, err := engine.Open(noCommit{d}, "")
	if err != nil {
		t.Fatal(err)
	}
	vm, err := e.Create(ctx, engine.CreateOptions{Name: "vm", Data: &machine.DataVolume{}})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := e.Commit(ctx, vm); !errors.Is(err, machine.ErrUnsupported) || !strings.Contains(err.Error(), "fake") {
		t.Fatalf("commit on a driver that does not: %v, want ErrUnsupported naming the driver", err)
	}
	if got, err := e.Get(ctx, "vm"); err != nil || got.Data == nil {
		t.Fatalf("a refused commit changed the VM: %+v, %v", got, err)
	}
	if _, err := e.Create(ctx, engine.CreateOptions{Name: "copy", From: "vm"}); !errors.Is(err, machine.ErrUnsupported) {
		t.Fatalf("a copy on a driver that does not commit: %v, want ErrUnsupported", err)
	}
	if list, err := e.List(ctx); err != nil || len(list) != 1 {
		t.Fatalf("VMs after a refused copy, vm alone: %+v, %v", list, err)
	}
}
