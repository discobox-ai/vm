package local_test

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/discobox-ai/vm/pkg/machine"
	"github.com/discobox-ai/vm/pkg/machine/fake"
	"github.com/discobox-ai/vm/pkg/machine/local"
)

// capped is a root whose fake hypervisor caps running guests of the host's
// OS at limit, and a driver of it per pool.
type capped struct {
	t     *testing.T
	root  string
	agent string
	limit int
}

func (c capped) driver(pool string) *local.Driver {
	c.t.Helper()
	hv := &fake.Hypervisor{Agent: c.agent, BaseName: "stock", MaxRunning: map[machine.OS]int{machine.OS(runtime.GOOS): c.limit}}
	d, err := local.New(machine.Config{
		Dir: c.root, Pool: pool,
		ShimCommand:  []string{c.agent, "--root", c.root, "--driver", "fake", "--pool", pool, "--driver-config", c.config(), "shim"},
		DriverConfig: json.RawMessage(c.config()),
	}, hv)
	if err != nil {
		c.t.Fatal(err)
	}
	return d
}

// config is the fake driver's configuration capped at the limit, which a
// shim is given as its starter is (ADR 26-11-11-215 §6).
func (c capped) config() string { return fmt.Sprintf(`{"maxRunning":%d}`, c.limit) }

func (c capped) create(d *local.Driver, name string) machine.Instance {
	c.t.Helper()
	inst, err := d.Create(context.Background(), machine.InstanceSpec{Name: name})
	if err != nil {
		c.t.Fatal(err)
	}
	c.t.Cleanup(func() { _ = d.Destroy(context.Background(), inst) })
	return inst
}

func kill(t *testing.T, m machine.Machine) {
	t.Helper()
	if err := m.Kill(context.Background()); err != nil {
		t.Fatal(err)
	}
	select {
	case <-m.Done():
	case <-time.After(30 * time.Second):
		t.Fatal("a killed machine is not done")
	}
}

// Boots in different pools of one root, at once, are admitted no further
// than the hypervisor's cap for their OS; a VM booted twice at once boots
// once and holds one slot; a guest that stops gives its slot back by the
// time its machine is done, as a boot that fails or is canceled does; and a
// cap on another OS caps nothing here.
func TestCapacityIsTheRootsAcrossPools(t *testing.T) {
	ctx := context.Background()
	c := capped{t: t, root: t.TempDir(), agent: buildAgent(t), limit: 2}
	var insts []machine.Instance
	var drivers []*local.Driver
	for i := range 3 {
		d := c.driver("pool" + strconv.Itoa(i))
		drivers = append(drivers, d)
		insts = append(insts, c.create(d, "vm"))
	}

	machines := make([]machine.Machine, 3)
	errs := make([]error, 3)
	var wg sync.WaitGroup
	for i := range 3 {
		wg.Go(func() { machines[i], errs[i] = drivers[i].Boot(ctx, insts[i], machine.BootOptions{}) })
	}
	wg.Wait()
	refused := -1
	for i, err := range errs {
		switch {
		case err == nil:
			t.Cleanup(func() { _ = machines[i].Kill(context.Background()) })
		case errors.Is(err, machine.ErrCapacity) && refused < 0:
			refused = i
		default:
			t.Fatalf("boot in pool %d: %v", i, err)
		}
	}
	if refused < 0 {
		t.Fatal("three guests booted at once under a cap of two")
	}
	if _, err := drivers[refused].Start(ctx, insts[refused], machine.BootOptions{}); !errors.Is(err, machine.ErrCapacity) {
		t.Fatalf("a start on a full host: %v, want ErrCapacity", err)
	}

	// An install runs a guest, so it is refused on a full host too.
	if _, err := drivers[refused].Install(ctx, machine.InstallSpec{GuestOS: machine.OS(runtime.GOOS), Media: "none"}, "installed"); !errors.Is(err, machine.ErrCapacity) {
		t.Fatalf("an install on a full host: %v, want ErrCapacity", err)
	}

	// Killing one makes room by the time Kill returns.
	running := (refused + 1) % 3
	if err := machines[running].Kill(ctx); err != nil {
		t.Fatal(err)
	}
	m, err := drivers[refused].Boot(ctx, insts[refused], machine.BootOptions{})
	if err != nil {
		t.Fatalf("a boot once a guest stopped: %v", err)
	}
	machines[refused] = m
	t.Cleanup(func() { _ = m.Kill(context.Background()) })

	// A running VM booted again is refused as running, not counted again.
	if _, err := drivers[refused].Boot(ctx, insts[refused], machine.BootOptions{}); err == nil || errors.Is(err, machine.ErrCapacity) || !strings.Contains(err.Error(), "already running") {
		t.Fatalf("a running VM booted again: %v", err)
	}
	for i, m := range machines {
		if i != running {
			kill(t, m)
		}
	}

	// The same VM booted twice at once boots once, and leaves the other
	// slot for another VM.
	same := make([]machine.Machine, 2)
	sameErrs := make([]error, 2)
	for i := range 2 {
		wg.Go(func() { same[i], sameErrs[i] = drivers[0].Boot(ctx, insts[0], machine.BootOptions{}) })
	}
	wg.Wait()
	if (sameErrs[0] == nil) == (sameErrs[1] == nil) {
		t.Fatalf("one VM booted twice at once: %v, %v", sameErrs[0], sameErrs[1])
	}
	for i, err := range sameErrs {
		if err == nil {
			defer kill(t, same[i])
		} else if !strings.Contains(err.Error(), "already running") {
			t.Fatalf("the second boot of one VM: %v", err)
		}
	}
	other, err := drivers[1].Boot(ctx, insts[1], machine.BootOptions{})
	if err != nil {
		t.Fatalf("a second VM beside one booted twice: %v", err)
	}
	kill(t, other)

	// A boot that fails, and one canceled, give their slots back.
	broken, err := local.New(machine.Config{Dir: c.root, Pool: "pool2"}, &fake.Hypervisor{Agent: filepath.Join(c.root, "no-agent"), BaseName: "stock", MaxRunning: map[machine.OS]int{machine.OS(runtime.GOOS): c.limit}})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := broken.Boot(ctx, insts[2], machine.BootOptions{}); err == nil {
		t.Fatal("a guest with no agent booted")
	}
	canceled, cancel := context.WithCancel(ctx)
	cancel()
	if _, err := drivers[1].Boot(canceled, insts[1], machine.BootOptions{}); err == nil {
		t.Fatal("a canceled boot booted")
	}
	m, err = drivers[2].Boot(ctx, insts[2], machine.BootOptions{})
	if err != nil {
		t.Fatalf("a boot after a failed one and a canceled one: %v", err)
	}
	kill(t, m)

	// A cap on another OS caps nothing of this one.
	uncapped := capped{t: t, root: t.TempDir(), agent: c.agent}
	hv := &fake.Hypervisor{Agent: c.agent, BaseName: "stock", MaxRunning: map[machine.OS]int{"plan9": 1}}
	d, err := local.New(machine.Config{Dir: uncapped.root}, hv)
	if err != nil {
		t.Fatal(err)
	}
	for i := range 3 {
		m, err := d.Boot(ctx, uncapped.create(d, "free"+strconv.Itoa(i)), machine.BootOptions{})
		if err != nil {
			t.Fatalf("an uncapped boot: %v", err)
		}
		defer kill(t, m)
	}
}

// A shim is a process of its own, and holds its guest's slot for as long as
// it runs: a boot in this process, in another pool, is refused while it
// does, and so is a shim, in any pool, that a start's own check let through;
// once the shim stops, the slot is free.
func TestCapacityAcrossProcesses(t *testing.T) {
	ctx := context.Background()
	c := capped{t: t, root: t.TempDir(), agent: buildAgent(t), limit: 1}
	a, b := c.driver("a"), c.driver("b")
	one, two := c.create(a, "vm"), c.create(b, "vm")

	startCtx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	m, err := a.Start(startCtx, one, machine.BootOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := b.Boot(ctx, two, machine.BootOptions{}); !errors.Is(err, machine.ErrCapacity) {
		t.Fatalf("a boot beside another process's guest, at a cap of one: %v, want ErrCapacity", err)
	}
	if _, err := b.Start(startCtx, two, machine.BootOptions{}); !errors.Is(err, machine.ErrCapacity) {
		t.Fatalf("a start beside another process's guest: %v, want ErrCapacity", err)
	}
	if err := local.ShimMain(ctx, b, []string{two.ID}); !errors.Is(err, machine.ErrCapacity) {
		t.Fatalf("a shim on a full host: %v, want ErrCapacity", err)
	}
	// A start whose own check let it through, as one racing another boot
	// would be, is refused by its shim, and says so with ErrCapacity: here
	// this process's driver caps nothing, and the shim's caps at one.
	loose, err := local.New(machine.Config{
		Dir: c.root, Pool: "b",
		ShimCommand:  []string{c.agent, "--root", c.root, "--driver", "fake", "--pool", "b", "--driver-config", c.config(), "shim"},
		DriverConfig: json.RawMessage(c.config()),
	}, &fake.Hypervisor{Agent: c.agent, BaseName: "stock"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := loose.Start(startCtx, two, machine.BootOptions{}); !errors.Is(err, machine.ErrCapacity) {
		t.Fatalf("a start its shim refused for capacity: %v, want ErrCapacity", err)
	}
	if err := m.Shutdown(ctx, false); err != nil {
		t.Fatal(err)
	}
	select {
	case <-m.Done():
	case <-time.After(30 * time.Second):
		t.Fatal("the shim did not exit")
	}
	m, err = b.Start(startCtx, two, machine.BootOptions{})
	if err != nil {
		t.Fatalf("a start once the other shim exited: %v", err)
	}
	kill(t, m)
}
