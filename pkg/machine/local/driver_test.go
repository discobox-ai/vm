package local_test

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/discobox-ai/vm/internal/fsutil"
	"github.com/discobox-ai/vm/pkg/machine"
	"github.com/discobox-ai/vm/pkg/machine/fake"
	"github.com/discobox-ai/vm/pkg/machine/local"
)

// chained is the fake hypervisor as one whose disks chain, as hcs's do,
// recording the chain each Prepare was given.
type chained struct {
	*fake.Hypervisor
	prepared [][]string
}

func (*chained) Chains() bool { return true }

func (c *chained) Prepare(ctx context.Context, inst local.InstanceSpec) error {
	var ids []string
	for _, l := range inst.Chain {
		ids = append(ids, l.ID)
	}
	c.prepared = append(c.prepared, ids)
	return c.Hypervisor.Prepare(ctx, inst)
}

// A VM created from a committed VM created from an install, on a hypervisor
// that chains, is prepared over the whole chain, and each VM in it is
// refused removal and boot while a VM's disk differences over its own; a VM
// whose chain was lost behind the driver's back does not boot, and is still
// removed. On a hypervisor that does not chain, a VM created from another
// outlives it.
func TestChainsKeepADiskAnotherNeeds(t *testing.T) {
	ctx := context.Background()
	guestOS := machine.OS(runtime.GOOS)
	root := t.TempDir()
	hv := &chained{Hypervisor: &fake.Hypervisor{}}
	d, err := local.New(machine.Config{Dir: root}, hv)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := d.Create(ctx, machine.InstanceSpec{Name: "fresh"}); !errors.Is(err, machine.ErrUnsupported) {
		t.Fatalf("a fresh VM with no base: %v, want ErrUnsupported", err)
	}
	copyOf := func(from machine.Instance, name string) machine.Instance {
		t.Helper()
		inst, err := d.Create(ctx, machine.InstanceSpec{Name: name, From: &from})
		if err != nil {
			t.Fatal(err)
		}
		if inst.From != from.Name || inst.GuestOS != from.GuestOS {
			t.Fatalf("created %+v from %s", inst, from.Name)
		}
		return inst
	}
	commit := func(inst machine.Instance) machine.Instance {
		t.Helper()
		committed, err := d.Commit(ctx, inst)
		if err != nil {
			t.Fatal(err)
		}
		return committed
	}
	base, err := d.Install(ctx, machine.InstallSpec{GuestOS: guestOS, Media: "latest"}, "base")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := d.Install(ctx, machine.InstallSpec{GuestOS: guestOS, Media: "latest"}, "base"); !errors.Is(err, machine.ErrExists) {
		t.Fatalf("a second install named base: %v, want ErrExists", err)
	}
	first := commit(copyOf(base, "one"))
	second := commit(copyOf(first, "two"))
	third := copyOf(second, "three")
	if got := hv.prepared[len(hv.prepared)-1]; len(got) != 3 || got[0] != base.ID || got[1] != first.ID || got[2] != second.ID {
		t.Fatalf("prepared over %v, want the whole chain, base first", got)
	}

	for _, inst := range []machine.Instance{base, first, second} {
		if err := d.Destroy(ctx, inst); !errors.Is(err, machine.ErrInUse) {
			t.Fatalf("remove %s, which another VM's disk needs: %v, want ErrInUse", inst.Name, err)
		}
		if _, err := d.Boot(ctx, inst, machine.BootOptions{}); !errors.Is(err, machine.ErrInUse) {
			t.Fatalf("boot %s, which another VM's disk needs: %v, want ErrInUse", inst.Name, err)
		}
	}
	// Another pool shares the directory and sees none of it.
	other, err := local.New(machine.Config{Dir: root, Pool: "other"}, hv)
	if err != nil {
		t.Fatal(err)
	}
	if list, err := other.Instances(ctx); err != nil || len(list) != 0 {
		t.Fatalf("another pool's VMs = %+v, %v", list, err)
	}

	// A VM of the chain removed behind the driver's back leaves the VMs
	// over it unbootable, and still removed.
	if err := os.RemoveAll(filepath.Join(root, "instances", first.ID)); err != nil {
		t.Fatal(err)
	}
	if _, err := d.Boot(ctx, third, machine.BootOptions{}); !errors.Is(err, machine.ErrNotFound) {
		t.Fatalf("boot a VM whose chain is gone: %v, want ErrNotFound", err)
	}
	// A VM being created from base, its disk made and its record not yet
	// written, holds base too.
	making := filepath.Join(root, "instances", "making")
	if err := os.MkdirAll(making, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(making, "from"), []byte(base.ID), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := d.Destroy(ctx, base); !errors.Is(err, machine.ErrInUse) {
		t.Fatalf("remove %s while a VM is being created from it: %v, want ErrInUse", base.Name, err)
	}
	// One a crashed Create left, past the stale-claim window, holds
	// nothing; the directory stays for the owner to remove.
	old := time.Now().Add(-time.Hour)
	if err := os.Chtimes(filepath.Join(making, "from"), old, old); err != nil {
		t.Fatal(err)
	}
	if _, err := d.Boot(ctx, base, machine.BootOptions{}); errors.Is(err, machine.ErrInUse) {
		t.Fatalf("boot %s with only a crashed create's leftover naming it: %v", base.Name, err)
	}
	if err := os.RemoveAll(making); err != nil {
		t.Fatal(err)
	}
	for _, inst := range []machine.Instance{third, second, base} {
		if err := d.Destroy(ctx, inst); err != nil {
			t.Fatalf("remove %s: %v", inst.Name, err)
		}
	}
	if list, err := d.Instances(ctx); err != nil || len(list) != 0 {
		t.Fatalf("VMs left = %+v, %v", list, err)
	}

	// The fake hypervisor's own copies are trees of their own.
	plain, err := local.New(machine.Config{Dir: t.TempDir()}, &fake.Hypervisor{})
	if err != nil {
		t.Fatal(err)
	}
	b, err := plain.Install(ctx, machine.InstallSpec{GuestOS: guestOS, Media: "latest"}, "b")
	if err != nil {
		t.Fatal(err)
	}
	inst, err := plain.Create(ctx, machine.InstanceSpec{Name: "four", From: &b})
	if err != nil {
		t.Fatal(err)
	}
	four, err := plain.Commit(ctx, inst)
	if err != nil {
		t.Fatal(err)
	}
	if err := plain.Destroy(ctx, b); err != nil {
		t.Fatalf("remove a VM another was created from, on a hypervisor that does not chain: %v", err)
	}
	if _, err := plain.Create(ctx, machine.InstanceSpec{Name: "five", From: &four}); err != nil {
		t.Fatalf("create from a committed VM whose own start is gone: %v", err)
	}
}

// An instance recorded before ADR 26-11-03-606, of an image or a base,
// or before the pool cutover with the old project field, is not read: not listed, not found, and so never created from or rewritten.
func TestRecordsFromBeforeAreNotRead(t *testing.T) {
	ctx := context.Background()
	root := t.TempDir()
	d, err := local.New(machine.Config{Dir: root}, &fake.Hypervisor{BaseName: "b"})
	if err != nil {
		t.Fatal(err)
	}
	guestOS := string(machine.OS(runtime.GOOS))
	for id, record := range map[string]string{
		"0000000000d4": `{"id":"0000000000d4","name":"old-scope","guestOS":"` + guestOS + `","project":"default"}`,
		"0000000000a1": `{"id":"0000000000a1","name":"of-image","image":"app","imageID":"aaaa","guestOS":"` + guestOS + `","pool":"default"}`,
		"0000000000b2": `{"id":"0000000000b2","name":"of-base","image":"stock","base":{"name":"stock"},"guestOS":"` + guestOS + `","pool":"default"}`,
		"0000000000c3": `{"id":"0000000000c3","name":"of-layer","layer":"aaaa","guestOS":"` + guestOS + `","pool":"default"}`,
	} {
		dir := filepath.Join(root, "instances", id)
		if err := os.MkdirAll(dir, 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(dir, "instance.json"), []byte(record), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	if list, err := d.Instances(ctx); err != nil || len(list) != 0 {
		t.Fatalf("instances = %+v, %v; want none from before", list, err)
	}
	fresh, err := d.Create(ctx, machine.InstanceSpec{Name: "fresh"})
	if err != nil {
		t.Fatal(err)
	}
	if list, err := d.Instances(ctx); err != nil || len(list) != 1 || list[0].ID != fresh.ID {
		t.Fatalf("instances = %+v, %v; want the new VM alone", list, err)
	}
}

// viewing is the fake hypervisor as one that opens windows, as hcs does: its
// installs hold until released, and ViewInstall records the install it was
// given.
type viewing struct {
	*fake.Hypervisor
	started chan string
	release chan struct{}
	viewed  chan local.Layer
}

func (v *viewing) Install(ctx context.Context, spec local.InstallSpec, dst local.Layer) (*machine.Release, error) {
	v.started <- dst.ID
	<-v.release
	return v.Hypervisor.Install(ctx, spec, dst)
}

func (v *viewing) Capabilities() machine.Capabilities {
	caps := v.Hypervisor.Capabilities()
	caps.Display, caps.View = true, true
	return caps
}

func (*viewing) View(context.Context, machine.Machine, func(context.Context) (net.Conn, error), string) error {
	return nil
}

func (v *viewing) ViewInstall(_ context.Context, dst local.Layer, _ string, _ io.Writer) error {
	v.viewed <- dst
	return nil
}

// A window on an install finds it while it runs, by its VM's name or as the
// only one, and finds nothing once the install has made its VM.
func TestViewInstallFindsAnInstallInProgress(t *testing.T) {
	ctx := context.Background()
	guestOS := machine.OS(runtime.GOOS)
	hv := &viewing{Hypervisor: &fake.Hypervisor{}, started: make(chan string, 2), release: make(chan struct{}), viewed: make(chan local.Layer, 1)}
	d, err := local.New(machine.Config{Dir: t.TempDir()}, hv)
	if err != nil {
		t.Fatal(err)
	}
	names := []string{"one", "two"}
	installed := make(chan machine.Instance, len(names))
	failed := make(chan error, len(names))
	for _, name := range names {
		go func() {
			inst, err := d.Install(ctx, machine.InstallSpec{GuestOS: guestOS, Media: "none"}, name)
			if err != nil {
				failed <- err
				return
			}
			installed <- inst
		}()
	}
	<-hv.started
	<-hv.started

	if err := d.ViewInstall(ctx, "", "t", io.Discard); err == nil || !strings.Contains(err.Error(), "name one") {
		t.Fatalf("view the only install of two: %v", err)
	}
	if err := d.ViewInstall(ctx, "three", "t", io.Discard); !errors.Is(err, machine.ErrNotFound) {
		t.Fatalf("view an install of no VM: %v", err)
	}
	if err := d.ViewInstall(ctx, "one", "t", io.Discard); err != nil {
		t.Fatal(err)
	}
	viewed := <-hv.viewed

	close(hv.release)
	for range names {
		select {
		case inst := <-installed:
			if inst.Name == "one" && (viewed.ID != inst.ID || filepath.Base(viewed.Dir) != "machine") {
				t.Fatalf("viewed %+v, want VM %s's machine", viewed, inst.ID)
			}
		case err := <-failed:
			t.Fatal(err)
		}
	}
	if err := d.ViewInstall(ctx, "", "t", io.Discard); !errors.Is(err, machine.ErrNotFound) {
		t.Fatalf("view an install that has finished: %v", err)
	}
}

// Create refuses a malformed forward itself, as the engine does before it
// asks, since a served driver's server calls it with what a client sent;
// nothing is made.
func TestCreateRefusesMalformedForwards(t *testing.T) {
	ctx := context.Background()
	d, err := local.New(machine.Config{Dir: t.TempDir()}, &fake.Hypervisor{BaseName: "stock"})
	if err != nil {
		t.Fatal(err)
	}
	sock := filepath.Join(t.TempDir(), "host.sock")
	for name, forwards := range map[string][]machine.Forward{
		"no port":                  {{Instance: "peer", InstancePort: 7300}},
		"a socket and an instance": {{Port: 9000, Socket: sock, Instance: "peer", InstancePort: 7300}},
		"not an instance name":     {{Port: 9000, Instance: "Peer!", InstancePort: 7300}},
		"itself":                   {{Port: 9000, Instance: "vm", InstancePort: 7300}},
		"a peer's port 22":         {{Port: 9000, Instance: "peer", InstancePort: 22}},
		"a port of no instance":    {{Port: 9000, Socket: sock, InstancePort: 7300}},
		"a relative socket":        {{Port: 9000, Socket: "host.sock"}},
		"no target":                {{Port: 9000}},
		"a port twice":             {{Port: 9000, Socket: sock}, {Port: 9000, Instance: "peer", InstancePort: 7300}},
	} {
		if _, err := d.Create(ctx, machine.InstanceSpec{Name: "vm", Forwards: forwards}); err == nil {
			t.Fatalf("a forward with %s was taken", name)
		}
	}
	if all, err := d.Instances(ctx); err != nil || len(all) != 0 {
		t.Fatalf("after the refusals: %v, %v, want nothing made", all, err)
	}
	inst, err := d.Create(ctx, machine.InstanceSpec{Name: "vm", Forwards: []machine.Forward{
		{Port: 9000, Socket: sock}, {Port: 9001, Instance: "peer", InstancePort: 7302},
	}})
	if err != nil {
		t.Fatal(err)
	}
	_ = d.Destroy(ctx, inst)
}

// A record a crashed install left (installing.json, its process gone) is no
// install in progress, and a hypervisor without View opens no window on one.
func TestViewInstallSkipsACrashedInstall(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	hv := &viewing{Hypervisor: &fake.Hypervisor{}, viewed: make(chan local.Layer, 1)}
	d, err := local.New(machine.Config{Dir: dir}, hv)
	if err != nil {
		t.Fatal(err)
	}
	gone := exec.CommandContext(ctx, "go", "version")
	if err := gone.Run(); err != nil {
		t.Fatal(err)
	}
	for _, rec := range []struct {
		id  string
		pid int
	}{{"crashed0001", gone.Process.Pid}, {"oldrecord01", 0}} {
		path := filepath.Join(dir, "instances", rec.id, "installing.json")
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			t.Fatal(err)
		}
		data := fmt.Sprintf(`{"id":%q,"name":"crashed","guestOS":%q,"pool":"default","pid":%d}`, rec.id, runtime.GOOS, rec.pid)
		if err := os.WriteFile(path, []byte(data), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	if err := d.ViewInstall(ctx, "crashed", "t", io.Discard); !errors.Is(err, machine.ErrNotFound) {
		t.Fatalf("view a crashed install: %v, want ErrNotFound", err)
	}
	if err := d.ViewInstall(ctx, "", "t", io.Discard); !errors.Is(err, machine.ErrNotFound) {
		t.Fatalf("view the only install, with a crashed one alone: %v, want ErrNotFound", err)
	}

	plain, err := local.New(machine.Config{Dir: t.TempDir()}, &fake.Hypervisor{})
	if err != nil {
		t.Fatal(err)
	}
	if err := plain.ViewInstall(ctx, "", "t", io.Discard); !errors.Is(err, machine.ErrUnsupported) {
		t.Fatalf("view an install without View: %v, want ErrUnsupported", err)
	}
	if err := plain.View(ctx, machine.Instance{Name: "x"}, "t"); !errors.Is(err, machine.ErrUnsupported) {
		t.Fatalf("view an instance without View: %v, want ErrUnsupported", err)
	}
}

// An install killed mid-run leaves its name's claim, its installing.json
// (its process gone) and its disk. It is listed, found by its name, and
// removed like any other VM, its directory and name included.
func TestAnInterruptedInstallIsListedAndRemoved(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	d, err := local.New(machine.Config{Dir: dir}, &fake.Hypervisor{})
	if err != nil {
		t.Fatal(err)
	}
	gone := exec.CommandContext(ctx, "go", "version")
	if err := gone.Run(); err != nil {
		t.Fatal(err)
	}
	const id, name = "killed00001", "killed"
	inst := filepath.Join(dir, "instances", id)
	if err := os.MkdirAll(filepath.Join(inst, "machine"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(inst, "machine", "disk"), []byte("half a disk"), 0o600); err != nil {
		t.Fatal(err)
	}
	data := fmt.Sprintf(`{"id":%q,"name":%q,"guestOS":%q,"pool":"default","pid":%d}`, id, name, runtime.GOOS, gone.Process.Pid)
	if err := os.WriteFile(filepath.Join(inst, "installing.json"), []byte(data), 0o600); err != nil {
		t.Fatal(err)
	}
	names := filepath.Join(dir, "instances", ".names", "default")
	if err := os.MkdirAll(names, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(names, name), []byte(id), 0o600); err != nil {
		t.Fatal(err)
	}

	all, err := d.Instances(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(all) != 1 || all[0].ID != id {
		t.Fatalf("instances = %+v, want the interrupted install %s", all, id)
	}
	found, err := d.Instance(ctx, name)
	if err != nil {
		t.Fatalf("instance %s, an interrupted install: %v", name, err)
	}
	if err := d.Destroy(ctx, found); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(inst); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("the install's directory after Destroy: %v, want it gone", err)
	}
	if _, err := os.Stat(filepath.Join(names, name)); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("the install's name after Destroy: %v, want it released", err)
	}
}

// installing writes what an install leaves in d's root while it runs as
// process pid, its name's claim made long enough ago to be stale, and with
// held, holds its lock until the test ends, as a running install does.
func installing(t *testing.T, dir, id, name string, pid int, held bool) {
	t.Helper()
	inst := filepath.Join(dir, "instances", id)
	if err := os.MkdirAll(filepath.Join(inst, "machine"), 0o755); err != nil {
		t.Fatal(err)
	}
	data := fmt.Sprintf(`{"id":%q,"name":%q,"guestOS":%q,"pool":"default","pid":%d}`, id, name, runtime.GOOS, pid)
	if err := os.WriteFile(filepath.Join(inst, "installing.json"), []byte(data), 0o600); err != nil {
		t.Fatal(err)
	}
	names := filepath.Join(dir, "instances", ".names", "default")
	if err := os.MkdirAll(names, 0o755); err != nil {
		t.Fatal(err)
	}
	claim := filepath.Join(names, name)
	if err := os.WriteFile(claim, []byte(id), 0o600); err != nil {
		t.Fatal(err)
	}
	old := time.Now().Add(-time.Hour)
	if err := os.Chtimes(claim, old, old); err != nil {
		t.Fatal(err)
	}
	if held {
		release, err := fsutil.TryLock(filepath.Join(inst, "install.lock"))
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(release)
	}
}

// An interrupted install is marked so, and is no VM: it is not booted,
// committed, upgraded, or created from, and its name is not taken by
// another install, however old its claim.
func TestAnInterruptedInstallIsRefusedAsAVM(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	d, err := local.New(machine.Config{Dir: dir}, &fake.Hypervisor{})
	if err != nil {
		t.Fatal(err)
	}
	gone := exec.CommandContext(ctx, "go", "version")
	if err := gone.Run(); err != nil {
		t.Fatal(err)
	}
	installing(t, dir, "killed00002", "killed", gone.Process.Pid, false)
	inst, err := d.Instance(ctx, "killed")
	if err != nil {
		t.Fatal(err)
	}
	if !inst.Interrupted || inst.Name != "killed" {
		t.Fatalf("instance = %+v, want the install killed marked interrupted", inst)
	}
	_, bootErr := d.Boot(ctx, inst, machine.BootOptions{})
	_, commitErr := d.Commit(ctx, inst)
	_, upgradeErr := d.Upgrade(ctx, inst, nil)
	_, createErr := d.Create(ctx, machine.InstanceSpec{Name: "copy", From: &inst})
	refusals := map[string]error{"boot": bootErr, "commit": commitErr, "upgrade": upgradeErr, "create from": createErr}
	if _, err := d.Install(ctx, machine.InstallSpec{GuestOS: machine.OS(runtime.GOOS), Media: "x"}, "killed"); !errors.Is(err, machine.ErrExists) {
		t.Errorf("install over the interrupted install's name: %v, want ErrExists", err)
	}
	for what, err := range refusals {
		if err == nil || !strings.Contains(err.Error(), "interrupted") {
			t.Errorf("%s of an interrupted install: %v, want it refused as interrupted", what, err)
		}
	}
	if err := d.Destroy(ctx, inst); err != nil {
		t.Fatal(err)
	}
}

// An install still running is no VM yet: it is not listed or found by its
// name, its name is not taken however old its claim, and it holds no
// committed VM's removal.
func TestAnInstallInProgressKeepsItsName(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	hv := &chained{Hypervisor: &fake.Hypervisor{}}
	d, err := local.New(machine.Config{Dir: dir}, hv)
	if err != nil {
		t.Fatal(err)
	}
	committed, err := d.Install(ctx, machine.InstallSpec{GuestOS: machine.OS(runtime.GOOS), Media: "x"}, "done")
	if err != nil {
		t.Fatal(err)
	}
	installing(t, dir, "running0001", "running", os.Getpid(), true)
	all, err := d.Instances(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(all) != 1 || all[0].ID != committed.ID {
		t.Fatalf("instances = %+v, want only %s", all, committed.Name)
	}
	if _, err := d.Instance(ctx, "running"); err == nil || errors.Is(err, machine.ErrNotFound) || !strings.Contains(err.Error(), "being installed") {
		t.Fatalf("instance of an install in progress: %v, want it being installed", err)
	}
	if _, err := d.Install(ctx, machine.InstallSpec{GuestOS: machine.OS(runtime.GOOS), Media: "x"}, "running"); !errors.Is(err, machine.ErrExists) {
		t.Fatalf("install over an install in progress: %v, want ErrExists", err)
	}
	if err := d.Destroy(ctx, committed); err != nil {
		t.Fatalf("remove a committed VM while an install runs: %v", err)
	}
}

// A dead install whose process ID a live process took since, as a reboot or
// a busy host makes, holds no lock: it is an interrupted install, listed
// and removed, not one in progress.
func TestAnInterruptedInstallWhosePIDWasReused(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	d, err := local.New(machine.Config{Dir: dir}, &fake.Hypervisor{})
	if err != nil {
		t.Fatal(err)
	}
	installing(t, dir, "reused00001", "reused", os.Getpid(), false)
	inst, err := d.Instance(ctx, "reused")
	if err != nil {
		t.Fatalf("an install whose PID is reused: %v", err)
	}
	if !inst.Interrupted {
		t.Fatalf("instance = %+v, want it interrupted", inst)
	}
	if err := d.Destroy(ctx, inst); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(dir, "instances", "reused00001")); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("its directory after Destroy: %v, want it gone", err)
	}
}

// An install an older disco-vm left, whose name a later VM has taken since
// (that one took a stale claim), is listed under that name beside the VM
// that holds it now. The name finds the one that holds it; the interrupted
// install is removed by its ID, and leaves the other's name alone.
func TestAnInterruptedInstallWhoseNameWasTaken(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	d, err := local.New(machine.Config{Dir: dir}, &fake.Hypervisor{})
	if err != nil {
		t.Fatal(err)
	}
	gone := exec.CommandContext(ctx, "go", "version")
	if err := gone.Run(); err != nil {
		t.Fatal(err)
	}
	installing(t, dir, "orphan00001", "taken", gone.Process.Pid, false)
	// The later VM's claim replaces the orphan's, as the older claim did.
	if err := os.Remove(filepath.Join(dir, "instances", ".names", "default", "taken")); err != nil {
		t.Fatal(err)
	}
	later, err := d.Install(ctx, machine.InstallSpec{GuestOS: machine.OS(runtime.GOOS), Media: "x"}, "taken")
	if err != nil {
		t.Fatal(err)
	}
	all, err := d.Instances(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(all) != 2 {
		t.Fatalf("instances = %+v, want the orphan and %s", all, later.ID)
	}
	byName, err := d.Instance(ctx, "taken")
	if err != nil || byName.ID != later.ID {
		t.Fatalf("instance taken = %+v, %v; want %s, which holds the name", byName, err, later.ID)
	}
	var orphan machine.Instance
	for _, inst := range all {
		if inst.ID == "orphan00001" {
			orphan = inst
		}
	}
	if !orphan.Interrupted {
		t.Fatalf("the orphan %+v is not marked interrupted", orphan)
	}
	if err := d.Destroy(ctx, orphan); err != nil {
		t.Fatal(err)
	}
	if byName, err := d.Instance(ctx, "taken"); err != nil || byName.ID != later.ID {
		t.Fatalf("instance taken after removing the orphan = %+v, %v; want %s still", byName, err, later.ID)
	}
}

// A caller with a key the driver issued for the pool removes its pool's
// interrupted install, which it sees listed, and no other pool's.
func TestAKeyRemovesItsPoolsInterruptedInstall(t *testing.T) {
	ctx := t.Context()
	dir := t.TempDir()
	hv := &fake.Hypervisor{}
	owner, err := local.New(machine.Config{Dir: dir}, hv)
	if err != nil {
		t.Fatal(err)
	}
	key, err := owner.IssueKey(ctx, machine.IssueRequest{Name: "pool", TTL: time.Hour})
	if err != nil {
		t.Fatal(err)
	}
	keyed, err := local.New(machine.Config{Dir: dir, Keys: machine.StaticKey(key)}, hv)
	if err != nil {
		t.Fatal(err)
	}
	gone := exec.CommandContext(ctx, "go", "version")
	if err := gone.Run(); err != nil {
		t.Fatal(err)
	}
	installing(t, dir, "keyed000001", "keyed", gone.Process.Pid, false)
	theirs := filepath.Join(dir, "instances", "theirs00001")
	if err := os.MkdirAll(theirs, 0o755); err != nil {
		t.Fatal(err)
	}
	data := fmt.Sprintf(`{"id":"theirs00001","name":"theirs","guestOS":%q,"pool":"other","pid":%d}`, runtime.GOOS, gone.Process.Pid)
	if err := os.WriteFile(filepath.Join(theirs, "installing.json"), []byte(data), 0o600); err != nil {
		t.Fatal(err)
	}
	all, err := keyed.Instances(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(all) != 1 || all[0].ID != "keyed000001" {
		t.Fatalf("instances with the pool's key = %+v, want its interrupted install alone", all)
	}
	if err := keyed.Destroy(ctx, machine.Instance{ID: "theirs00001", Name: "theirs"}); !errors.Is(err, machine.ErrNotFound) {
		t.Fatalf("remove another pool's interrupted install: %v, want ErrNotFound", err)
	}
	if err := keyed.Destroy(ctx, all[0]); err != nil {
		t.Fatalf("remove the pool's interrupted install with its key: %v", err)
	}
}
