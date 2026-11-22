package fake_test

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"runtime"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/discobox-ai/vm/pkg/machine"
	"github.com/discobox-ai/vm/pkg/machine/fake"
	"github.com/discobox-ai/vm/pkg/machine/local"
	"github.com/discobox-ai/vm/pkg/machine/machinetest"
)

// failingDestroy refuses the first removal of each VM that exists, and the
// first deletion of each cache that does, that the suite makes from a
// test's cleanup, as hcs refuses a removal while a shim still holds an
// instance's files open (vm#166); removals the suite checks itself go
// through.
type failingDestroy struct {
	machine.Driver
	mu      *sync.Mutex
	refused map[string]bool
}

var errHeldOpen = errors.New("the process cannot access the file because it is being used by another process")

// refuse reports whether to refuse this call of what: the first of it from
// a cleanup.
func (d *failingDestroy) refuse(what string) bool {
	pcs := make([]uintptr, 64)
	frames := runtime.CallersFrames(pcs[:runtime.Callers(3, pcs)])
	for {
		f, more := frames.Next()
		if strings.Contains(f.Function, "testing.(*common).runCleanup") {
			d.mu.Lock()
			defer d.mu.Unlock()
			first := !d.refused[what]
			d.refused[what] = true
			return first
		}
		if !more {
			return false
		}
	}
}

func (d *failingDestroy) Destroy(ctx context.Context, inst machine.Instance) error {
	if _, err := d.Instance(ctx, inst.Name); err == nil && d.refuse("vm "+inst.Pool+"/"+inst.Name) {
		return errHeldOpen
	}
	return d.Driver.Destroy(ctx, inst)
}

func (d *failingDestroy) DeleteCache(ctx context.Context, key string) error {
	caches, err := d.Caches(ctx)
	if err == nil && slices.ContainsFunc(caches, func(c machine.Cache) bool { return c.Key == key }) && d.refuse("cache "+key) {
		return errHeldOpen
	}
	return d.Driver.DeleteCache(ctx, key)
}

// TestConformanceFailsWhenItLeavesAVMBehind runs the suite, in a process of
// its own since it must fail, on a driver whose removals from cleanups fail
// once: the suite fails naming each VM it left, and removes them all before
// it ends.
func TestConformanceFailsWhenItLeavesAVMBehind(t *testing.T) {
	if dir := os.Getenv("DISCO_VM_LEAK_DIR"); dir != "" {
		leakingSuite(t, dir, os.Getenv("DISCO_VM_TEST_BINARY"))
		return
	}
	agent := os.Getenv("DISCO_VM_TEST_BINARY")
	if agent == "" {
		agent = filepath.Join(t.TempDir(), "disco-vm")
		if runtime.GOOS == "windows" {
			agent += ".exe"
		}
		build := exec.CommandContext(t.Context(), "go", "build", "-o", agent, "../../../cmd/disco-vm")
		build.Stdout, build.Stderr = os.Stdout, os.Stderr
		if err := build.Run(); err != nil {
			t.Fatal(err)
		}
	}
	dir := t.TempDir()
	child := exec.CommandContext(t.Context(), os.Args[0], "-test.run=^TestConformanceFailsWhenItLeavesAVMBehind$", "-test.count=1") //nolint:gosec // G204: this test binary
	child.Env = append(os.Environ(), "DISCO_VM_LEAK_DIR="+dir, "DISCO_VM_TEST_BINARY="+agent)
	out, err := child.CombinedOutput()
	if _, failed := errors.AsType[*exec.ExitError](err); !failed {
		t.Fatalf("the suite on a driver whose cleanups fail to remove: %v, want it to fail\n%s", err, out)
	}
	// Every VM whose removal failed is named again at the end, as left
	// behind in its pool.
	removed := regexp.MustCompile(`remove (\S+) of pool (\S+): `).FindAllStringSubmatch(string(out), -1)
	if len(removed) == 0 {
		t.Fatalf("the failed suite names no removal that failed:\n%s", out)
	}
	for _, m := range removed {
		if want := "the suite left " + m[1] + " behind in pool " + m[2]; !strings.Contains(string(out), want) {
			t.Errorf("the failed suite does not say %q:\n%s", want, out)
		}
	}
	if !strings.Contains(string(out), "the suite left cache conformance-") {
		t.Errorf("the failed suite names no cache it left behind:\n%s", out)
	}
	// Then the suite removed it all: both pools and the caches are empty.
	d := newLeakDriver(t, dir, agent, nil)
	pools := []machine.Driver{d}
	for _, m := range removed {
		if m[2] != "default" {
			other, err := d.InPool(m[2])
			if err != nil {
				t.Fatal(err)
			}
			pools = append(pools, other)
			break
		}
	}
	if len(pools) == 1 {
		t.Errorf("the failed suite names no removal in another pool:\n%s", out)
	}
	for _, p := range pools {
		left, err := p.Instances(context.Background())
		if err != nil {
			t.Fatal(err)
		}
		for _, inst := range left {
			t.Errorf("the suite left %s in pool %s once it ended", inst.Name, inst.Pool)
		}
	}
	caches, err := d.Caches(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	for _, c := range caches {
		t.Errorf("the suite left cache %s once it ended", c.Key)
	}
}

// leakingSuite is the suite, run in the child process.
func leakingSuite(t *testing.T, dir, agent string) {
	t.Helper()
	var mu sync.Mutex
	refused := map[string]bool{}
	failing := func(d machine.Driver) machine.Driver {
		return &failingDestroy{Driver: d, mu: &mu, refused: refused}
	}
	machinetest.Run(t, failing(newLeakDriver(t, dir, agent, nil)), machinetest.Config{
		Timeout: 30 * time.Second,
		// A fake guest is a host process, on the host's clock.
		PauseHold:  2 * time.Second,
		ScratchDir: "/scratch",
		GuestAgent: agent,
		Keyed: func(keys machine.KeySource) machine.Driver {
			return failing(newLeakDriver(t, dir, agent, keys))
		},
		Pool: func(pool string) machine.Driver {
			d, err := newLeakDriver(t, dir, agent, nil).InPool(pool)
			if err != nil {
				t.Fatal(err)
			}
			return failing(d)
		},
	})
}

func newLeakDriver(t *testing.T, dir, agent string, keys machine.KeySource) *local.Driver {
	t.Helper()
	d, err := local.New(machine.Config{
		Dir:         dir,
		ShimCommand: []string{agent, "--root", dir, "--driver", "fake", "--driver-config", "{}", "shim"},
		Keys:        keys,
	}, &fake.Hypervisor{Agent: agent, BaseName: "stock"})
	if err != nil {
		t.Fatal(err)
	}
	return d
}
