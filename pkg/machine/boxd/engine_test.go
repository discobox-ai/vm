package boxd

import (
	"context"
	"errors"
	"maps"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/discobox-ai/vm/pkg/build"
	"github.com/discobox-ai/vm/pkg/engine"
	"github.com/discobox-ai/vm/pkg/machine"
)

// TestEngineRunsRemoteInstancesWithoutAShim drives the engine's remote paths
// against the fake boxd API: start, state, the guest through the engine,
// stop, start again, a guest's own poweroff found by the next state, and
// remove. No shim is started at any point, and a shim.json an older
// disco-vm left behind is cleared.
func TestEngineRunsRemoteInstancesWithoutAShim(t *testing.T) {
	if runtime.GOOS != "linux" {
		t.Skip("the fake boxd's guests run on the host, and boxd guests are linux")
	}
	dir, err := os.MkdirTemp("", "boxd")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(dir) })
	d, fake := fakeDriver(t, dir)
	ctx := context.Background()

	e, err := engine.Open(t.TempDir(), d, nil, "")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = e.Close() })
	spec := filepath.Join(t.TempDir(), "disco-vm.yaml")
	if err := os.WriteFile(spec, []byte("from:\n  install: {os: linux, media: latest}\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	builder := &build.Builder{Engine: e, Out: testLog{t}}
	if _, err := builder.Build(ctx, build.Options{File: spec, Tags: []string{"t/base"}}); err != nil {
		t.Fatalf("build: %v", err)
	}
	inst, err := e.Create(ctx, "t/base", engine.CreateOptions{Name: "remote"})
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	t.Cleanup(func() { _ = e.Remove(context.Background(), inst, true) })

	instDir := filepath.Join(e.Root, "instances", inst.ID)
	// What an older disco-vm's shim left: a state file naming no live process.
	if err := os.WriteFile(filepath.Join(instDir, "shim.json"), []byte(`{"pid":0,"addr":"127.0.0.1:1","token":"x"}`), 0o600); err != nil {
		t.Fatal(err)
	}
	start := func() {
		t.Helper()
		if err := e.Start(ctx, inst, engine.StartOptions{Timeout: 30 * time.Second}); err != nil {
			t.Fatalf("start: %v", err)
		}
		if _, err := os.Stat(filepath.Join(instDir, "shim.json")); !os.IsNotExist(err) {
			t.Fatalf("a remote instance has a shim.json after start: %v", err)
		}
		if got := e.State(ctx, inst); got != engine.Running {
			t.Fatalf("state after start = %s", got)
		}
		g, err := e.Guest(ctx, inst)
		if err != nil {
			t.Fatalf("the guest through the engine: %v", err)
		}
		if _, err := g.Info(ctx); err != nil {
			t.Fatalf("the guest through the engine: %v", err)
		}
	}
	waitState := func(want engine.State) {
		t.Helper()
		deadline := time.Now().Add(30 * time.Second)
		for got := e.State(ctx, inst); got != want; got = e.State(ctx, inst) {
			if time.Now().After(deadline) {
				t.Fatalf("state = %s, want %s", got, want)
			}
			time.Sleep(200 * time.Millisecond)
		}
	}

	start()
	if err := e.Stop(ctx, inst, 30*time.Second); err != nil {
		t.Fatalf("stop: %v", err)
	}
	waitState(engine.Stopped)

	start()
	// The guest powers itself off and nothing watches it: the next state
	// finishes the halt.
	g, err := e.Guest(ctx, inst)
	if err != nil {
		t.Fatal(err)
	}
	if err := g.Shutdown(ctx, false); err != nil {
		t.Fatalf("guest shutdown: %v", err)
	}
	waitState(engine.Stopped)

	if err := e.Remove(ctx, inst, true); err != nil {
		t.Fatalf("remove: %v", err)
	}
	fake.mu.Lock()
	defer fake.mu.Unlock()
	for _, v := range fake.vms {
		if strings.Contains(v.name, inst.ID) {
			t.Fatalf("machine %s outlived its instance", v.name)
		}
	}
}

// A driver that lists Remote without being an Attacher would leave the engine
// running shims it says it does not need; Open refuses it.
func TestEngineRefusesRemoteWithoutAttacher(t *testing.T) {
	d, _ := fakeDriver(t, t.TempDir())
	notAttacher := struct{ machine.Driver }{d}
	if _, err := engine.Open(t.TempDir(), notAttacher, nil, ""); err == nil || !strings.Contains(err.Error(), "Attacher") {
		t.Fatalf("open: %v", err)
	}
}

// TestEngineListsOrphans checks that what boxd holds under disco-vm's names
// and the store has no record of is listed, and nothing else: not what the
// store records, and not the caller's own machines and snapshots. A store
// that is lost leaves everything it made an orphan.
func TestEngineListsOrphans(t *testing.T) {
	if runtime.GOOS != "linux" {
		t.Skip("the fake boxd's guests run on the host, and boxd guests are linux")
	}
	dir, err := os.MkdirTemp("", "boxd")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(dir) })
	d, fake := fakeDriver(t, dir)
	ctx := context.Background()

	e, err := engine.Open(t.TempDir(), d, nil, "")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = e.Close() })
	spec := filepath.Join(t.TempDir(), "disco-vm.yaml")
	if err := os.WriteFile(spec, []byte("from:\n  install: {os: linux, media: latest}\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	builder := &build.Builder{Engine: e, Out: testLog{t}}
	if _, err := builder.Build(ctx, build.Options{File: spec, Tags: []string{"t/base"}}); err != nil {
		t.Fatalf("build: %v", err)
	}
	inst, err := e.Create(ctx, "t/base", engine.CreateOptions{Name: "kept"})
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	t.Cleanup(func() { _ = e.Remove(context.Background(), inst, true) })
	layerID, err := e.Images.Resolve("t/base")
	if err != nil {
		t.Fatal(err)
	}
	chain, err := e.Images.MachineChain(layerID)
	if err != nil {
		t.Fatal(err)
	}

	names := func(e *engine.Engine) map[string]machine.OrphanKind {
		t.Helper()
		orphans, err := e.Orphans(ctx)
		if err != nil {
			t.Fatal(err)
		}
		out := map[string]machine.OrphanKind{}
		for _, o := range orphans {
			out[o.Name] = o.Kind
		}
		return out
	}
	if got := names(e); len(got) != 0 {
		t.Fatalf("orphans of a store that records everything: %v", got)
	}

	// Made behind the store's back: a machine and a snapshot under
	// disco-vm's names, and the caller's own of each, which are not
	// disco-vm's.
	// One is named as before projects, which is the default project's; a
	// third is another project's.
	fake.mu.Lock()
	lost := fake.newVM("dvm-default--lost-0000", 0, 0)
	fake.newVM("mine", 0, 0)
	fake.newVM("dvm-other--lost-0000", 0, 0)
	fake.snapshots["dvm-lost-snap"] = &fakeSnapshot{id: "snap_lost", name: "dvm-lost-snap"}
	fake.snapshots["my-snapshot"] = &fakeSnapshot{id: "snap_mine", name: "my-snapshot"}
	fake.mu.Unlock()
	want := map[string]machine.OrphanKind{lost.name: machine.OrphanMachine, "dvm-lost-snap": machine.OrphanLayer}
	if got := names(e); !maps.Equal(got, want) {
		t.Fatalf("orphans = %v, want %v", got, want)
	}
	otherProject, err := engine.Open(e.Root, d, nil, "other")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = otherProject.Close() })
	if got, want := names(otherProject), map[string]machine.OrphanKind{"dvm-other--lost-0000": machine.OrphanMachine}; !maps.Equal(got, want) {
		t.Fatalf("another project's orphans = %v, want %v", got, want)
	}
	fake.mu.Lock()
	var vmNames []string
	for _, v := range fake.vms {
		if v.id != lost.id {
			vmNames = append(vmNames, v.name)
		}
	}
	fake.mu.Unlock()
	for _, name := range vmNames {
		if strings.HasPrefix(name, "dvm-") && !strings.HasPrefix(name, "dvm-default--") && !strings.HasPrefix(name, "dvm-other--") {
			t.Fatalf("machine %s is not named for its project", name)
		}
	}

	// A second store on the same account knows none of the first's: its
	// instance's machine, its layer's snapshot, and its tag's are all
	// orphans to it.
	other, err := engine.Open(t.TempDir(), d, nil, "")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = other.Close() })
	got := names(other)
	var machines, layers int
	for _, kind := range got {
		if kind == machine.OrphanMachine {
			machines++
		} else {
			layers++
		}
	}
	if machines != 2 || layers != 3 {
		t.Fatalf("a lost store's orphans = %v, want 2 machines (the instance's and the lost one) and 3 snapshots (layer, tag, lost)", got)
	}
	for name := range got {
		if name != "dvm-lost-snap" && !strings.HasPrefix(name, "dvm-default--") {
			t.Fatalf("%s is not named for the default project", name)
		}
	}

	// An orphan is removed by its name; nothing the store records, and
	// nothing of another project's, is.
	if _, err := e.RemoveOrphan(ctx, machine.OrphanMachine, lost.name, false); err == nil || !strings.Contains(err.Error(), "running") {
		t.Fatalf("removing a running orphan without force: %v", err)
	}
	if _, err := e.RemoveOrphan(ctx, machine.OrphanMachine, lost.name, true); err != nil {
		t.Fatalf("remove orphan machine: %v", err)
	}
	if _, err := e.RemoveOrphan(ctx, machine.OrphanLayer, "dvm-lost-snap", false); err != nil {
		t.Fatalf("remove orphan snapshot: %v", err)
	}
	var layerSnap snapshotRef
	if err := readJSON(filepath.Join(chain[0].Dir, layerFile), &layerSnap); err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{layerSnap.Name, "dvm-other--lost-0000", "my-snapshot", "mine"} {
		for _, kind := range []machine.OrphanKind{machine.OrphanMachine, machine.OrphanLayer} {
			if _, err := e.RemoveOrphan(ctx, kind, name, true); !errors.Is(err, engine.ErrNotOrphan) {
				t.Fatalf("remove %s %s: %v, want ErrNotOrphan", kind, name, err)
			}
		}
	}
	if got := names(e); len(got) != 0 {
		t.Fatalf("orphans after removing them = %v", got)
	}
	fake.mu.Lock()
	_, layerKept := fake.snapshots[layerSnap.Name]
	kept := 0
	for _, v := range fake.vms {
		if v.name == "dvm-other--lost-0000" || v.name == "mine" {
			kept++
		}
	}
	fake.mu.Unlock()
	if !layerKept || kept != 2 {
		t.Fatalf("the store's snapshot kept = %v, other machines kept = %d of 2", layerKept, kept)
	}
}
