package boxd

import (
	"context"
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
// against the fake boxd API: start, state, the agent through the engine,
// stop, start again, a guest's own poweroff found by the next state, and
// remove. No shim is started at any point, and a shim.json an older
// disco-vm left behind is cleared.
func TestEngineRunsRemoteInstancesWithoutAShim(t *testing.T) {
	if runtime.GOOS != "linux" {
		t.Skip("the fake boxd's guests run on the host, and boxd guests are linux")
	}
	agent := linuxAgent(t)
	dir, err := os.MkdirTemp("", "boxd")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(dir) })
	d, fake := fakeDriver(t, agent, dir)
	t.Cleanup(fake.close)
	ctx := context.Background()

	e, err := engine.Open(t.TempDir(), d)
	if err != nil {
		t.Fatal(err)
	}
	spec := filepath.Join(t.TempDir(), "disco-vm.yaml")
	if err := os.WriteFile(spec, []byte("from:\n  install: {os: linux, media: latest}\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	builder := &build.Builder{Engine: e, Out: testLog{t}}
	if _, err := builder.Build(ctx, build.Options{File: spec, Tags: []string{"t/base"}, Agent: agent}); err != nil {
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
		client := e.Guest(inst)
		defer client.Close()
		if _, err := client.Info(ctx); err != nil {
			t.Fatalf("the agent through the engine: %v", err)
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
	client := e.Guest(inst)
	err = client.Shutdown(ctx, false)
	client.Close()
	if err != nil {
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
	d, fake := fakeDriver(t, "unused", t.TempDir())
	t.Cleanup(fake.close)
	notAttacher := struct{ machine.Driver }{d}
	if _, err := engine.Open(t.TempDir(), notAttacher); err == nil || !strings.Contains(err.Error(), "Attacher") {
		t.Fatalf("open: %v", err)
	}
}
