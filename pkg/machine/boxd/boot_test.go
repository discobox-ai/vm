package boxd

import (
	"context"
	"errors"
	"runtime"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/discobox-ai/vm/pkg/engine"
	"github.com/discobox-ai/vm/pkg/machine"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

// TestStartAsksStartVmFirst starts a machine from each state boxd can leave
// it in: StartVm is the first call, with nothing read before it, and the
// wait boots whatever StartVm leaves: a stop still settling is waited out
// and started, a hibernated machine woken, a suspended one resumed by
// StartVm itself, as boxd does. Boot refuses a suspended or hibernated
// machine with machine.ErrPaused, and leaves it so: Start alone resumes it.
func TestStartAsksStartVmFirst(t *testing.T) {
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
			if from == "hibernated" || from == "suspended" {
				if _, err := d.Boot(ctx, inst, machine.BootOptions{}); !errors.Is(err, machine.ErrPaused) {
					t.Fatalf("boot from %s: %v, want ErrPaused", from, err)
				}
				fake.mu.Lock()
				state := v.status
				fake.mu.Unlock()
				if state != from {
					t.Fatalf("a boot refused left a machine %s %s", from, state)
				}
			}
			fake.takeCalls()
			m, err := d.Start(ctx, inst, machine.BootOptions{})
			if err != nil {
				t.Fatalf("start from %s: %v", from, err)
			}
			t.Cleanup(func() { _ = m.Kill(context.Background()) })
			calls := fake.takeCalls()
			if len(calls) == 0 || calls[0] != "StartVm" {
				t.Fatalf("start from %s called %v; want StartVm first", from, calls)
			}
			fake.mu.Lock()
			state := v.status
			fake.mu.Unlock()
			if state != "running" {
				t.Fatalf("start from %s left the machine %s (calls %v)", from, state, calls)
			}
		})
	}
}

// TestStartWaitsOutARacedResume starts a suspended machine whose resume, by
// StartVm, shows only after the wait has read it suspended: the wait's own
// ResumeVm is refused "VM is starting", as boxd refused it (region fra,
// 2026-11-22), and Start waits until the machine runs, though it reads
// suspended for several polls. A machine that still reads suspended once its
// resume has been refused for raceGrace fails Start, rather than waiting out
// the wait's limit.
func TestStartWaitsOutARacedResume(t *testing.T) {
	defer func(every, grace time.Duration) { waitEvery, raceGrace = every, grace }(waitEvery, raceGrace)
	waitEvery, raceGrace = 20*time.Millisecond, 500*time.Millisecond
	for _, tc := range []struct {
		name string
		lag  int
		want string
	}{
		{name: "raced", lag: 3},
		{name: "refused past the grace", lag: 1000, want: "VM is starting (must be suspended to resume)"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			d, fake := fakeDriver(t, t.TempDir())
			ctx := context.Background()
			inst, err := d.Create(ctx, machine.InstanceSpec{Name: "race"})
			if err != nil {
				t.Fatal(err)
			}
			fake.mu.Lock()
			v := fake.vms[inst.ID]
			v.status = "suspended"
			fake.resumeLag, fake.startingReads = tc.lag, 100
			fake.mu.Unlock()
			if tc.want == "" {
				settle := 10 * waitEvery
				go func() {
					// The resume settles once ResumeVm has been refused.
					time.Sleep(settle)
					fake.mu.Lock()
					v.starting = 0
					fake.mu.Unlock()
				}()
			}
			fake.takeCalls()
			m, err := d.Start(ctx, inst, machine.BootOptions{})
			calls := fake.takeCalls()
			if tc.want != "" {
				if err == nil || !strings.Contains(err.Error(), tc.want) {
					t.Fatalf("start: %v (calls %v); want %q", err, calls, tc.want)
				}
				if n := countCalls(calls, "ResumeVm"); n < 2 {
					t.Fatalf("calls %v; want the refused resume asked again", calls)
				}
				return
			}
			if err != nil {
				t.Fatalf("start: %v (calls %v)", err, calls)
			}
			t.Cleanup(func() { _ = m.Kill(context.Background()) })
			if n := countCalls(calls, "ResumeVm"); n != 3 {
				t.Fatalf("calls %v; want a refused resume for each suspended read", calls)
			}
			fake.mu.Lock()
			state := v.status
			fake.mu.Unlock()
			if state != "running" {
				t.Fatalf("start left the machine %s (calls %v)", state, calls)
			}
		})
	}
}

func countCalls(calls []string, method string) int {
	n := 0
	for _, c := range calls {
		if c == method {
			n++
		}
	}
	return n
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

// TestBootFailsOnARefusedResizeStop boots a machine an instance asks
// another size of, and boxd refuses the resize's StopVm FailedPrecondition:
// that is no race with boxd's own boot, and the boot fails at once with
// boxd's reason rather than waiting for a stop that never comes.
func TestBootFailsOnARefusedResizeStop(t *testing.T) {
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
	fake.mu.Lock()
	fake.stopErr = status.Error(codes.FailedPrecondition, "VM is starting")
	fake.mu.Unlock()
	ctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	if _, err := d.Boot(ctx, inst, machine.BootOptions{}); err == nil || !strings.Contains(err.Error(), "VM is starting") || errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("boot: %v; want the refused stop", err)
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

// A fresh create asked to start leaves the machine CreateVm booted running,
// with no stop; one restored from a committed VM is stopped, so it boots
// cold, and then started; a window is refused before anything is made (ADR
// 26-11-23-458 §2).
func TestCreateStarted(t *testing.T) {
	defer func(every time.Duration) { waitEvery = every }(waitEvery)
	waitEvery = 20 * time.Millisecond
	d, fake := fakeDriver(t, t.TempDir())
	ctx := context.Background()
	status := func(id string) string {
		fake.mu.Lock()
		defer fake.mu.Unlock()
		return fake.vms[id].status
	}

	if _, err := d.Create(ctx, machine.InstanceSpec{Name: "window", Start: &machine.BootOptions{GUI: true}}); !errors.Is(err, machine.ErrUnsupported) {
		t.Fatalf("a create started with a window: %v, want ErrUnsupported", err)
	}
	if _, err := d.Instance(ctx, "window"); !errors.Is(err, machine.ErrNotFound) {
		t.Fatalf("a refused create made %v", err)
	}

	fake.takeCalls()
	fresh, err := d.Create(ctx, machine.InstanceSpec{Name: "fresh", Start: &machine.BootOptions{}})
	if err != nil {
		t.Fatal(err)
	}
	if calls := fake.takeCalls(); slices.Contains(calls, "StopVm") || slices.Contains(calls, "StartVm") {
		t.Fatalf("a fresh create started called %v; want no stop and no start", calls)
	}
	if got := status(fresh.ID); got != "running" {
		t.Fatalf("a fresh create started left its machine %s", got)
	}

	source, err := d.Create(ctx, machine.InstanceSpec{Name: "source"})
	if err != nil {
		t.Fatal(err)
	}
	committed, err := d.Commit(ctx, source)
	if err != nil {
		t.Fatal(err)
	}
	fake.takeCalls()
	copied, err := d.Create(ctx, machine.InstanceSpec{Name: "copied", From: &committed, Start: &machine.BootOptions{}})
	if err != nil {
		t.Fatal(err)
	}
	calls := fake.takeCalls()
	stop, start := slices.Index(calls, "StopVm"), slices.Index(calls, "StartVm")
	if stop < 0 || start < stop {
		t.Fatalf("a copy created started called %v; want StopVm, so it boots cold, then StartVm", calls)
	}
	if got := status(copied.ID); got != "running" {
		t.Fatalf("a copy created started left its machine %s", got)
	}
}
