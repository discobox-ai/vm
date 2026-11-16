package boxd

import (
	"context"
	"runtime"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/discobox-ai/vm/pkg/engine"
	"github.com/discobox-ai/vm/pkg/machine"
)

// TestBootAsksStartVmFirst boots a machine from each state boxd can leave it
// in: StartVm is the first call, with nothing read before it, and the wait
// boots whatever StartVm leaves: a stop still settling is waited out and
// started, a hibernated machine woken, a suspended one resumed by StartVm
// itself, as boxd does.
func TestBootAsksStartVmFirst(t *testing.T) {
	defer func(every time.Duration) { waitEvery = every }(waitEvery)
	waitEvery = 20 * time.Millisecond
	for _, from := range []string{"stopped", "stopping", "hibernated", "suspended", "running"} {
		t.Run(from, func(t *testing.T) {
			d, fake := fakeDriver(t, t.TempDir())
			ctx := context.Background()
			inst, err := d.Create(ctx, machine.InstanceSpec{Name: "boot"})
			if err != nil {
				t.Fatal(err)
			}
			fake.mu.Lock()
			v := fake.vms[inst.ID]
			v.status = from
			fake.mu.Unlock()
			if from == "stopping" {
				go func() {
					time.Sleep(100 * time.Millisecond)
					fake.mu.Lock()
					v.status = "stopped"
					fake.mu.Unlock()
				}()
			}
			fake.takeCalls()
			m, err := d.Boot(ctx, inst, machine.BootOptions{})
			if err != nil {
				t.Fatalf("boot from %s: %v", from, err)
			}
			t.Cleanup(func() { _ = m.Kill(context.Background()) })
			calls := fake.takeCalls()
			if len(calls) == 0 || calls[0] != "StartVm" {
				t.Fatalf("boot from %s called %v; want StartVm first", from, calls)
			}
			fake.mu.Lock()
			state := v.status
			fake.mu.Unlock()
			if state != "running" {
				t.Fatalf("boot from %s left the machine %s (calls %v)", from, state, calls)
			}
		})
	}
}

// TestBootResizes boots a machine an instance asks another size of: the
// machine StartVm started at its own is flushed, stopped, resized and
// started again, and runs at the size asked, though its stop shows only
// after StopVm answers.
func TestBootResizes(t *testing.T) {
	if runtime.GOOS != "linux" {
		t.Skip("the fake boxd's guests run on the host, and boxd guests are linux")
	}
	defer func(every time.Duration) { waitEvery = every }(waitEvery)
	waitEvery = 20 * time.Millisecond
	d, fake := fakeDriver(t, t.TempDir())
	ctx := context.Background()
	inst, err := d.Create(ctx, machine.InstanceSpec{Name: "grow", CPUs: 2, Memory: 4 << 30})
	if err != nil {
		t.Fatal(err)
	}
	inst.CPUs, inst.Memory = 4, 8<<30
	// The stop settles after StopVm answers: reads still say running.
	fake.mu.Lock()
	fake.stopLag = 3
	fake.mu.Unlock()
	fake.takeCalls()
	m, err := d.Boot(ctx, inst, machine.BootOptions{})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = m.Kill(context.Background()) })
	calls := fake.takeCalls()
	fake.mu.Lock()
	v := fake.vms[inst.ID]
	vcpu, memory, state := v.vcpu, v.memory, v.status
	fake.mu.Unlock()
	if vcpu != 4 || memory != 8<<30 || state != "running" {
		t.Fatalf("the machine is %s at %d vCPUs, %d bytes (calls %v); want running at 4, %d", state, vcpu, memory, calls, 8<<30)
	}
	if !slices.Contains(calls, "StopVm") || !slices.Contains(calls, "ResizeVm") {
		t.Fatalf("calls %v; want a stop and a resize", calls)
	}
}

// TestLookupsReadByName finds VMs through the engine as a start and a
// create from a committed VM do: a committed VM is one read of its snapshot,
// a machine that and one of the machine, and neither lists the region.
func TestLookupsReadByName(t *testing.T) {
	if runtime.GOOS != "linux" {
		t.Skip("the fake boxd's guests run on the host, and boxd guests are linux")
	}
	d, fake := fakeDriver(t, t.TempDir())
	ctx := context.Background()
	e, err := engine.Open(d, "")
	if err != nil {
		t.Fatal(err)
	}
	base := commitBase(t, e, "base")
	inst, err := e.Create(ctx, engine.CreateOptions{Name: "sbx", From: base.Name})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = e.Remove(context.Background(), inst, true) })

	fake.takeCalls()
	if got, err := e.Get(ctx, "base"); err != nil || got.ID != base.ID {
		t.Fatalf("get base: %+v, %v", got, err)
	}
	if calls := fake.takeCalls(); !slices.Equal(calls, []string{"GetSnapshot"}) {
		t.Fatalf("finding a committed VM called %v; want GetSnapshot alone", calls)
	}
	if got, err := e.Get(ctx, "sbx"); err != nil || got.ID != inst.ID {
		t.Fatalf("get sbx: %+v, %v", got, err)
	}
	if calls := fake.takeCalls(); !slices.Equal(calls, []string{"GetSnapshot", "GetVm"}) {
		t.Fatalf("finding a machine called %v; want GetSnapshot, GetVm", calls)
	}
	if _, err := e.Create(ctx, engine.CreateOptions{Name: "sbx2", From: base.Name}); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if got, err := e.Get(context.Background(), "sbx2"); err == nil {
			_ = e.Remove(context.Background(), got, true)
		}
	})
	if calls := fake.takeCalls(); slices.Contains(calls, "ListVms") || slices.Contains(calls, "ListSnapshots") {
		t.Fatalf("a create from a committed VM listed the region: %v", calls)
	}
}

// TestBootEndsWhenTheMachineStops boots a machine that is stopped once it
// has come up, as a concurrent stop would: the boot fails, and does not
// start it again.
func TestBootEndsWhenTheMachineStops(t *testing.T) {
	defer func(every time.Duration) { waitEvery = every }(waitEvery)
	waitEvery = 20 * time.Millisecond
	d, fake := fakeDriver(t, t.TempDir())
	ctx := context.Background()
	inst, err := d.Create(ctx, machine.InstanceSpec{Name: "stopped"})
	if err != nil {
		t.Fatal(err)
	}
	fake.mu.Lock()
	fake.startingReads = 1000
	v := fake.vms[inst.ID]
	fake.mu.Unlock()
	go func() {
		// Once the wait has seen the machine starting, stop it.
		for {
			time.Sleep(10 * time.Millisecond)
			fake.mu.Lock()
			seen := slices.Contains(fake.calls, "GetVm")
			if seen {
				v.status, v.starting = "stopped", 0
			}
			fake.mu.Unlock()
			if seen {
				return
			}
		}
	}()
	fake.takeCalls()
	if _, err := d.Boot(ctx, inst, machine.BootOptions{}); err == nil || !strings.Contains(err.Error(), "stopped before it ran") {
		t.Fatalf("boot of a machine stopped meanwhile: %v; want it to fail", err)
	}
	if calls := fake.takeCalls(); countOf(calls, "StartVm") != 1 {
		t.Fatalf("the boot started the machine again: %v", calls)
	}
}

// countOf is how many of calls are call.
func countOf(calls []string, call string) int {
	n := 0
	for _, c := range calls {
		if c == call {
			n++
		}
	}
	return n
}

// TestBootLeavesARunningMachinesSize boots a machine that already runs at
// another size than its instance asks: Boot did not start it, so it is left
// running at its size, not stopped to resize it.
func TestBootLeavesARunningMachinesSize(t *testing.T) {
	defer func(every time.Duration) { waitEvery = every }(waitEvery)
	waitEvery = 20 * time.Millisecond
	d, fake := fakeDriver(t, t.TempDir())
	ctx := context.Background()
	inst, err := d.Create(ctx, machine.InstanceSpec{Name: "busy", CPUs: 2, Memory: 4 << 30})
	if err != nil {
		t.Fatal(err)
	}
	fake.mu.Lock()
	fake.vms[inst.ID].status = "running"
	fake.mu.Unlock()
	inst.CPUs, inst.Memory = 4, 8<<30
	fake.takeCalls()
	m, err := d.Boot(ctx, inst, machine.BootOptions{})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = m.Kill(context.Background()) })
	if calls := fake.takeCalls(); slices.Contains(calls, "StopVm") || slices.Contains(calls, "ResizeVm") {
		t.Fatalf("a running machine was stopped to resize it: %v", calls)
	}
}
