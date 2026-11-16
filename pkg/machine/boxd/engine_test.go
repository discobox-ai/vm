package boxd

import (
	"context"
	"errors"
	"fmt"
	"io"
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

// TestEngineRunsInstances drives the engine against the fake boxd API: a VM
// created from a committed one, started, state, the guest through the engine, stop, start again, a shutdown
// through the guest's operations (boxd's StopVm) found by the next state, and
// remove. Nothing is kept on the host: a second engine, as on another host,
// sees the same committed VM and VM, and drives them.
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
	base := commitBase(t, e, "base")
	inst, err := e.Create(ctx, engine.CreateOptions{Name: "remote", From: base.Name})
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	t.Cleanup(func() { _ = e.Remove(context.Background(), inst, true) })

	// Another engine, made from nothing but the API, as another host's is.
	elsewhere, err := engine.Open(&Driver{api: d.api, pool: machine.DefaultPool}, "")
	if err != nil {
		t.Fatal(err)
	}
	if got, err := elsewhere.Get(ctx, "base"); err != nil || got.ID != base.ID {
		t.Fatalf("another engine's committed VM base = %+v, %v", got, err)
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

// TestEngineKeepsPoolsApart checks that a pool lists only what it made,
// and that what boxd holds under no pool's name is not listed.
func TestEngineKeepsPoolsApart(t *testing.T) {
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

	base, err := commitFresh(ctx, d, "base")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := d.Create(ctx, machine.InstanceSpec{Name: "web", From: &base}); err != nil {
		t.Fatal(err)
	}
	other := &Driver{api: d.api, pool: "other"}
	if list, err := other.Instances(ctx); err != nil || len(list) != 0 {
		t.Fatalf("another pool's VMs = %+v, %v", list, err)
	}
	if _, err := other.Instance(ctx, "base"); !errors.Is(err, machine.ErrNotFound) {
		t.Fatalf("another pool found the default's committed VM: %v", err)
	}
	list, err := d.Instances(ctx)
	if err != nil || len(list) != 2 {
		t.Fatalf("the default pool's VMs = %+v, %v", list, err)
	}
	var names []string
	for _, inst := range list {
		names = append(names, fmt.Sprintf("%s:%v", inst.Name, inst.ID == base.ID))
	}
	slices.Sort(names)
	if want := []string{"base:true", "web:false"}; !slices.Equal(names, want) {
		t.Fatalf("the default pool's VMs = %v, want %v", names, want)
	}
}

// TestEngineCommitsAVM makes a fresh VM of boxd's default image at the size
// asked for, writes to it, commits it, and creates a VM from the committed
// one that holds the write, again at the size asked for. The committed VM,
// a snapshot, never starts.
func TestEngineCommitsAVM(t *testing.T) {
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

	inst, err := e.Create(ctx, engine.CreateOptions{Name: "fresh", CPUs: 8, Memory: 32 << 30})
	if err != nil {
		t.Fatalf("create a fresh VM: %v", err)
	}
	if v := machineOf("fresh"); v.vcpu != 8 || v.memory != 32<<30 || v.status != "stopped" || v.imageRef != fake.defaultImage {
		t.Errorf("the fresh machine is %d CPUs, %d bytes, %s, of %q; want 8, 32 GiB, stopped, of the default image", v.vcpu, v.memory, v.status, v.imageRef)
	}
	if got := e.State(ctx, inst); got != engine.Stopped {
		t.Fatalf("state before the first start = %s", got)
	}
	if _, err := e.URL(ctx, inst, machine.URLIngress); err != nil {
		t.Fatalf("the URL of a stopped VM: %v", err)
	}
	if err := e.Start(ctx, inst, engine.StartOptions{Timeout: 30 * time.Second}); err != nil {
		t.Fatalf("start: %v", err)
	}
	g, err := e.Guest(ctx, inst)
	if err != nil {
		t.Fatal(err)
	}
	write := machine.ExecRequest{Argv: []string{"sh", "-c", "mkdir -p etc && echo yes > etc/written"}}
	if code, err := machine.Run(ctx, g, write, nil, io.Discard, io.Discard); err != nil || code != 0 {
		t.Fatalf("write: %d, %v", code, err)
	}
	if err := e.Stop(ctx, inst, 30*time.Second); err != nil {
		t.Fatalf("stop: %v", err)
	}
	committed, err := e.Commit(ctx, inst)
	if err != nil {
		t.Fatalf("commit: %v", err)
	}
	t.Cleanup(func() { _ = e.Remove(context.Background(), committed, true) })
	if got := e.State(ctx, committed); got != engine.Stopped {
		t.Fatalf("state of a committed VM = %s", got)
	}
	if err := e.Start(ctx, committed, engine.StartOptions{Timeout: 30 * time.Second}); !errors.Is(err, machine.ErrCommitted) {
		t.Fatalf("start of a committed VM: %v, want ErrCommitted", err)
	}

	from, err := e.Create(ctx, engine.CreateOptions{Name: "again", From: "fresh", CPUs: 4, Memory: 16 << 30, URLs: machine.URLRequests{Ingress: machine.URLRequest{Mode: machine.URLOff, Strict: true}}})
	if err != nil {
		t.Fatalf("create from the committed VM: %v", err)
	}
	t.Cleanup(func() { _ = e.Remove(context.Background(), from, true) })
	if v := machineOf("again"); v.vcpu != 4 || v.memory != 16<<30 {
		t.Fatalf("the copy's machine = %+v; want it made at 4 CPUs and 16 GiB", v)
	}
	// Its ingress URL asked off, it has no URL, and the engine says why.
	if _, err := e.URL(ctx, from, machine.URLIngress); !errors.Is(err, machine.ErrUnsupported) {
		t.Fatalf("the URL of a VM whose ingress URL is asked off: %v, want ErrUnsupported", err)
	}
	if err := e.Start(ctx, from, engine.StartOptions{Timeout: 30 * time.Second}); err != nil {
		t.Fatalf("start the copy: %v", err)
	}
	if g, err = e.Guest(ctx, from); err != nil {
		t.Fatal(err)
	}
	var out strings.Builder
	if code, err := machine.Run(ctx, g, machine.ExecRequest{Argv: []string{"cat", "etc/written"}}, nil, &out, &out); err != nil || code != 0 || out.String() != "yes\n" {
		t.Fatalf("the copy does not hold the committed disk: %d, %v: %q", code, err, out.String())
	}
}

// commitBase makes the committed VM name, as every boxd one is made: a fresh
// VM of boxd's default image, committed.
func commitBase(t *testing.T, e *engine.Engine, name string) engine.Instance {
	t.Helper()
	ctx := context.Background()
	inst, err := e.Create(ctx, engine.CreateOptions{Name: name})
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	committed, err := e.Commit(ctx, inst)
	if err != nil {
		t.Fatalf("commit: %v", err)
	}
	t.Cleanup(func() { _ = e.Remove(context.Background(), committed, true) })
	return committed
}

// TestAFailedSnapshotIsRemovable checks that a failed snapshot with no
// machine of its name, which an earlier Destroy left, is listed, and that
// the engine removes it.
func TestAFailedSnapshotIsRemovable(t *testing.T) {
	d, fake := fakeDriver(t, t.TempDir())
	ctx := context.Background()
	name := "dvm-default--build"
	fake.mu.Lock()
	fake.snapshots[name] = &fakeSnapshot{id: "snap_build", name: name, dir: t.TempDir(), status: "failed"}
	fake.mu.Unlock()
	e, err := engine.Open(d, "")
	if err != nil {
		t.Fatal(err)
	}
	inst, err := e.Get(ctx, "build")
	if err != nil {
		t.Fatalf("the failed snapshot is not listed: %v", err)
	}
	if err := e.Start(ctx, inst, engine.StartOptions{}); !errors.Is(err, machine.ErrCommitted) {
		t.Fatalf("a start of the failed snapshot: %v, want ErrCommitted", err)
	}
	if err := e.Remove(ctx, inst, false); err != nil {
		t.Fatal(err)
	}
	fake.mu.Lock()
	_, left := fake.snapshots[name]
	fake.mu.Unlock()
	if left {
		t.Fatal("the failed snapshot is left after its Remove")
	}
}

// TestStartDoesNotWaitForSystemd starts a VM whose `systemctl
// is-system-running --wait` never answers, as one sent early in a boot can
// hang until its timeout: the start returns once boxd says the machine runs,
// with the guest's service file written, and waits on no systemd. A stop
// straight after flushes the guest and stops it, waiting on no systemd either.
func TestStartDoesNotWaitForSystemd(t *testing.T) {
	if runtime.GOOS != "linux" {
		t.Skip("the fake boxd's guests run on the host, and boxd guests are linux")
	}
	dir := t.TempDir()
	d, fake := fakeDriver(t, dir)
	ctx := context.Background()
	e, err := engine.Open(d, "")
	if err != nil {
		t.Fatal(err)
	}
	inst, err := e.Create(ctx, engine.CreateOptions{Name: "early"})
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	t.Cleanup(func() { _ = e.Remove(context.Background(), inst, true) })

	fake.readyHangs.Store(true)
	began := time.Now()
	if err := e.Start(ctx, inst, engine.StartOptions{Timeout: 15 * time.Second}); err != nil {
		t.Fatalf("start after %s: %v", time.Since(began).Round(time.Second), err)
	}
	if took := time.Since(began); took > 5*time.Second {
		t.Errorf("start took %s; want it to return once the machine runs", took.Round(time.Second))
	}
	readService(t, e, inst)

	began = time.Now()
	if err := e.Stop(ctx, inst, 15*time.Second); err != nil {
		t.Fatalf("stop: %v", err)
	}
	if took := time.Since(began); took > 5*time.Second {
		t.Errorf("stop took %s; want it to flush the guest and stop it at once", took.Round(time.Second))
	}
	if state := e.State(ctx, inst); state != engine.Stopped {
		t.Fatalf("after the stop: %s", state)
	}
}

// TestStartRetriesAnUnreachableGuest starts a VM whose first Execs boxd
// answers Unavailable, as for a guest it cannot reach yet: the start writes
// the service file once one reaches the guest, and does not fail.
func TestStartRetriesAnUnreachableGuest(t *testing.T) {
	if runtime.GOOS != "linux" {
		t.Skip("the fake boxd's guests run on the host, and boxd guests are linux")
	}
	d, fake := fakeDriver(t, t.TempDir())
	ctx := context.Background()
	e, err := engine.Open(d, "")
	if err != nil {
		t.Fatal(err)
	}
	inst, err := e.Create(ctx, engine.CreateOptions{Name: "unreachable"})
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	t.Cleanup(func() { _ = e.Remove(context.Background(), inst, true) })

	fake.unreachable.Store(3)
	if err := e.Start(ctx, inst, engine.StartOptions{Timeout: 15 * time.Second}); err != nil {
		t.Fatalf("start: %v", err)
	}
	if left := fake.unreachable.Load(); left > 0 {
		t.Fatalf("%d unreachable Execs were never sent", left)
	}
	readService(t, e, inst)
}

// TestStartFailsAGuestThatAnswersAnError starts a VM whose service file
// cannot be written: the copy reached the guest and failed, so the start
// fails rather than trying again until its timeout.
func TestStartFailsAGuestThatAnswersAnError(t *testing.T) {
	if runtime.GOOS != "linux" {
		t.Skip("the fake boxd's guests run on the host, and boxd guests are linux")
	}
	dir := t.TempDir()
	d, fake := fakeDriver(t, dir)
	ctx := context.Background()
	e, err := engine.Open(d, "")
	if err != nil {
		t.Fatal(err)
	}
	inst, err := e.Create(ctx, engine.CreateOptions{Name: "refuses"})
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	t.Cleanup(func() { _ = e.Remove(context.Background(), inst, true) })
	// A file where the service file's directory goes: the copy's mkdir fails.
	fake.mu.Lock()
	root := fake.root(fake.vms[inst.ID])
	fake.mu.Unlock()
	if err := os.MkdirAll(filepath.Join(root, "var/lib"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "var/lib/disco-vm"), nil, 0o644); err != nil {
		t.Fatal(err)
	}
	began := time.Now()
	err = e.Start(ctx, inst, engine.StartOptions{Timeout: 15 * time.Second})
	if err == nil {
		t.Fatal("a start whose service file could not be written succeeded")
	}
	if errors.Is(err, machine.ErrUnreachable) || time.Since(began) > 5*time.Second {
		t.Fatalf("start failed after %s with %v; want the copy's own error at once", time.Since(began).Round(time.Second), err)
	}
}

// readService reads the guest's service file through the engine.
func readService(t *testing.T, e *engine.Engine, inst engine.Instance) {
	t.Helper()
	ctx := context.Background()
	g, err := e.Guest(ctx, inst)
	if err != nil {
		t.Fatal(err)
	}
	var out strings.Builder
	read := machine.ExecRequest{Argv: []string{"cat", "var/lib/disco-vm/service"}}
	if code, err := machine.Run(ctx, g, read, nil, &out, io.Discard); err != nil || code != 0 || out.Len() == 0 {
		t.Fatalf("the guest's service file: %d, %v, %q", code, err, out.String())
	}
}
