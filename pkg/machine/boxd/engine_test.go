package boxd

import (
	"context"
	"os"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/discobox-ai/vm/pkg/engine"
	"github.com/discobox-ai/vm/pkg/machine"
)

// TestEngineRunsInstances drives the engine against the fake boxd API: build,
// start, state, the guest through the engine, stop, start again, a shutdown
// through the guest's operations (boxd's StopVm) found by the next state, and
// remove. Nothing is kept on the host: a second engine, as on another host,
// sees the same image and instance, and drives them.
func TestEngineRunsInstances(t *testing.T) {
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

	e, err := engine.Open(d, "")
	if err != nil {
		t.Fatal(err)
	}
	snapshotImage(t, e, "t/base")
	inst, err := e.Create(ctx, "t/base", engine.CreateOptions{Name: "remote"})
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	t.Cleanup(func() { _ = e.Remove(context.Background(), inst, true) })

	// Another engine, made from nothing but the API, as another host's is.
	elsewhere, err := engine.Open(&Driver{api: d.api, project: machine.DefaultProject}, "")
	if err != nil {
		t.Fatal(err)
	}
	if images, err := elsewhere.Images(ctx); err != nil || len(images) != 1 || images[0].Ref != "t/base:latest" {
		t.Fatalf("another engine's images = %+v, %v", images, err)
	}
	if got, err := elsewhere.Get(ctx, "remote"); err != nil || got.ID != inst.ID {
		t.Fatalf("another engine's instance remote = %+v, %v", got, err)
	}

	start := func(e *engine.Engine) {
		t.Helper()
		if err := e.Start(ctx, inst, engine.StartOptions{Timeout: 30 * time.Second}); err != nil {
			t.Fatalf("start: %v", err)
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

	start(e)
	if err := elsewhere.Stop(ctx, inst, 30*time.Second); err != nil {
		t.Fatalf("stop from another engine: %v", err)
	}
	waitState(engine.Stopped)

	start(elsewhere)
	// A shutdown through the guest's operations is boxd's stop, and nothing
	// watches it: the next state reads it stopped.
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
	if _, err := elsewhere.Get(ctx, "remote"); err == nil {
		t.Fatal("another engine still finds a removed instance")
	}
	fake.mu.Lock()
	defer fake.mu.Unlock()
	for _, v := range fake.vms {
		if strings.HasSuffix(v.name, "--remote") {
			t.Fatalf("machine %s outlived its instance", v.name)
		}
	}
}

// TestEngineKeepsProjectsApart checks that a project lists only what it made,
// and that what boxd holds under no project's name is not listed.
func TestEngineKeepsProjectsApart(t *testing.T) {
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
	fake.mu.Lock()
	fake.snapshots["my-snapshot"] = &fakeSnapshot{id: "snap_mine", name: "my-snapshot", dir: filepath.Join(dir, "mine")}
	fake.mu.Unlock()
	if _, err := fake.CreateVm(ctx, nil); err != nil {
		t.Fatal(err)
	}

	layer, err := snapshotOfBase(ctx, d, "0123456789abcdef")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := d.Create(ctx, machine.InstanceSpec{Name: "web", ImageID: layer.ID, GuestOS: machine.Linux}); err != nil {
		t.Fatal(err)
	}
	other := &Driver{api: d.api, project: "other"}
	if images, err := other.Images(ctx); err != nil || len(images) != 0 {
		t.Fatalf("another project's images = %+v, %v", images, err)
	}
	if list, err := other.Instances(ctx); err != nil || len(list) != 0 {
		t.Fatalf("another project's instances = %+v, %v", list, err)
	}
	if _, err := other.Create(ctx, machine.InstanceSpec{Name: "web", ImageID: layer.ID, GuestOS: machine.Linux}); err == nil {
		t.Fatal("another project made an instance of the default's image")
	}
	list, err := d.Instances(ctx)
	if err != nil || len(list) != 1 || list[0].Name != "web" {
		t.Fatalf("the default project's instances = %+v, %v", list, err)
	}
	images, err := d.Images(ctx)
	if err != nil || len(images) != 1 || images[0].ID != layer.ID {
		t.Fatalf("the default project's images = %+v, %v", images, err)
	}
}

// TestEngineRunsABase lists boxd's default image and the snapshots it did not
// make as bases, and none of disco-vm's own snapshots of any project, then
// runs an instance of each with no image, made from the snapshot
// or the image at the size asked for; and an instance of a base snapshots to
// an image like any other.
func TestEngineRunsABase(t *testing.T) {
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

	mine := filepath.Join(dir, "snap_mine")
	if err := os.MkdirAll(filepath.Join(mine, "etc"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(mine, "etc", "made-by-hand"), []byte("yes\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	fake.mu.Lock()
	fake.snapshots["my-snapshot"] = &fakeSnapshot{id: "snap_mine", name: "my-snapshot", dir: mine}
	fake.snapshots["dvm-default--aaaa"] = &fakeSnapshot{id: "snap_a", name: "dvm-default--aaaa"}
	fake.snapshots["dvm-other--bbbb"] = &fakeSnapshot{id: "snap_b", name: "dvm-other--bbbb"}
	fake.mu.Unlock()
	machineOf := func(name string) *fakeVM {
		t.Helper()
		fake.mu.Lock()
		defer fake.mu.Unlock()
		v, err := fake.vm("dvm-default--" + name)
		if err != nil {
			t.Fatal(err)
		}
		return v
	}

	e, err := engine.Open(d, "")
	if err != nil {
		t.Fatal(err)
	}
	bases, err := d.Bases(ctx)
	if err != nil {
		t.Fatal(err)
	}
	var names []string
	for _, b := range bases {
		names = append(names, b.Name)
	}
	if want := []string{"ubuntu-24.04", "my-snapshot"}; !slices.Equal(names, want) {
		t.Fatalf("bases = %v, want %v", names, want)
	}

	inst, err := e.Create(ctx, "my-snapshot", engine.CreateOptions{Name: "by-hand", CPUs: 8, Memory: 32 << 30})
	if err != nil {
		t.Fatalf("create from a base: %v", err)
	}
	t.Cleanup(func() { _ = e.Remove(context.Background(), inst, true) })
	if v := machineOf("by-hand"); v.vcpu != 8 || v.memory != 32<<30 || v.status != "stopped" {
		t.Errorf("the snapshot's machine is %d CPUs, %d bytes, %s; want it made at 8 and 32 GiB, and stopped", v.vcpu, v.memory, v.status)
	}
	if got := e.State(ctx, inst); got != engine.Stopped {
		t.Fatalf("state before the first start = %s", got)
	}
	if _, err := e.URL(ctx, inst); err != nil {
		t.Fatalf("the URL of a stopped instance: %v", err)
	}
	if err := e.Start(ctx, inst, engine.StartOptions{Timeout: 30 * time.Second}); err != nil {
		t.Fatalf("start: %v", err)
	}
	g, err := e.Guest(ctx, inst)
	if err != nil {
		t.Fatal(err)
	}
	var out strings.Builder
	if code, err := machine.Run(ctx, g, machine.ExecRequest{Argv: []string{"cat", "etc/made-by-hand"}}, nil, &out, &out); err != nil || code != 0 || out.String() != "yes\n" {
		t.Fatalf("the instance does not hold the snapshot's disk: %d, %v: %q", code, err, out.String())
	}
	if err := e.Stop(ctx, inst, 30*time.Second); err != nil {
		t.Fatalf("stop: %v", err)
	}
	if _, err := e.Snapshot(ctx, inst, "t/by-hand"); err != nil {
		t.Fatalf("snapshot an instance of a base: %v", err)
	}
	from, err := e.Create(ctx, "t/by-hand", engine.CreateOptions{Name: "again"})
	if err != nil {
		t.Fatalf("create from the snapshot: %v", err)
	}
	t.Cleanup(func() { _ = e.Remove(context.Background(), from, true) })

	stock, err := e.Create(ctx, "ubuntu-24.04", engine.CreateOptions{Name: "stock", CPUs: 4, Memory: 16 << 30})
	if err != nil {
		t.Fatalf("create from the default image: %v", err)
	}
	t.Cleanup(func() { _ = e.Remove(context.Background(), stock, true) })
	if v := machineOf("stock"); v.vcpu != 4 || v.memory != 16<<30 {
		t.Fatalf("the default image's machine = %+v; want it made at 4 CPUs and 16 GiB", v)
	}

	// The hand-made snapshot is untouched, and disco-vm's own are no bases.
	fake.mu.Lock()
	_, kept := fake.snapshots["my-snapshot"]
	fake.mu.Unlock()
	if !kept {
		t.Fatal("the hand-made snapshot went")
	}
}

// snapshotImage makes an image tagged ref, as every boxd image is made: an
// instance of boxd's default image, snapshotted.
func snapshotImage(t *testing.T, e *engine.Engine, ref string) {
	t.Helper()
	ctx := context.Background()
	inst, err := e.Create(ctx, "ubuntu-24.04", engine.CreateOptions{Name: "for-" + strings.ReplaceAll(ref, "/", "-")})
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	if _, err := e.Snapshot(ctx, inst, ref); err != nil {
		t.Fatalf("snapshot: %v", err)
	}
}
