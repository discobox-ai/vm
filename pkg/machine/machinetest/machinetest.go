// Package machinetest is the conformance suite every machine.Driver must pass.
// It is the definition of done for a driver: the fake driver passes it in CI on
// every OS, and the hcs and vz drivers run it on real hardware.
//
// The suite speaks only the machine interfaces, so it checks exactly the
// contract the engine relies on (ADR 26-11-03-606): a VM is made fresh from
// the driver's base, or installed, found, listed, booted, and reached; a VM
// is found and listed by its name, which no second VM can take, committed or
// not; a booted machine's guest operations answer, and exec carries stdin,
// stdout, stderr, the environment, the working directory, and the exit code;
// an orderly shutdown stops the machine; a running instance is held to the
// size it was created with, where the driver sizes one; a driver that caps
// running guests of an OS boots that many and refuses one more until one
// stops; a driver
// that caps the VMs its provider has refuses a create past the cap until one
// is destroyed; on a driver that reports
// Capabilities.Commit, a committed VM carries its writes to VMs created from
// it and to no one else, and a copy of a copy outlives the first's removal,
// or the driver refuses the removal with ErrInUse, while a driver that does
// not refuses a commit and a copy and changes nothing (ADR 26-11-03-868),
// and every other subtest starts from a fresh VM, so needs no commit; VMs
// run side by side; Kill always works; a VM fenced at create keeps its fence
// across a boot, or the driver refuses the fence; a started machine is
// attached to from another call, its service has an ingress URL this process
// connects to where the driver gives one, and a pool URL that is an HTTP URL
// or the ingress one where it has one, and it is found stopped once its
// guest powers off; a
// guest's agent port is dialed through an attached machine and no port but
// the agent's and the service's is, and a guest reaches a peer's agent port
// through a forward and no other of its ports, or the driver refuses both; a
// data volume
// is kept across a boot and never in a committed VM; a cache volume is a
// private clone of its key's generation, which only a save moves; an
// upgrade gives a stopped VM a new root and keeps its name, settings, and
// volumes, or the driver refuses it; and a key
// the driver issues runs the pool's machines, cannot issue another, and
// works no more once it is revoked. That a VM is committed is the caller's
// to record, so the suite never boots one again. Every VM and cache the suite
// makes it removes, a removal that fails fails the case that made it, and
// the suite ends by failing on, and removing, any of its run still in a pool
// it used.
package machinetest

import (
	"archive/tar"
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"net/http"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/discobox-ai/vm/pkg/guest"
	"github.com/discobox-ai/vm/pkg/machine"
)

// Config adapts the suite to a driver. The suite starts from From, else
// from an Install on a driver that lists Capabilities.Install, else from a
// fresh VM of the driver's base.
type Config struct {
	// Install makes the committed VM the suite starts from.
	Install machine.InstallSpec
	// From names a committed VM already made to start from, which is how a
	// real driver keeps the suite from reinstalling an OS on every run.
	From string
	// CPUs and Memory size every instance the suite makes.
	CPUs   int
	Memory uint64
	// Size reads the size the provider holds a running instance to, as
	// someone with the provider would (the fake's record of its boot, a
	// container's limits, a stand-in's machine, the launcher's
	// configuration): the suite holds a driver whose definition declares
	// Definition.Sizes to applying CPUs and Memory, and fails one that
	// passes neither Size nor SkipSize.
	Size func(ctx context.Context, inst machine.Instance) (cpus int, memory uint64, err error)
	// SkipSize, when set, says why no Size reads a declaring driver's
	// size back, and the suite skips that subtest saying so.
	SkipSize string
	// Timeout bounds each boot until the guest answers, and each shutdown.
	Timeout time.Duration
	// ScratchDir is a guest directory the suite may write into.
	ScratchDir string
	// UnreachedIngress says an instance's ingress URL names a host this
	// process does not reach, as a stand-in of the provider's API gives:
	// the suite checks the URL, and does not connect to it.
	UnreachedIngress bool
	// URLClient is the HTTP client the suite reaches a credentialed URL
	// with (URLCapabilities.Credential): one that trusts a stand-in's TLS.
	// Nil is a client of the system's roots, speaking HTTP/1.1.
	URLClient *http.Client
	// Credentials lists the credentials the provider still honors for an
	// instance, by ID: on a driver that reports URLCapabilities.Credential,
	// the suite reads it after a commit and after a destroy, and wants
	// none. Nil skips those reads. A provider that lists nothing for a
	// machine it deleted shows nothing after a destroy, so there the read
	// after a commit, of a machine still there, is the one that tells; a
	// stand-in that keeps a deleted machine's credentials tells both.
	Credentials func(ctx context.Context, inst machine.Instance) ([]string, error)
	// HostGuest, when set, says why the guest is no machine of its own: a
	// stand-in whose guest is this host's process, on its network and its
	// mounts. The suite then looks from inside it for nothing only a machine
	// of its own shows: no resolver answering a guest whose fence closes DNS
	// (Egress.CloseDNS), and a data volume that is a mount. The fence and
	// the volume are still checked as the driver reports them.
	HostGuest string
	// GuestAgent is where the disco-vm agent is in the guest, which the
	// suite runs as `dial-host` to reach a peer through a forward. Empty
	// skips that subtest.
	GuestAgent string
	// Keyed is the driver again, for the same pool, calling its
	// provider with the key keys answers. A driver whose Capabilities.Keys
	// issues needs it.
	Keyed func(keys machine.KeySource) machine.Driver
	// Pool is the driver again, as its owner, for another pool of the
	// same provider (root). A driver that lists Capabilities.AcrossPools
	// needs it.
	Pool func(pool string) machine.Driver
	// Pause pauses a running instance as its provider would (fake's Pause,
	// a stand-in's paused sandbox). Nil skips the subtest of a paused
	// instance, for a driver that cannot be made to pause one.
	Pause func(ctx context.Context, inst machine.Instance) error
	// PauseHold is how long the suite holds an instance paused by
	// Machine.Pause before it resumes it, and checks that the guest's clock
	// caught up: 30 seconds unless set. A driver whose guests read the
	// host's own clock (fake) may hold it less.
	PauseHold time.Duration
	// SkipMaxMachines, when set, says why the suite must not fill the
	// provider's Capabilities.MaxMachines: a cap others share, such as an
	// account's, would refuse their creates while the suite holds it.
	SkipMaxMachines string
}

// Run runs the suite. The driver is one pool's, and the suite makes and
// removes VMs in it, every name its own.
func Run(t *testing.T, driver machine.Driver, cfg Config) {
	t.Helper()
	if cfg.Timeout == 0 {
		cfg.Timeout = 10 * time.Minute
	}
	if cfg.PauseHold == 0 {
		cfg.PauseHold = 30 * time.Second
	}
	ctx := context.Background()
	// Check first: a driver may learn a cap from its provider there
	// (Capabilities.MaxMachines).
	if err := driver.Check(ctx); err != nil {
		t.Fatalf("check: %v", err)
	}
	caps := driver.Capabilities()
	// What the driver declares of itself, with no driver: its registration.
	def, err := machine.DefinitionOf(driver.Name())
	if err != nil {
		t.Fatalf("the %s driver's definition: %v", driver.Name(), err)
	}

	t.Run("capabilities", func(t *testing.T) {
		if len(caps.GuestOS) == 0 {
			t.Fatal("no guest OS")
		}
		// A driver that installs makes VMs only from what it installs, by
		// copying it (ADR 26-11-03-868 §3).
		if len(caps.Install) > 0 && !caps.Commit {
			t.Fatalf("the driver installs %v, and does not report Commit", caps.Install)
		}
	})

	suite := t
	run := randomHex(4)
	// Registered first, so it runs last, once every subtest and every other
	// cleanup has removed what it made: a VM or cache of this run still in
	// a pool the suite used is one a removal left behind, which fails the
	// suite, and is removed again so it does not outlive the run. The
	// other pool across-pools makes is the run's alone, and its VMs and
	// this pool's are made from each other, so both are swept together.
	suite.Cleanup(func() {
		pools := []sweptPool{{driver, func(name string) bool {
			return name == "conformance-"+run || strings.HasPrefix(name, "conformance-"+run+"-")
		}}}
		if caps.AcrossPools && caps.Commit && cfg.Pool != nil {
			pools = append(pools, sweptPool{cfg.Pool("conformance-" + run), func(string) bool { return true }})
		}
		sweep(suite, pools)
		if caps.Attaches(machine.VolumeCache) {
			sweepCaches(suite, driver, pools[0].ours)
		}
	})
	names := 0
	newName := func() string {
		names++
		return fmt.Sprintf("conformance-%s-%d", run, names)
	}
	// start is what the first VMs are made from: a committed VM, or the
	// driver's base.
	var start machine.InstanceSpec
	switch {
	case cfg.From != "":
		if !caps.Commit {
			t.Fatalf("Config.From %s, on a driver that does not report Commit", cfg.From)
		}
		from, err := driver.Instance(ctx, cfg.From)
		if err != nil {
			t.Fatalf("committed VM %s: %v", cfg.From, err)
		}
		start.From = &from
	case cfg.Install.GuestOS != "" && caps.Installs(cfg.Install.GuestOS):
		installed, err := driver.Install(ctx, cfg.Install, newName())
		if err != nil {
			t.Fatalf("install: %v", err)
		}
		t.Cleanup(func() { remove(t, driver, installed) })
		if installed.GuestOS != cfg.Install.GuestOS {
			t.Fatalf("installed %+v, want a %s guest", installed, cfg.Install.GuestOS)
		}
		start.From = &installed
	}
	// guestOS is the start's, which a fresh VM learns from the first one.
	var guestOS machine.OS
	if start.From != nil {
		guestOS = start.From.GuestOS
	}
	of := func(from machine.Instance) machine.InstanceSpec { return machine.InstanceSpec{From: &from} }
	// suiteOwned are the VMs committed, which the suite removes, not the
	// subtest that made them.
	suiteOwned := map[string]bool{}
	create := func(t *testing.T, spec machine.InstanceSpec) machine.Instance {
		t.Helper()
		spec.Name = newName()
		if spec.CPUs == 0 {
			spec.CPUs, spec.Memory = cfg.CPUs, cfg.Memory
		}
		inst, err := driver.Create(ctx, spec)
		if err != nil {
			t.Fatalf("create %s: %v", spec.Name, err)
		}
		// A step that fails before the VM is removed would leave, on a
		// cloud driver, a billed machine.
		t.Cleanup(func() {
			if !suiteOwned[inst.Name] {
				remove(t, driver, inst)
			}
		})
		if inst.Name != spec.Name || inst.ID == "" || inst.Pool == "" {
			t.Fatalf("created %+v for name %s", inst, spec.Name)
		}
		if spec.From != nil && inst.GuestOS != spec.From.GuestOS {
			t.Fatalf("created %+v from a %s guest", inst, spec.From.GuestOS)
		}
		if !caps.Commit && inst.From != "" {
			t.Fatalf("a driver that does not commit created %+v from %s", inst, inst.From)
		}
		if !caps.SupportsOS(inst.GuestOS) {
			t.Fatalf("created a %q guest, which the driver does not run", inst.GuestOS)
		}
		return inst
	}
	// reachable waits until a machine just booted or started answers: a
	// driver may return once its machine runs, its guest operations
	// ErrUnreachable until the guest answers (machine.UntilReachable).
	reachable := func(t *testing.T, m machine.Machine) {
		t.Helper()
		waitCtx, cancel := context.WithTimeout(ctx, cfg.Timeout)
		defer cancel()
		err := machine.UntilReachable(waitCtx, func() error {
			_, err := m.Info(waitCtx)
			return err
		})
		if err != nil {
			t.Fatalf("the guest of a machine just started: %v", err)
		}
	}
	boot := func(t *testing.T, inst machine.Instance) machine.Machine {
		t.Helper()
		bootCtx, cancel := context.WithTimeout(ctx, cfg.Timeout)
		defer cancel()
		m, err := driver.Boot(bootCtx, inst, machine.BootOptions{})
		if err != nil {
			t.Fatalf("boot %s: %v", inst.Name, err)
		}
		t.Cleanup(func() { _ = m.Kill(context.Background()) })
		reachable(t, m)
		return m
	}
	shutdown := func(t *testing.T, m machine.Machine) {
		t.Helper()
		if err := m.Shutdown(ctx, false); err != nil {
			t.Fatalf("shutdown request: %v", err)
		}
		select {
		case <-m.Done():
			if err := m.Err(); err != nil {
				t.Fatalf("an orderly shutdown ended in error: %v", err)
			}
		case <-time.After(cfg.Timeout):
			t.Fatal("the guest did not power off after an orderly shutdown")
		}
	}
	// commit freezes a stopped VM, which stays under its name, with no
	// volume. The suite removes it once every subtest is done, since later
	// subtests create from it; a VM created from it is removed first.
	commit := func(t *testing.T, inst machine.Instance) machine.Instance {
		t.Helper()
		committed, err := driver.Commit(ctx, inst)
		if err != nil {
			t.Fatalf("commit %s: %v", inst.Name, err)
		}
		suiteOwned[inst.Name] = true
		suite.Cleanup(func() { remove(suite, driver, committed) })
		if committed.Name != inst.Name || committed.GuestOS != inst.GuestOS || committed.Data != nil || committed.Cache != nil {
			t.Fatalf("committed %+v from %+v", committed, inst)
		}
		found, err := driver.Instance(ctx, inst.Name)
		if err != nil || found.ID != committed.ID {
			t.Fatalf("the committed VM %s: %+v, %v; committed %+v", inst.Name, found, err, committed)
		}
		return committed
	}
	marker := strings.TrimRight(cfg.ScratchDir, `/\`) + "/disco-vm-conformance"

	// Boot a VM of the start, write a marker, shut down, and commit. A
	// driver that does not commit refuses, running or stopped, and refuses a
	// copy, and the VM is as it was: still found, and booted again with the
	// marker.
	var committed machine.Instance
	wrote := false
	t.Run("boot-write-commit", func(t *testing.T) {
		inst := create(t, start)
		if start.From == nil {
			guestOS = inst.GuestOS
		}
		m := boot(t, inst)
		info, err := m.Info(ctx)
		if err != nil {
			t.Fatal(err)
		}
		if info.OS != string(guestOS) {
			t.Fatalf("guest reports OS %q, want %q", info.OS, guestOS)
		}
		if err := m.CopyTo(ctx, marker, tarOf(t, "proof.txt", "committed")); err != nil {
			t.Fatalf("write marker: %v", err)
		}
		checkExec(t, m, guestOS, marker)
		if !caps.Commit {
			if _, err := driver.Commit(ctx, inst); !errors.Is(err, machine.ErrUnsupported) {
				t.Fatalf("a driver that does not commit committed a running VM: %v, want ErrUnsupported", err)
			}
			if got := readMarker(t, m, marker+"/proof.txt"); got != "committed" {
				t.Fatalf("a refused commit left the running VM reading %q", got)
			}
		}
		shutdown(t, m)
		if caps.Commit {
			committed = commit(t, inst)
			wrote = true
			return
		}
		if _, err := driver.Commit(ctx, inst); !errors.Is(err, machine.ErrUnsupported) {
			t.Fatalf("a driver that does not commit committed a stopped VM: %v, want ErrUnsupported", err)
		}
		if found, err := driver.Instance(ctx, inst.Name); err != nil || found.ID != inst.ID {
			t.Fatalf("a refused commit: %s is %+v, %v; created %+v", inst.Name, found, err, inst)
		}
		copied := of(inst)
		copied.Name = fmt.Sprintf("conformance-%s-copied", run)
		if made, err := driver.Create(ctx, copied); !errors.Is(err, machine.ErrUnsupported) {
			if err == nil {
				remove(t, driver, made)
			}
			t.Fatalf("a driver that does not commit made a copy: %v, want ErrUnsupported", err)
		}
		if _, err := driver.Instance(ctx, copied.Name); !errors.Is(err, machine.ErrNotFound) {
			t.Fatalf("a refused copy left %s: %v, want ErrNotFound", copied.Name, err)
		}
		m = boot(t, inst)
		if got := readMarker(t, m, marker+"/proof.txt"); got != "committed" {
			t.Fatalf("a VM whose commit was refused reads %q once booted again", got)
		}
		shutdown(t, m)
		wrote = true
	})
	if !wrote {
		t.FailNow()
	}
	// copied is a VM of the committed one, where the driver commits, and of
	// the start where it does not.
	copied := start
	if caps.Commit {
		copied = of(committed)
	}

	// A committed VM is listed, and its name is taken, as any VM's.
	t.Run("a-committed-vm-is-found-and-listed", func(t *testing.T) {
		if !caps.Commit {
			t.Skip("the driver does not commit")
		}
		list, err := driver.Instances(ctx)
		if err != nil {
			t.Fatal(err)
		}
		if !slices.ContainsFunc(list, func(i machine.Instance) bool { return i.Name == committed.Name && i.ID == committed.ID }) {
			t.Fatalf("instances do not list the committed VM %s: %+v", committed.Name, list)
		}
		again := of(committed)
		again.Name = committed.Name
		if inst, err := driver.Create(ctx, again); !errors.Is(err, machine.ErrExists) {
			if err == nil {
				remove(t, driver, inst)
			}
			t.Fatalf("a VM named as the committed VM %s: %v, want ErrExists", committed.Name, err)
		}
		if _, err := driver.Attach(ctx, committed); !errors.Is(err, machine.ErrNotRunning) {
			t.Fatalf("attach to a committed VM: %v, want ErrNotRunning", err)
		}
		wantState(t, driver, committed, machine.StateStopped, "committed")
	})

	t.Run("an-instance-is-found-listed-and-its-name-taken", func(t *testing.T) {
		inst := create(t, start)
		got, err := driver.Instance(ctx, inst.Name)
		if err != nil || got.ID != inst.ID || got.Name != inst.Name || got.GuestOS != guestOS || (got.From != "" && (start.From == nil || got.From != start.From.Name)) {
			t.Fatalf("instance %s: %+v, %v; created %+v", inst.Name, got, err, inst)
		}
		list, err := driver.Instances(ctx)
		if err != nil {
			t.Fatal(err)
		}
		if !slices.ContainsFunc(list, func(i machine.Instance) bool { return i.Name == inst.Name && i.ID == inst.ID }) {
			t.Fatalf("instances do not list %s: %+v", inst.Name, list)
		}
		again := start
		again.Name = inst.Name
		if _, err := driver.Create(ctx, again); !errors.Is(err, machine.ErrExists) {
			t.Fatalf("a second instance named %s: %v, want ErrExists", inst.Name, err)
		}
		if _, err := driver.Attach(ctx, inst); !errors.Is(err, machine.ErrNotRunning) {
			t.Fatalf("attach to a stopped instance: %v, want ErrNotRunning", err)
		}
		wantState(t, driver, inst, machine.StateStopped, "created")
		if err := driver.Destroy(ctx, inst); err != nil {
			t.Fatalf("destroy: %v", err)
		}
		if err := driver.Destroy(ctx, inst); err != nil {
			t.Fatalf("destroy again: %v", err)
		}
		if _, err := driver.Instance(ctx, inst.Name); !errors.Is(err, machine.ErrNotFound) {
			t.Fatalf("a destroyed instance: %v, want ErrNotFound", err)
		}
	})

	// A fresh VM is made from the driver's base, and runs a guest OS it
	// lists; a driver with no base refuses it.
	t.Run("a-fresh-vm", func(t *testing.T) {
		spec := machine.InstanceSpec{Name: newName(), CPUs: cfg.CPUs, Memory: cfg.Memory}
		inst, err := driver.Create(ctx, spec)
		if errors.Is(err, machine.ErrUnsupported) {
			if !caps.Commit {
				t.Fatalf("a driver with no base does not report Commit, so makes no VM at all: %v", err)
			}
			t.Skipf("the driver has no base: %v", err)
		}
		if err != nil {
			t.Fatalf("create a fresh VM: %v", err)
		}
		t.Cleanup(func() { remove(t, driver, inst) })
		if inst.From != "" || !caps.SupportsOS(inst.GuestOS) {
			t.Fatalf("a fresh VM %+v", inst)
		}
		m := boot(t, inst)
		info, err := m.Info(ctx)
		if err != nil {
			t.Fatal(err)
		}
		if info.OS != string(inst.GuestOS) {
			t.Fatalf("a fresh VM reports OS %q, want %q", info.OS, inst.GuestOS)
		}
		shutdown(t, m)
	})

	// A VM created from the committed one sees the marker; one of the start
	// does not; both run at once. Where the driver does not commit, both
	// are of the start.
	t.Run("copies-are-isolated-and-concurrent", func(t *testing.T) {
		child := create(t, copied)
		sibling := create(t, start)
		limit := caps.MaxRunning[guestOS]
		childM := boot(t, child)
		if limit == 1 {
			shutdown(t, childM)
		}
		siblingM := boot(t, sibling)

		if limit != 1 && caps.Commit {
			if got := readMarker(t, childM, marker+"/proof.txt"); got != "committed" {
				t.Fatalf("a VM created from the committed one reads %q", got)
			}
		}
		if _, err := siblingM.CopyFrom(ctx, marker+"/proof.txt"); err == nil {
			t.Fatal("a VM of the start saw a write committed to another")
		}
	})

	// A driver that caps running guests of the OS (Capabilities.MaxRunning)
	// boots that many and refuses one more, by Boot and by Start, with
	// ErrCapacity, until one stops. The cap is the host's, so the suite
	// needs every other guest of the OS stopped.
	t.Run("max-running", func(t *testing.T) {
		limit := caps.MaxRunning[guestOS]
		if limit == 0 {
			t.Skipf("no cap on %s guests", guestOS)
		}
		var running []machine.Machine
		for range limit {
			running = append(running, boot(t, create(t, start)))
		}
		extra := create(t, start)
		if _, err := driver.Boot(ctx, extra, machine.BootOptions{}); !errors.Is(err, machine.ErrCapacity) {
			t.Fatalf("boot past a cap of %d %s guests: %v, want ErrCapacity", limit, guestOS, err)
		}
		if _, err := driver.Start(ctx, extra, machine.BootOptions{}); !errors.Is(err, machine.ErrCapacity) {
			t.Fatalf("start past a cap of %d %s guests: %v, want ErrCapacity", limit, guestOS, err)
		}
		shutdown(t, running[0])
		shutdown(t, boot(t, extra))
		for _, m := range running[1:] {
			shutdown(t, m)
		}
	})

	// A driver that caps the VMs its provider has (Capabilities.MaxMachines)
	// refuses a create past the cap with ErrCapacity, and a destroy makes
	// room. The provider may count VMs the suite does not see, so it creates
	// until refused, never more than the cap.
	t.Run("max-machines", func(t *testing.T) {
		limit := caps.MaxMachines[guestOS]
		switch {
		case limit == 0:
			t.Skipf("no cap on %s VMs", guestOS)
		case cfg.SkipMaxMachines != "":
			t.Skip(cfg.SkipMaxMachines)
		}
		// Each VM's cleanup is the subtest's own, not create's, since one
		// is refused.
		next := func() (machine.Instance, error) {
			spec := start
			spec.Name = newName()
			if spec.CPUs == 0 {
				spec.CPUs, spec.Memory = cfg.CPUs, cfg.Memory
			}
			return driver.Create(ctx, spec)
		}
		var made []machine.Instance
		t.Cleanup(func() {
			for _, inst := range made {
				remove(t, driver, inst)
			}
		})
		for {
			inst, err := next()
			if errors.Is(err, machine.ErrCapacity) {
				break
			}
			if err != nil {
				t.Fatalf("create under a cap of %d %s VMs: %v", limit, guestOS, err)
			}
			made = append(made, inst)
			if len(made) > limit {
				t.Fatalf("created %d VMs past a cap of %d %s VMs", len(made), limit, guestOS)
			}
		}
		if len(made) == 0 {
			t.Fatalf("the provider is at its cap of %d %s VMs before the suite made one", limit, guestOS)
		}
		if err := driver.Destroy(ctx, made[0]); err != nil {
			t.Fatalf("destroy: %v", err)
		}
		made = made[1:]
		inst, err := next()
		if err != nil {
			t.Fatalf("create once a destroy made room: %v", err)
		}
		made = append(made, inst)
	})

	t.Run("kill", func(t *testing.T) {
		m := boot(t, create(t, start))
		if err := m.Kill(ctx); err != nil {
			t.Fatalf("kill: %v", err)
		}
		select {
		case <-m.Done():
		case <-time.After(time.Minute):
			t.Fatal("Done was not closed after Kill")
		}
	})

	// A driver that sizes a machine (Definition.Sizes) holds a running
	// instance to the size it was created with.
	t.Run("sized", func(t *testing.T) {
		switch {
		case !def.Sizes:
			t.Skip("the driver does not declare that it sizes a machine")
		case cfg.SkipSize != "":
			t.Skip(cfg.SkipSize)
		case cfg.Size == nil:
			t.Fatal("the driver declares that it sizes a machine, and no Config.Size reads the size back, nor Config.SkipSize says why")
		case cfg.CPUs == 0 || cfg.Memory == 0:
			t.Fatal("Config.Size is given, and Config.CPUs and Memory size nothing")
		}
		inst := create(t, start)
		m := boot(t, inst)
		cpus, memory, err := cfg.Size(ctx, inst)
		if err != nil {
			t.Fatalf("read the size of %s: %v", inst.Name, err)
		}
		if cpus != cfg.CPUs || memory != cfg.Memory {
			t.Fatalf("%s runs with %d CPUs and %d bytes of memory, created with %d and %d", inst.Name, cpus, memory, cfg.CPUs, cfg.Memory)
		}
		shutdown(t, m)
	})

	// An instance fenced at create, to nothing from the start and to a list
	// from a committed VM where the driver commits, reports the fence as asked, while it runs and once it
	// stops. A driver refuses each fence it does not hold.
	t.Run("a-fenced-instance", func(t *testing.T) {
		held := 0
		for i, from := range []struct {
			spec  machine.InstanceSpec
			allow []string
		}{
			{start, nil},
			{copied, []string{"example.com"}},
		} {
			spec := from.spec
			spec.Egress = &machine.Egress{Allow: from.allow}
			if !caps.Fences(spec.Egress) {
				spec.Name = fmt.Sprintf("conformance-%s-fenced-%d", run, i)
				if inst, err := driver.Create(ctx, spec); !errors.Is(err, machine.ErrUnsupported) {
					if err == nil {
						remove(t, driver, inst)
					}
					t.Fatalf("a driver that does not hold the fence %q made an instance fenced to it: %v, want ErrUnsupported", from.allow, err)
				}
				continue
			}
			held++
			inst := create(t, spec)
			fenced := func(when string) {
				t.Helper()
				got, err := driver.Instance(ctx, inst.Name)
				if err != nil || got.Egress == nil || !slices.Equal(got.Egress.Allow, from.allow) {
					t.Fatalf("an instance fenced to %q %s reports %+v: %v", from.allow, when, got.Egress, err)
				}
			}
			fenced("once created")
			m := boot(t, inst)
			fenced("while it runs")
			shutdown(t, m)
			fenced("once it stops")
		}
		if held == 0 {
			t.Skip("the driver holds no fence")
		}
	})

	// A fence that closes DNS, to an allowed host, is reported so, and,
	// from inside the guest, no name resolves: neither through the guest's
	// resolver, nor at any resolver its resolv.conf names, nor at 8.8.8.8 or
	// 1.1.1.1 (ADR 26-11-17-311 §§2, 5). A driver that holds a list and not
	// this refuses it.
	t.Run("close-dns", func(t *testing.T) {
		spec := copied
		spec.Egress = &machine.Egress{Allow: []string{"example.com"}, CloseDNS: true}
		if !caps.Fences(spec.Egress) {
			if !slices.Contains(caps.Egress, machine.EgressAllow) {
				t.Skip("the driver holds no list")
			}
			spec.Name, spec.CPUs, spec.Memory = newName(), cfg.CPUs, cfg.Memory
			if inst, err := driver.Create(ctx, spec); !errors.Is(err, machine.ErrUnsupported) {
				if err == nil {
					remove(t, driver, inst)
				}
				t.Fatalf("a driver that does not close DNS made an instance fenced to close it: %v, want ErrUnsupported", err)
			}
			return
		}
		inst := create(t, spec)
		if got, err := driver.Instance(ctx, inst.Name); err != nil || got.Egress == nil || !got.Egress.CloseDNS || !slices.Equal(got.Egress.Allow, spec.Egress.Allow) {
			t.Fatalf("an instance fenced to %+v reports %+v: %v", *spec.Egress, got.Egress, err)
		}
		if guestOS != machine.Linux && inst.GuestOS != machine.Linux {
			t.Skip("the suite looks for a resolver from a linux guest")
		}
		if cfg.HostGuest != "" {
			t.Skip(cfg.HostGuest)
		}
		m := boot(t, inst)
		var out bytes.Buffer
		code, err := machine.Run(ctx, m, machine.ExecRequest{Argv: []string{"bash", "-c", resolveScript}}, nil, &out, &out)
		if err != nil {
			t.Fatalf("look for a resolver in the guest: %v\n%s", err, out.String())
		}
		if code != 0 {
			t.Fatalf("a guest whose fence closes DNS resolves names:\n%s", out.String())
		}
	})

	// A driver whose URLs answer only a credential it mints gives one with
	// every Service, new at each call and finite; each URL refuses a request
	// with none, or a wrong one, before the guest answers, and passes one
	// with it; a request with none leaves a stopped instance stopped; and a
	// commit and a destroy leave the provider honoring none of its
	// credentials (ADR 26-11-17-311 §§3-5). Any other driver gives none.
	t.Run("credentialed-url", func(t *testing.T) {
		if !caps.URLs.Credential {
			// start-attach-and-stop checks it gives none.
			t.Skip("the driver's URLs need no credential")
		}
		inst := create(t, copied)
		startCtx, cancel := context.WithTimeout(ctx, cfg.Timeout)
		defer cancel()
		m, err := driver.Start(startCtx, inst, machine.BootOptions{})
		if err != nil {
			t.Fatalf("start: %v", err)
		}
		t.Cleanup(func() { _ = m.Kill(context.Background()) })
		reachable(t, m)
		service, err := driver.Service(ctx, inst)
		if err != nil {
			t.Fatalf("service of a running instance: %v", err)
		}
		cred := service.Credential
		switch {
		case cred == nil || cred.Header == "" || cred.Token == "":
			t.Fatalf("a driver whose URLs need a credential gave %+v", cred)
		case !cred.Expires.After(time.Now()) || cred.Expires.After(time.Now().AddDate(1, 0, 1)):
			t.Fatalf("a credential that expires %s: want a finite life from now", cred.Expires)
		}
		again, err := driver.Service(ctx, inst)
		if err != nil || again.Credential == nil || again.Credential.Token == cred.Token {
			t.Fatalf("a second Service gave %+v, %v: want a credential of its own", again.Credential, err)
		}
		var urls []string
		for _, u := range []string{service.IngressURL, service.PoolURL} {
			if u != "" && !slices.Contains(urls, u) {
				urls = append(urls, u)
			}
		}
		reach := !cfg.UnreachedIngress
		if reach {
			for _, u := range urls {
				for _, c := range []*machine.Credential{nil, {Header: cred.Header, Token: "not-" + cred.Token}} {
					if code := urlStatus(ctx, t, cfg.URLClient, u, c); code != http.StatusUnauthorized {
						t.Fatalf("GET %s with credential %v: %d, want %d", u, c != nil, code, http.StatusUnauthorized)
					}
				}
				if code := urlStatus(ctx, t, cfg.URLClient, u, cred); code == http.StatusUnauthorized {
					t.Fatalf("GET %s with its credential: %d", u, code)
				}
			}
		}
		shutdown(t, m)
		stopped := func(when string) {
			t.Helper()
			if _, err := driver.Attach(ctx, inst); !errors.Is(err, machine.ErrNotRunning) {
				t.Fatalf("an instance %s attaches: %v, want ErrNotRunning", when, err)
			}
		}
		stopped("once it stopped")
		if reach {
			for _, u := range urls {
				if code := urlStatus(ctx, t, cfg.URLClient, u, nil); code != http.StatusUnauthorized {
					t.Fatalf("GET %s of a stopped instance with no credential: %d, want %d", u, code, http.StatusUnauthorized)
				}
			}
			time.Sleep(5 * time.Second)
			stopped("asked for with no credential")
		}
		none := func(when string) {
			t.Helper()
			if cfg.Credentials == nil {
				return
			}
			left, err := cfg.Credentials(ctx, inst)
			if err != nil || len(left) > 0 {
				t.Fatalf("%s, the provider honors credentials %v: %v", when, left, err)
			}
		}
		if caps.Commit {
			committed, err := driver.Commit(ctx, inst)
			if err != nil {
				t.Fatalf("commit %s: %v", inst.Name, err)
			}
			none("once it is committed")
			if reach {
				for _, u := range urls {
					if code := urlStatus(ctx, t, cfg.URLClient, u, cred); code != http.StatusUnauthorized {
						t.Fatalf("GET %s of a committed VM with a credential from before: %d, want %d", u, code, http.StatusUnauthorized)
					}
				}
				time.Sleep(5 * time.Second)
				stopped("once committed and asked for with an old credential")
			}
			inst = committed
		}
		if err := driver.Destroy(ctx, inst); err != nil {
			t.Fatalf("destroy %s: %v", inst.Name, err)
		}
		none("once it is destroyed")
	})

	// A bare instance is made where the driver strips one or gives none
	// anything, and reported bare; a driver that cannot refuses it. A
	// driver that does not place machines on networks refuses them.
	t.Run("bare", func(t *testing.T) {
		spec := copied
		spec.Bare = true
		if !caps.Bare.Strips() {
			spec.Name, spec.CPUs, spec.Memory = newName(), cfg.CPUs, cfg.Memory
			if inst, err := driver.Create(ctx, spec); !errors.Is(err, machine.ErrUnsupported) {
				if err == nil {
					remove(t, driver, inst)
				}
				t.Fatalf("a driver whose machines are never bare made a bare one: %v, want ErrUnsupported", err)
			}
		} else {
			inst := create(t, spec)
			if got, err := driver.Instance(ctx, inst.Name); err != nil || !got.Bare {
				t.Fatalf("an instance made bare reports %+v: %v", got, err)
			}
		}
		if !caps.Networks {
			spec := copied
			spec.Name, spec.CPUs, spec.Memory, spec.Networks = newName(), cfg.CPUs, cfg.Memory, []string{"conformance"}
			if inst, err := driver.Create(ctx, spec); !errors.Is(err, machine.ErrUnsupported) {
				if err == nil {
					remove(t, driver, inst)
				}
				t.Fatalf("a driver that places no machine on networks made one on them: %v, want ErrUnsupported", err)
			}
		}
	})

	// A guest port is dialed through a machine another call attached to: the
	// agent's, where an HTTP request for its health is answered. Any port but
	// the agent's and the service's is refused (ADR 26-11-03-247). A driver
	// that reaches no guest port refuses the dial.
	t.Run("a-guest-port", func(t *testing.T) {
		inst := create(t, start)
		startCtx, cancel := context.WithTimeout(ctx, cfg.Timeout)
		defer cancel()
		started, err := driver.Start(startCtx, inst, machine.BootOptions{})
		if err != nil {
			t.Fatalf("start: %v", err)
		}
		t.Cleanup(func() { _ = started.Kill(context.Background()) })
		reachable(t, started)
		m, err := driver.Attach(ctx, inst)
		if err != nil {
			t.Fatalf("attach: %v", err)
		}
		dialCtx, cancel := context.WithTimeout(ctx, time.Minute)
		defer cancel()
		conn, err := m.Dial(dialCtx, guest.AgentPort)
		if !caps.Ports {
			if !errors.Is(err, machine.ErrUnsupported) {
				t.Fatalf("a driver that reaches no guest port dialed one: %v, want ErrUnsupported", err)
			}
			t.Skip("the driver reaches no guest port")
		}
		if err != nil {
			t.Fatalf("dial the agent's port: %v", err)
		}
		defer conn.Close()
		if other, err := m.Dial(dialCtx, guest.DisplayPort); !errors.Is(err, machine.ErrUnsupported) {
			if err == nil {
				_ = other.Close()
			}
			t.Fatalf("dial guest port %d, neither the agent's nor the service's: %v, want ErrUnsupported", guest.DisplayPort, err)
		}
		_ = conn.SetDeadline(time.Now().Add(time.Minute))
		if _, err := io.WriteString(conn, "GET /v1/health HTTP/1.0\r\nHost: guest\r\n\r\n"); err != nil {
			t.Fatal(err)
		}
		answer, _ := io.ReadAll(conn)
		if !strings.HasPrefix(string(answer), "HTTP/1.0 200") && !strings.HasPrefix(string(answer), "HTTP/1.1 200") {
			t.Fatalf("the agent's port answered %q", answer)
		}
	})

	// A guest reaches a peer's guest port through a forward, and it is the
	// peer that answers: the agent's port of an instance holding a file the
	// other does not, which the request reads back. While the peer is stopped
	// the guest's connection is closed with nothing read, and a peer started
	// again is followed. A forward to a peer's port other than its agent's or
	// its service's is refused at Create.
	t.Run("a-peer-forward", func(t *testing.T) {
		if caps.Ports {
			spec := start
			spec.Name = fmt.Sprintf("conformance-%s-other-port", run)
			spec.Forwards = []machine.Forward{{Port: 7402, Instance: "peer", InstancePort: guest.DisplayPort}}
			if inst, err := driver.Create(ctx, spec); !errors.Is(err, machine.ErrUnsupported) {
				if err == nil {
					remove(t, driver, inst)
				}
				t.Fatalf("a forward to a peer's port %d, neither its agent's nor its service's: %v, want ErrUnsupported", guest.DisplayPort, err)
			}
		}
		if !caps.Ports {
			spec := start
			spec.Name = fmt.Sprintf("conformance-%s-forwarded", run)
			spec.Forwards = []machine.Forward{{Port: 7402, Instance: "peer", InstancePort: guest.AgentPort}}
			if inst, err := driver.Create(ctx, spec); !errors.Is(err, machine.ErrUnsupported) {
				if err == nil {
					remove(t, driver, inst)
				}
				t.Fatalf("a driver that reaches no guest port made a peer forward: %v, want ErrUnsupported", err)
			}
			t.Skip("the driver reaches no guest port")
		}
		if cfg.GuestAgent == "" {
			t.Skip("no GuestAgent to dial the host from inside the guest")
		}
		start := func(t *testing.T, inst machine.Instance) machine.Machine {
			t.Helper()
			startCtx, cancel := context.WithTimeout(ctx, cfg.Timeout)
			defer cancel()
			m, err := driver.Start(startCtx, inst, machine.BootOptions{})
			if err != nil {
				t.Fatalf("start %s: %v", inst.Name, err)
			}
			t.Cleanup(func() { _ = m.Kill(context.Background()) })
			reachable(t, m)
			return m
		}
		peer := create(t, copied)
		pm := start(t, peer)
		proof := randomHex(8)
		if err := pm.CopyTo(ctx, marker, tarOf(t, "peer.txt", proof)); err != nil {
			t.Fatal(err)
		}
		// The peer is killed below, a power cut, and must still hold the
		// file when it starts again.
		if guestOS != machine.Windows {
			if code, err := machine.Run(ctx, pm, machine.ExecRequest{Argv: []string{"sync"}}, nil, io.Discard, io.Discard); err != nil || code != 0 {
				t.Fatalf("sync the peer: exit %d, %v", code, err)
			}
		}
		spec := copied
		spec.Forwards = []machine.Forward{{Port: 7402, Instance: peer.Name, InstancePort: guest.AgentPort}}
		inst := create(t, spec)
		got, err := driver.Instance(ctx, inst.Name)
		if err != nil || len(got.Forwards) != 1 || got.Forwards[0] != spec.Forwards[0] {
			t.Fatalf("an instance with a peer forward reports %+v: %v", got.Forwards, err)
		}
		m := start(t, inst)
		request := "GET /v1/files?path=" + marker + "/peer.txt HTTP/1.0\r\nHost: guest\r\n\r\n"
		argv := []string{"/bin/sh", "-c", `printf "$1" | "$0" dial-host 7402`, cfg.GuestAgent, strings.ReplaceAll(request, "\r\n", `\r\n`)}
		if guestOS == machine.Windows {
			argv = []string{"powershell", "-NoProfile", "-Command", "\"" + strings.ReplaceAll(request, "\r\n", "`r`n") + "\" | & '" + cfg.GuestAgent + "' dial-host 7402"}
		}
		ask := func(t *testing.T) string {
			t.Helper()
			var out bytes.Buffer
			if _, err := machine.Run(ctx, m, machine.ExecRequest{Argv: argv}, nil, &out, &out); err != nil {
				t.Fatalf("dial-host through the forward: %v\n%s", err, out.String())
			}
			return out.String()
		}
		if got := ask(t); !strings.Contains(got, proof) {
			t.Fatalf("the peer's agent did not answer with its file; the guest read %q", got)
		}
		if err := pm.Kill(ctx); err != nil {
			t.Fatal(err)
		}
		// Kill returns once the stop is asked; the peer is stopped once its
		// machine says so, and only then may it start again.
		select {
		case <-pm.Done():
		case <-time.After(cfg.Timeout):
			t.Fatal("the peer did not stop after Kill")
		}
		if got := ask(t); strings.Contains(got, proof) {
			t.Fatalf("a stopped peer still answered through the forward: %q", got)
		}
		start(t, peer)
		if got := ask(t); !strings.Contains(got, proof) {
			t.Fatalf("the forward did not follow its peer once it started again; the guest read %q", got)
		}
	})

	// A driver that lists AcrossPools creates a VM from a committed VM of
	// another pool, and forwards a guest's connections to an instance of
	// another pool, which its owner names and a key it issued, one pool's,
	// does not; a driver that does not refuses both.
	t.Run("across-pools", func(t *testing.T) {
		other := "conformance-" + run
		if !caps.AcrossPools {
			if caps.Commit {
				from := committed
				from.Pool = other
				spec := machine.InstanceSpec{Name: fmt.Sprintf("conformance-%s-across", run), From: &from}
				if inst, err := driver.Create(ctx, spec); !errors.Is(err, machine.ErrUnsupported) {
					if err == nil {
						remove(t, driver, inst)
					}
					t.Fatalf("a VM from another pool's committed VM, on a driver that does not list AcrossPools: %v, want ErrUnsupported", err)
				}
			}
			spec := copied
			spec.Name = fmt.Sprintf("conformance-%s-across", run)
			spec.Forwards = []machine.Forward{{Port: 7402, Pool: other, Instance: "peer", InstancePort: guest.AgentPort}}
			if inst, err := driver.Create(ctx, spec); !errors.Is(err, machine.ErrUnsupported) {
				if err == nil {
					remove(t, driver, inst)
				}
				t.Fatalf("a forward to another pool's instance, on a driver that does not list AcrossPools: %v, want ErrUnsupported", err)
			}
			if caps.Commit && caps.Attaches(machine.VolumeData) {
				spec := copied
				spec.Data = &machine.DataVolume{}
				inst := create(t, spec)
				from := committed
				from.Pool = other
				if _, err := driver.Upgrade(ctx, inst, &from); !errors.Is(err, machine.ErrUnsupported) {
					t.Fatalf("an upgrade to another pool's committed VM, on a driver that does not list AcrossPools: %v, want ErrUnsupported", err)
				}
			}
			t.Skip("the driver reaches no other pool")
		}
		if cfg.Pool == nil {
			t.Fatal("the driver lists AcrossPools, and Config.Pool is nil")
		}
		if !caps.Commit {
			t.Skip("the driver commits nothing to copy across pools")
		}
		od := cfg.Pool(other)
		// The other pool's VM, a copy of this pool's committed VM, with a
		// proof of its own, committed there: a VM of this pool is created
		// from it, and forwards to a copy of it that runs.
		src, err := od.Create(ctx, machine.InstanceSpec{Name: "source", From: &committed, CPUs: cfg.CPUs, Memory: cfg.Memory})
		if err != nil {
			t.Fatalf("a VM of pool %s from this pool's committed VM: %v", other, err)
		}
		t.Cleanup(func() { remove(t, od, src) })
		if src.Pool != other || src.From != committed.Name || src.FromPool != committed.Pool {
			t.Fatalf("a VM of pool %s from %+v is %+v", other, committed, src)
		}
		if _, err := driver.Instance(ctx, "source"); !errors.Is(err, machine.ErrNotFound) {
			t.Fatalf("this pool finds pool %s's VM: %v, want ErrNotFound", other, err)
		}
		proof := randomHex(8)
		bootCtx, cancel := context.WithTimeout(ctx, cfg.Timeout)
		sm, err := od.Boot(bootCtx, src, machine.BootOptions{})
		cancel()
		if err != nil {
			t.Fatalf("boot %s of pool %s: %v", src.Name, other, err)
		}
		t.Cleanup(func() { _ = sm.Kill(context.Background()) })
		reachable(t, sm)
		if err := sm.CopyTo(ctx, marker, tarOf(t, "pool.txt", proof)); err != nil {
			t.Fatal(err)
		}
		shutdown(t, sm)
		from, err := od.Commit(ctx, src)
		if err != nil {
			t.Fatalf("commit %s of pool %s: %v", src.Name, other, err)
		}
		if from.Pool != other {
			t.Fatalf("pool %s committed %+v", other, from)
		}
		if cfg.Keyed != nil && caps.Keys.Mode == machine.KeysIssue {
			key, err := driver.IssueKey(ctx, machine.IssueRequest{Name: "conformance-" + run + "-pools", TTL: time.Hour})
			if err != nil {
				t.Fatalf("issue: %v", err)
			}
			t.Cleanup(func() { revoke(t, driver, key) })
			keyed := cfg.Keyed(machine.StaticKey(key))
			spec := machine.InstanceSpec{Name: fmt.Sprintf("conformance-%s-keyed", run), From: &from}
			if inst, err := keyed.Create(ctx, spec); !errors.Is(err, machine.ErrUnauthorized) {
				if err == nil {
					remove(t, driver, inst)
				}
				t.Fatalf("a key the driver issued made a VM from pool %s's: %v, want ErrUnauthorized", other, err)
			}
			spec = copied
			spec.Name = fmt.Sprintf("conformance-%s-keyed", run)
			spec.Forwards = []machine.Forward{{Port: 7402, Pool: other, Instance: "peer", InstancePort: guest.AgentPort}}
			if inst, err := keyed.Create(ctx, spec); !errors.Is(err, machine.ErrUnauthorized) {
				if err == nil {
					remove(t, driver, inst)
				}
				t.Fatalf("a key the driver issued forwarded to pool %s: %v, want ErrUnauthorized", other, err)
			}
			if caps.Attaches(machine.VolumeData) {
				spec := copied
				spec.Data = &machine.DataVolume{}
				inst := create(t, spec)
				if _, err := keyed.Upgrade(ctx, inst, &from); !errors.Is(err, machine.ErrUnauthorized) {
					t.Fatalf("a key the driver issued upgraded a VM to pool %s's: %v, want ErrUnauthorized", other, err)
				}
			}
		}
		if caps.Attaches(machine.VolumeData) {
			spec := copied
			spec.Data = &machine.DataVolume{}
			inst := create(t, spec)
			upgraded, err := driver.Upgrade(ctx, inst, &from)
			if err != nil || upgraded.From != from.Name || upgraded.FromPool != other {
				t.Fatalf("an upgrade to pool %s's committed VM: %+v, %v", other, upgraded, err)
			}
		}
		spec := machine.InstanceSpec{From: &from}
		var peer machine.Instance
		if caps.Ports && cfg.GuestAgent != "" {
			peer, err = od.Create(ctx, machine.InstanceSpec{Name: "peer", From: &from, CPUs: cfg.CPUs, Memory: cfg.Memory})
			if err != nil {
				t.Fatalf("a peer of pool %s: %v", other, err)
			}
			t.Cleanup(func() { remove(t, od, peer) })
			spec.Forwards = []machine.Forward{{Port: 7402, Pool: other, Instance: peer.Name, InstancePort: guest.AgentPort}}
		}
		inst := create(t, spec)
		if inst.From != from.Name || inst.FromPool != other || inst.Pool == other {
			t.Fatalf("a VM from pool %s's %+v is %+v", other, from, inst)
		}
		if found, err := driver.Instance(ctx, inst.Name); err != nil || found.FromPool != other || len(found.Forwards) != len(spec.Forwards) || len(spec.Forwards) == 1 && found.Forwards[0] != spec.Forwards[0] {
			t.Fatalf("a VM from pool %s's, forwarding %+v, is found %+v: %v", other, spec.Forwards, found, err)
		}
		startCtx, cancel := context.WithTimeout(ctx, cfg.Timeout)
		m, err := driver.Start(startCtx, inst, machine.BootOptions{})
		cancel()
		if err != nil {
			t.Fatalf("start %s: %v", inst.Name, err)
		}
		t.Cleanup(func() { _ = m.Kill(context.Background()) })
		reachable(t, m)
		if got := readMarker(t, m, marker+"/pool.txt"); got != proof {
			t.Fatalf("a VM from pool %s's committed VM reads %q, want its proof", other, got)
		}
		if len(spec.Forwards) == 0 {
			t.Skip("the driver reaches no guest port, or no GuestAgent to dial the host from inside the guest")
		}
		startCtx, cancel = context.WithTimeout(ctx, cfg.Timeout)
		pm, err := od.Start(startCtx, peer, machine.BootOptions{})
		cancel()
		if err != nil {
			t.Fatalf("start %s of pool %s: %v", peer.Name, other, err)
		}
		t.Cleanup(func() { _ = pm.Kill(context.Background()) })
		reachable(t, pm)
		request := "GET /v1/files?path=" + marker + "/pool.txt HTTP/1.0\r\nHost: guest\r\n\r\n"
		argv := []string{"/bin/sh", "-c", `printf "$1" | "$0" dial-host 7402`, cfg.GuestAgent, strings.ReplaceAll(request, "\r\n", `\r\n`)}
		if guestOS == machine.Windows {
			argv = []string{"powershell", "-NoProfile", "-Command", "\"" + strings.ReplaceAll(request, "\r\n", "`r`n") + "\" | & '" + cfg.GuestAgent + "' dial-host 7402"}
		}
		var out bytes.Buffer
		if _, err := machine.Run(ctx, m, machine.ExecRequest{Argv: argv}, nil, &out, &out); err != nil {
			t.Fatalf("dial-host through the forward: %v\n%s", err, out.String())
		}
		if !strings.Contains(out.String(), proof) {
			t.Fatalf("pool %s's peer did not answer through the forward with its file; the guest read %q", other, out.String())
		}
	})

	// A started machine outlives the call that started it: another call
	// attaches to it, its service has an ingress URL this process connects
	// to where the driver gives one, and a pool URL that is an HTTP URL or
	// the ingress one where it has any (ADR 26-11-14-610), and a guest that
	// powers itself off while nothing watches is found stopped.
	t.Run("start-attach-and-stop", func(t *testing.T) {
		inst := create(t, start)
		startCtx, cancel := context.WithTimeout(ctx, cfg.Timeout)
		defer cancel()
		started, err := driver.Start(startCtx, inst, machine.BootOptions{})
		if err != nil {
			t.Fatalf("start: %v", err)
		}
		t.Cleanup(func() { _ = started.Kill(context.Background()) })
		reachable(t, started)
		wantState(t, driver, inst, machine.StateRunning, "started")
		m, err := driver.Attach(ctx, inst)
		if err != nil {
			t.Fatalf("attach to a running instance: %v", err)
		}
		if info, err := m.Info(ctx); err != nil || info.OS != string(guestOS) {
			t.Fatalf("info through an attached machine: %+v, %v", info, err)
		}
		if listen, err := driver.Listen(ctx, inst); err != nil || listen == "" {
			t.Fatalf("where a running instance's service listens: %q, %v", listen, err)
		}
		service, err := driver.Service(ctx, inst)
		if err != nil {
			t.Fatalf("service of a running instance: %+v, %v", service, err)
		}
		checkServiceURLs(ctx, t, caps.URLs, service, !cfg.UnreachedIngress && !caps.URLs.Credential)
		if err := m.Shutdown(ctx, false); err != nil {
			t.Fatalf("shutdown request: %v", err)
		}
		deadline := time.Now().Add(cfg.Timeout)
		for {
			_, err := driver.Attach(ctx, inst)
			if errors.Is(err, machine.ErrNotRunning) {
				break
			}
			if time.Now().After(deadline) {
				t.Fatalf("a guest that powered itself off still attaches: %v", err)
			}
			time.Sleep(time.Second)
		}
		wantState(t, driver, inst, machine.StateStopped, "shut down")
	})

	// A create asked to start returns its VM running (ADR 26-11-23-458 §6):
	// attached, its guest running a command once reachable, stopped in
	// order, and then stopped; fresh, where the driver has a base, and a
	// copy of the committed VM, carrying its write, where it commits.
	t.Run("created-started", func(t *testing.T) {
		specs := map[string]machine.InstanceSpec{"fresh": {}}
		if caps.Commit {
			specs["copied"] = of(committed)
		}
		for which, spec := range specs {
			t.Run(which, func(t *testing.T) {
				spec.Name = newName()
				spec.CPUs, spec.Memory = cfg.CPUs, cfg.Memory
				spec.Start = &machine.BootOptions{}
				createCtx, cancel := context.WithTimeout(ctx, cfg.Timeout)
				defer cancel()
				inst, err := driver.Create(createCtx, spec)
				if which == "fresh" && errors.Is(err, machine.ErrUnsupported) {
					t.Skipf("the driver has no base: %v", err)
				}
				if err != nil {
					t.Fatalf("create %s started: %v", spec.Name, err)
				}
				t.Cleanup(func() {
					if m, err := driver.Attach(context.Background(), inst); err == nil {
						_ = m.Kill(context.Background())
					}
					remove(t, driver, inst)
				})
				wantState(t, driver, inst, machine.StateRunning, "created started")
				m, err := driver.Attach(ctx, inst)
				if err != nil {
					t.Fatalf("attach to a VM created started: %v", err)
				}
				reachable(t, m)
				argv := []string{"/bin/sh", "-c", "echo started"}
				if inst.GuestOS == machine.Windows {
					argv = []string{"cmd", "/c", "echo started"}
				}
				if out := guestOutput(t, m, argv); out != "started" {
					t.Fatalf("a command in a VM created started printed %q", out)
				}
				if which == "copied" {
					checkExec(t, m, inst.GuestOS, marker)
				}
				shutdown(t, m)
				wantState(t, driver, inst, machine.StateStopped, "created started, then shut down")
			})
		}
	})

	// A fresh VM created started, fenced and with a data volume, returns
	// running with the volume mounted (asked of a linux guest), and reports the fence as asked once
	// created, once stopped, and booted again, when the write to the volume
	// is still there: a provider that mounts the volume at the create or a
	// boot may add to the fence for it, and that is not the fence's.
	t.Run("created-started-fenced-with-data", func(t *testing.T) {
		spec := machine.InstanceSpec{
			CPUs: cfg.CPUs, Memory: cfg.Memory, Start: &machine.BootOptions{},
			Data: &machine.DataVolume{}, Egress: &machine.Egress{Allow: []string{"example.com"}},
		}
		switch {
		case !caps.Fences(spec.Egress):
			t.Skip("the driver holds no list")
		case !caps.Attaches(machine.VolumeData):
			t.Skip("the driver attaches no data volume")
		}
		spec.Name = newName()
		createCtx, cancel := context.WithTimeout(ctx, cfg.Timeout)
		defer cancel()
		inst, err := driver.Create(createCtx, spec)
		if errors.Is(err, machine.ErrUnsupported) {
			t.Skipf("the driver has no base: %v", err)
		}
		if err != nil {
			t.Fatalf("create %s started, fenced, with a data volume: %v", spec.Name, err)
		}
		t.Cleanup(func() {
			if m, err := driver.Attach(context.Background(), inst); err == nil {
				_ = m.Kill(context.Background())
			}
			remove(t, driver, inst)
		})
		fenced := func(when string) {
			t.Helper()
			got, err := driver.Instance(ctx, inst.Name)
			if err != nil || got.Egress == nil || !slices.Equal(got.Egress.Allow, spec.Egress.Allow) {
				t.Fatalf("an instance fenced to %q %s reports %+v: %v", spec.Egress.Allow, when, got.Egress, err)
			}
			if got.Data == nil {
				t.Fatalf("an instance created with a data volume %s reports none", when)
			}
		}
		wantState(t, driver, inst, machine.StateRunning, "created started")
		fenced("once created started")
		m, err := driver.Attach(ctx, inst)
		if err != nil {
			t.Fatalf("attach to a VM created started: %v", err)
		}
		reachable(t, m)
		dir := machine.VolumePath(inst.GuestOS, machine.VolumeData)
		if inst.GuestOS == machine.Linux && cfg.HostGuest == "" {
			// A write kept across a boot could be the root's: the volume
			// is a mount of its own.
			if out := guestOutput(t, m, []string{"/bin/sh", "-c", "cut -d ' ' -f 2 /proc/mounts | grep -qxF " + dir + " && echo mounted"}); out != "mounted" {
				t.Fatalf("the data volume of a VM created started is not mounted at %s: %q", dir, out)
			}
		}
		if err := m.CopyTo(ctx, dir, tarOf(t, "data.txt", "kept")); err != nil {
			t.Fatalf("write to the data volume of a VM created started: %v", err)
		}
		shutdown(t, m)
		fenced("once it stops")
		m = boot(t, inst)
		fenced("booted again")
		if got := readMarker(t, m, dir+"/data.txt"); got != "kept" {
			t.Fatalf("the data volume of a VM created started reads %q after a stop and a boot", got)
		}
		shutdown(t, m)
	})

	// A create asked to start whose options its start refuses, a window on
	// a driver with no display, is refused, and makes nothing.
	t.Run("created-started-refused", func(t *testing.T) {
		if caps.Display {
			t.Skip("the driver shows a display")
		}
		spec := start
		spec.Name = newName()
		spec.CPUs, spec.Memory = cfg.CPUs, cfg.Memory
		spec.Start = &machine.BootOptions{GUI: true}
		if inst, err := driver.Create(ctx, spec); !errors.Is(err, machine.ErrUnsupported) {
			if err == nil {
				remove(t, driver, inst)
			}
			t.Fatalf("a create started with a window on a driver with no display: %v, want ErrUnsupported", err)
		}
		if _, err := driver.Instance(ctx, spec.Name); !errors.Is(err, machine.ErrNotFound) {
			t.Fatalf("a refused create left %s: %v, want ErrNotFound", spec.Name, err)
		}
	})

	// An instance its provider paused reads as paused, however often it is
	// read, attaches to nothing and boots nothing (ErrPaused), and Start
	// alone resumes it, its guest answering; paused again, Destroy removes
	// it (discobox-ai/vm ADR 26-11-21-438 §§3, 5, 6).
	t.Run("paused-read-and-resumed", func(t *testing.T) {
		if cfg.Pause == nil {
			t.Skip("the driver cannot be made to pause an instance")
		}
		inst := create(t, start)
		startCtx, cancel := context.WithTimeout(ctx, cfg.Timeout)
		defer cancel()
		started, err := driver.Start(startCtx, inst, machine.BootOptions{})
		if err != nil {
			t.Fatalf("start: %v", err)
		}
		t.Cleanup(func() { _ = started.Kill(context.Background()) })
		reachable(t, started)
		if err := cfg.Pause(ctx, inst); err != nil {
			t.Fatalf("pause: %v", err)
		}
		wantState(t, driver, inst, machine.StatePaused, "paused")
		wantState(t, driver, inst, machine.StatePaused, "paused, read again")
		if _, err := driver.Attach(ctx, inst); !errors.Is(err, machine.ErrPaused) || errors.Is(err, machine.ErrNotRunning) {
			t.Fatalf("attach to a paused instance: %v, want ErrPaused, not ErrNotRunning", err)
		}
		if m, err := driver.Boot(startCtx, inst, machine.BootOptions{}); !errors.Is(err, machine.ErrPaused) {
			if err == nil {
				_ = m.Kill(context.Background())
			}
			t.Fatalf("boot a paused instance: %v, want ErrPaused", err)
		}
		wantState(t, driver, inst, machine.StatePaused, "refused an attach and a boot")
		resumed, err := driver.Start(startCtx, inst, machine.BootOptions{})
		if err != nil {
			t.Fatalf("start a paused instance: %v", err)
		}
		wantState(t, driver, inst, machine.StateRunning, "resumed")
		reachable(t, resumed)
		if info, err := resumed.Info(ctx); err != nil || info.OS != string(guestOS) {
			t.Fatalf("info of a resumed instance: %+v, %v", info, err)
		}

		if err := cfg.Pause(ctx, inst); err != nil {
			t.Fatalf("pause again: %v", err)
		}
		wantState(t, driver, inst, machine.StatePaused, "paused again")
		if err := driver.Destroy(ctx, inst); err != nil {
			t.Fatalf("destroy a paused instance: %v", err)
		}
		if _, err := driver.Instance(ctx, inst.Name); !errors.Is(err, machine.ErrNotFound) {
			t.Fatalf("a paused instance destroyed: %v, want ErrNotFound", err)
		}
	})

	// A driver that reports Capabilities.Pause pauses a running machine
	// whole when asked: it reads paused, attaches to nothing, and a second
	// pause does nothing. Held paused, then resumed by Start, its processes
	// run on under the same PIDs and its clock is the host's. A paused
	// machine holds its slot under MaxRunning, is stopped by resuming it and
	// shutting it down, and is removed by Destroy (discobox-ai/vm ADR
	// 26-11-21-438 §8).
	t.Run("pause-and-resume", func(t *testing.T) {
		if !caps.Pause {
			t.Skip("the driver does not pause a machine")
		}
		inst := create(t, start)
		// Each start has a timeout of its own, since the hold outlasts one.
		startOf := func() (machine.Machine, error) {
			startCtx, cancel := context.WithTimeout(ctx, cfg.Timeout)
			defer cancel()
			return driver.Start(startCtx, inst, machine.BootOptions{})
		}
		m, err := startOf()
		if err != nil {
			t.Fatalf("start: %v", err)
		}
		t.Cleanup(func() { _ = m.Kill(context.Background()) })
		reachable(t, m)
		pid := startSleeper(t, m, guestOS)
		if err := m.Pause(ctx); err != nil {
			t.Fatalf("pause: %v", err)
		}
		wantState(t, driver, inst, machine.StatePaused, "paused")
		if _, err := driver.Attach(ctx, inst); !errors.Is(err, machine.ErrPaused) {
			t.Fatalf("attach to a paused instance: %v, want ErrPaused", err)
		}
		if err := m.Pause(ctx); err != nil {
			t.Fatalf("pause a paused instance: %v, want nil", err)
		}
		select {
		case <-m.Done():
			t.Fatalf("a paused machine reads as stopped: %v", m.Err())
		case <-time.After(cfg.PauseHold):
		}
		wantState(t, driver, inst, machine.StatePaused, "held paused")
		resumed, err := startOf()
		if err != nil {
			t.Fatalf("start a paused instance: %v", err)
		}
		wantState(t, driver, inst, machine.StateRunning, "resumed")
		reachable(t, resumed)
		if !sleeperRuns(t, resumed, guestOS, pid) {
			t.Fatalf("process %s did not survive a pause and a resume", pid)
		}
		checkClock(t, resumed, guestOS)
		stopSleeper(t, resumed, guestOS, pid)

		// Stopped while paused, as the engine stops one: resumed, then shut
		// down in order. It boots again.
		if err := resumed.Pause(ctx); err != nil {
			t.Fatalf("pause again: %v", err)
		}
		wantState(t, driver, inst, machine.StatePaused, "paused again")
		toStop, err := startOf()
		if err != nil {
			t.Fatalf("resume to stop: %v", err)
		}
		reachable(t, toStop)
		shutdown(t, toStop)
		wantState(t, driver, inst, machine.StateStopped, "paused, then stopped")
		again, err := startOf()
		if err != nil {
			t.Fatalf("start an instance stopped while paused: %v", err)
		}
		t.Cleanup(func() { _ = again.Kill(context.Background()) })
		reachable(t, again)

		// Paused, it is destroyed whole.
		if err := again.Pause(ctx); err != nil {
			t.Fatalf("pause to destroy: %v", err)
		}
		if err := driver.Destroy(ctx, inst); err != nil {
			t.Fatalf("destroy a paused instance: %v", err)
		}
		if _, err := driver.Instance(ctx, inst.Name); !errors.Is(err, machine.ErrNotFound) {
			t.Fatalf("a paused instance destroyed: %v, want ErrNotFound", err)
		}
	})

	// A paused machine keeps its memory on its host, so it holds its slot
	// under MaxRunning.
	t.Run("a-paused-machine-holds-its-slot", func(t *testing.T) {
		limit := caps.MaxRunning[guestOS]
		switch {
		case !caps.Pause:
			t.Skip("the driver does not pause a machine")
		case limit == 0:
			t.Skipf("no cap on %s guests", guestOS)
		}
		var running []machine.Machine
		for range limit {
			running = append(running, boot(t, create(t, start)))
		}
		if err := running[0].Pause(ctx); err != nil {
			t.Fatalf("pause: %v", err)
		}
		extra := create(t, start)
		if m, err := driver.Start(ctx, extra, machine.BootOptions{}); !errors.Is(err, machine.ErrCapacity) {
			if err == nil {
				_ = m.Kill(context.Background())
			}
			t.Fatalf("start past a cap of %d %s guests, one paused: %v, want ErrCapacity", limit, guestOS, err)
		}
	})

	// A driver that does not report Capabilities.Pause refuses a pause, and
	// its machine runs on.
	t.Run("pause-refused", func(t *testing.T) {
		if caps.Pause {
			t.Skip("the driver pauses a machine")
		}
		inst := create(t, start)
		m := boot(t, inst)
		if err := m.Pause(ctx); !errors.Is(err, machine.ErrUnsupported) {
			t.Fatalf("pause on a driver without Capabilities.Pause: %v, want ErrUnsupported", err)
		}
		reachable(t, m)
		if _, err := m.Info(ctx); err != nil {
			t.Fatalf("a machine refused a pause: %v, want it running", err)
		}
	})

	// A strict URL request is met or refused: a URL the driver never gives
	// asked on, or always gives asked off, is refused, and an ingress URL
	// it gives when asked, asked off, is not there.
	t.Run("url-requests", func(t *testing.T) {
		for _, scope := range machine.URLScopes {
			reach := caps.URLs.Of(scope)
			for _, mode := range []machine.URLMode{machine.URLOn, machine.URLOff} {
				if (mode == machine.URLOn && reach != machine.URLNever) || (mode == machine.URLOff && reach != machine.URLAlways) {
					continue
				}
				spec := start
				spec.Name, spec.CPUs, spec.Memory = newName(), cfg.CPUs, cfg.Memory
				req := machine.URLRequest{Mode: mode, Strict: true}
				if scope == machine.URLPool {
					spec.URLs.Pool = req
				} else {
					spec.URLs.Ingress = req
				}
				inst, err := driver.Create(ctx, spec)
				if err == nil {
					remove(t, driver, inst)
				}
				if !errors.Is(err, machine.ErrUnsupported) {
					t.Fatalf("a strict %s URL %s on a driver whose reach is %q: %v, want ErrUnsupported", scope, mode, reach, err)
				}
			}
		}
		if caps.URLs.Ingress != machine.URLOptional {
			return
		}
		spec := start
		spec.URLs.Ingress = machine.URLRequest{Mode: machine.URLOff, Strict: true}
		inst := create(t, spec)
		startCtx, cancel := context.WithTimeout(ctx, cfg.Timeout)
		defer cancel()
		started, err := driver.Start(startCtx, inst, machine.BootOptions{})
		if err != nil {
			t.Fatalf("start: %v", err)
		}
		t.Cleanup(func() { _ = started.Kill(context.Background()) })
		reachable(t, started)
		service, err := driver.Service(ctx, inst)
		if err != nil {
			t.Fatalf("service of a running instance: %v", err)
		}
		if service.IngressURL != "" {
			t.Fatalf("an instance whose ingress URL is asked off has service %+v, with an ingress URL", service)
		}
		// A driver with networks gives a peer on none no URL but the
		// ingress one (boxd); any other always giving a pool URL keeps it.
		if caps.URLs.Pool == machine.URLAlways && !caps.Networks && service.PoolURL == "" {
			t.Fatalf("an instance whose ingress URL is asked off has service %+v, with no pool URL, which the driver always gives", service)
		}
	})

	// A key the driver issues runs the pool's machines from another
	// driver, cannot issue a key, and works no more once revoked. A driver
	// that issues none says so with ErrNoKey or ErrCannotIssue.
	t.Run("an-issued-key", func(t *testing.T) {
		const ttl = time.Hour
		key, err := driver.IssueKey(ctx, machine.IssueRequest{Name: "conformance-" + run, TTL: ttl})
		if caps.Keys.Mode != machine.KeysIssue {
			if !errors.Is(err, machine.ErrNoKey) && !errors.Is(err, machine.ErrCannotIssue) {
				t.Fatalf("a driver that issues no keys issued %v: %v, want ErrNoKey or ErrCannotIssue", key, err)
			}
			return
		}
		if errors.Is(err, machine.ErrCannotIssue) {
			t.Skipf("the driver's key cannot issue keys: %v", err)
		}
		if err != nil {
			t.Fatalf("issue: %v", err)
		}
		t.Cleanup(func() { revoke(t, driver, key) })
		if !key.Issued || key.Secret == "" || key.ID == "" || key.Scope != caps.Keys.Scope {
			t.Fatalf("issued %v (issued %t, a secret %t), want an issued key with a secret, an ID, and scope %s", key, key.Issued, key.Secret != "", caps.Keys.Scope)
		}
		if key.Expires.IsZero() || key.Expires.After(time.Now().Add(ttl+time.Minute)) {
			t.Fatalf("issued %v for %s", key, ttl)
		}
		if strings.Contains(fmt.Sprintf("%v %+v %#v %s", key, key, key, key), key.Secret) {
			t.Fatal("a key prints its secret")
		}
		listed, err := driver.IssuedKeys(ctx, key.Name)
		if err != nil {
			t.Fatalf("issued keys: %v", err)
		}
		if len(listed) != 1 || listed[0].ID != key.ID || listed[0].Secret != "" {
			t.Fatalf("issued keys named %s: %v, want %s alone, without its secret", key.Name, listed, key.ID)
		}
		if again, err := driver.IssueKey(ctx, machine.IssueRequest{Name: key.Name, TTL: ttl}); !errors.Is(err, machine.ErrExists) {
			if err == nil {
				revoke(t, driver, again)
			}
			t.Fatalf("a second key named %s: %v, want ErrExists", key.Name, err)
		}
		if cfg.Keyed == nil {
			t.Fatal("the driver issues keys, and Config.Keyed is nil")
		}
		keyed := cfg.Keyed(machine.StaticKey(key))
		if again, err := keyed.IssueKey(ctx, machine.IssueRequest{Name: "conformance-" + run + "-again", TTL: ttl}); err == nil {
			revoke(t, driver, again)
			t.Fatal("an issued key issued another")
		}

		spec := start
		names++
		spec.Name = fmt.Sprintf("conformance-%s-%d", run, names)
		spec.CPUs, spec.Memory = cfg.CPUs, cfg.Memory
		inst, err := keyed.Create(ctx, spec)
		if err != nil {
			t.Fatalf("create with the issued key: %v", err)
		}
		t.Cleanup(func() { remove(t, driver, inst) })
		startCtx, cancel := context.WithTimeout(ctx, cfg.Timeout)
		defer cancel()
		m, err := keyed.Start(startCtx, inst, machine.BootOptions{})
		if err != nil {
			t.Fatalf("start with the issued key: %v", err)
		}
		t.Cleanup(func() { _ = m.Kill(context.Background()) })
		reachable(t, m)
		if err := m.CopyTo(ctx, marker, tarOf(t, "keyed.txt", "keyed")); err != nil {
			t.Fatalf("copy in with the issued key: %v", err)
		}
		if got := readMarker(t, m, marker+"/keyed.txt"); got != "keyed" {
			t.Fatalf("read back %q", got)
		}
		shutdown(t, m)
		if err := keyed.Destroy(ctx, inst); err != nil {
			t.Fatalf("remove with the issued key: %v", err)
		}

		if err := driver.RevokeKey(ctx, key); err != nil {
			t.Fatalf("revoke: %v", err)
		}
		if listed, err := driver.IssuedKeys(ctx, key.Name); err != nil || len(listed) != 0 {
			t.Fatalf("issued keys after revoking: %v, %v", listed, err)
		}
		if _, err := keyed.Instances(ctx); !errors.Is(err, machine.ErrUnauthorized) {
			t.Fatalf("a revoked key listing instances: %v, want ErrUnauthorized", err)
		}
		spec.Name += "-revoked"
		if inst, err := keyed.Create(ctx, spec); err == nil {
			remove(t, driver, inst)
			t.Fatal("a revoked key creates an instance")
		}
		if err := driver.RevokeKey(ctx, key); err != nil {
			t.Fatalf("revoking a key already revoked: %v", err)
		}
	})

	// A data volume is the instance's: empty at Create, kept across a stop
	// and a boot, gone with the instance, and never in a committed VM, where
	// the driver commits. A driver that attaches none refuses it.
	t.Run("a-data-volume", func(t *testing.T) {
		spec := start
		spec.Data = &machine.DataVolume{}
		if !caps.Attaches(machine.VolumeData) {
			spec.Name = fmt.Sprintf("conformance-%s-data", run)
			if inst, err := driver.Create(ctx, spec); !errors.Is(err, machine.ErrUnsupported) {
				if err == nil {
					remove(t, driver, inst)
				}
				t.Fatalf("a driver that attaches no data volume made an instance with one: %v, want ErrUnsupported", err)
			}
			t.Skip("the driver attaches no data volume")
		}
		dir := machine.VolumePath(guestOS, machine.VolumeData)
		inst := create(t, spec)
		if got, err := driver.Instance(ctx, inst.Name); err != nil || got.Data == nil {
			t.Fatalf("an instance with a data volume reports %+v: %v", got.Data, err)
		}
		m := boot(t, inst)
		if err := m.CopyTo(ctx, dir, tarOf(t, "data.txt", "kept")); err != nil {
			t.Fatalf("write to the data volume: %v", err)
		}
		shutdown(t, m)
		m = boot(t, inst)
		if got := readMarker(t, m, dir+"/data.txt"); got != "kept" {
			t.Fatalf("the data volume reads %q after a stop and a boot", got)
		}
		shutdown(t, m)
		gone := start
		gone.Name, gone.Data = fmt.Sprintf("conformance-%s-data-gone", run), &machine.DataVolume{}
		destroyed, err := driver.Create(ctx, gone)
		if err != nil {
			t.Fatalf("create %s: %v", gone.Name, err)
		}
		t.Cleanup(func() { remove(t, driver, destroyed) })
		m = boot(t, destroyed)
		if err := m.CopyTo(ctx, dir, tarOf(t, "data.txt", "destroyed")); err != nil {
			t.Fatalf("write to the data volume: %v", err)
		}
		shutdown(t, m)
		if err := driver.Destroy(ctx, destroyed); err != nil {
			t.Fatalf("destroy %s: %v", gone.Name, err)
		}
		reborn, err := driver.Create(ctx, gone)
		if err != nil {
			t.Fatalf("create %s again: %v", gone.Name, err)
		}
		t.Cleanup(func() { remove(t, driver, reborn) })
		m = boot(t, reborn)
		absent(t, m, dir+"/data.txt", "a new instance of a destroyed one's name")
		shutdown(t, m)
		if !caps.Commit {
			return
		}
		img := commit(t, inst)
		again := of(img)
		again.Data = &machine.DataVolume{}
		m = boot(t, create(t, again))
		absent(t, m, dir+"/data.txt", "the data volume of a VM created from a committed one")
		shutdown(t, m)
		bare := create(t, of(img))
		if got, err := driver.Instance(ctx, bare.Name); err != nil || got.Data != nil {
			t.Fatalf("a VM created from a committed one, with no volume, reports data %+v: %v", got.Data, err)
		}
		m = boot(t, bare)
		absent(t, m, dir+"/data.txt", "a committed VM")
	})

	// A cache volume is a private clone of its key's current generation,
	// which only a save moves: a write is seen by no live peer and by no
	// later instance until it is saved; a guarded save is refused once the
	// key has moved; another key sees nothing; and an instance keeps its
	// clone once its key is removed; and a committed VM has no cache, where
	// the driver commits. A driver that attaches none refuses it.
	t.Run("a-cache-volume", func(t *testing.T) {
		key, other := "conformance-"+run, "conformance-"+run+"-other"
		withCache := func(key string) machine.InstanceSpec {
			spec := start
			spec.Cache = &machine.CacheVolume{Key: key}
			return spec
		}
		if !caps.Attaches(machine.VolumeCache) {
			spec := withCache(key)
			spec.Name = fmt.Sprintf("conformance-%s-cache", run)
			if inst, err := driver.Create(ctx, spec); !errors.Is(err, machine.ErrUnsupported) {
				if err == nil {
					remove(t, driver, inst)
				}
				t.Fatalf("a driver that attaches no cache volume made an instance with one: %v, want ErrUnsupported", err)
			}
			if caches, err := driver.Caches(ctx); err != nil || len(caches) != 0 {
				t.Fatalf("a driver that attaches no cache volume lists caches %+v: %v", caches, err)
			}
			if err := driver.DeleteCache(ctx, key); !errors.Is(err, machine.ErrUnsupported) {
				t.Fatalf("a driver that attaches no cache volume deletes one: %v, want ErrUnsupported", err)
			}
			t.Skip("the driver attaches no cache volume")
		}
		t.Cleanup(func() {
			deleteCache(t, driver, key)
			deleteCache(t, driver, other)
		})
		dir := machine.VolumePath(guestOS, machine.VolumeCache)
		cached := func(t *testing.T, key string) machine.Instance {
			t.Helper()
			inst := create(t, withCache(key))
			got, err := driver.Instance(ctx, inst.Name)
			if err != nil || got.Cache == nil || got.Cache.Key != key {
				t.Fatalf("an instance with cache %s reports %+v: %v", key, got.Cache, err)
			}
			return got
		}

		first := cached(t, key)
		if first.Cache.Generation != "" {
			t.Fatalf("an instance of a key never saved was cloned from generation %q", first.Cache.Generation)
		}
		m := boot(t, first)
		if err := m.CopyTo(ctx, dir, tarOf(t, "cache.txt", "first")); err != nil {
			t.Fatalf("write to the cache volume: %v", err)
		}
		peer := boot(t, cached(t, key))
		absent(t, peer, dir+"/cache.txt", "a live peer's cache volume")
		shutdown(t, peer)
		shutdown(t, m)
		if _, err := driver.SaveCache(ctx, first, machine.CacheSave{Key: key, Guarded: true, IfGeneration: "x"}); !errors.Is(err, machine.ErrCacheMoved) {
			t.Fatalf("a save guarded on a generation the key never held: %v, want ErrCacheMoved", err)
		}
		saved, err := driver.SaveCache(ctx, first, machine.CacheSave{Key: key, Guarded: true})
		if err != nil || saved.Key != key || saved.Generation == "" {
			t.Fatalf("save the first instance's cache: %+v, %v", saved, err)
		}
		if _, err := driver.SaveCache(ctx, first, machine.CacheSave{Key: key, Guarded: true}); !errors.Is(err, machine.ErrCacheMoved) {
			t.Fatalf("a save guarded on no generation once the key holds one: %v, want ErrCacheMoved", err)
		}
		caches, err := driver.Caches(ctx)
		if err != nil || !slices.ContainsFunc(caches, func(c machine.Cache) bool { return c.Key == key && c.Generation == saved.Generation }) {
			t.Fatalf("caches %+v do not list %s at %s: %v", caches, key, saved.Generation, err)
		}

		second := cached(t, key)
		if second.Cache.Generation != saved.Generation {
			t.Fatalf("an instance of %s was cloned from %q, want %q", key, second.Cache.Generation, saved.Generation)
		}
		m = boot(t, second)
		if got := readMarker(t, m, dir+"/cache.txt"); got != "first" {
			t.Fatalf("a clone of the saved generation reads %q", got)
		}
		if err := m.CopyTo(ctx, dir, tarOf(t, "cache.txt", "second")); err != nil {
			t.Fatalf("write to the cache volume: %v", err)
		}
		shutdown(t, m)
		m = boot(t, cached(t, other))
		absent(t, m, dir+"/cache.txt", "another key's cache volume")
		shutdown(t, m)

		if err := driver.DeleteCache(ctx, key); err != nil {
			t.Fatalf("delete cache %s: %v", key, err)
		}
		if err := driver.DeleteCache(ctx, key); err != nil {
			t.Fatalf("delete cache %s again: %v", key, err)
		}
		caches, err = driver.Caches(ctx)
		if err != nil || slices.ContainsFunc(caches, func(c machine.Cache) bool { return c.Key == key }) {
			t.Fatalf("caches %+v still list %s once it is deleted: %v", caches, key, err)
		}
		m = boot(t, second)
		if got := readMarker(t, m, dir+"/cache.txt"); got != "second" {
			t.Fatalf("an instance's cache volume reads %q once its key is deleted", got)
		}
		shutdown(t, m)
		saved, err = driver.SaveCache(ctx, second, machine.CacheSave{Key: other})
		if err != nil || saved.Key != other {
			t.Fatalf("save an instance's cache to another key: %+v, %v", saved, err)
		}
		m = boot(t, cached(t, other))
		if got := readMarker(t, m, dir+"/cache.txt"); got != "second" {
			t.Fatalf("a clone of %s reads %q", other, got)
		}
		shutdown(t, m)
		if !caps.Commit {
			return
		}
		img := commit(t, second)
		bare := create(t, of(img))
		if got, err := driver.Instance(ctx, bare.Name); err != nil || got.Cache != nil || got.Data != nil {
			t.Fatalf("a VM created from a committed one, with no volume, reports cache %+v and data %+v: %v", got.Cache, got.Data, err)
		}
		m = boot(t, bare)
		absent(t, m, dir+"/cache.txt", "a committed VM that had a cache volume")
	})

	// An upgrade gives a stopped VM a new root, a copy of a committed VM or
	// a fresh one from the base, and keeps its name, its settings, its data
	// volume, and its cache clone: what it wrote to its root is gone, and
	// what it wrote to its volumes is not. It is refused while the VM runs,
	// for a committed VM the provider does not have, and from one of another
	// guest OS, changing nothing. A driver that does not commit upgrades
	// only to a fresh root, and refuses every from with ErrUnsupported (ADR
	// 26-11-03-868). A driver that attaches no data volume refuses to
	// upgrade a VM with none, which every VM it makes is (a VM made with one
	// before, which it upgrades, is its own test's: ADR 26-11-28-860 §3).
	t.Run("an-upgrade", func(t *testing.T) {
		// to is the committed VM the suite upgrades to, where the driver
		// commits; else every upgrade is to a fresh root.
		var to *machine.Instance
		if caps.Commit {
			to = &committed
		}
		if !caps.Attaches(machine.VolumeData) {
			inst := create(t, copied)
			if _, err := driver.Upgrade(ctx, inst, to); !errors.Is(err, machine.ErrUnsupported) {
				t.Fatalf("a driver that attaches no data volume upgraded a VM: %v, want ErrUnsupported", err)
			}
			if got, err := driver.Instance(ctx, inst.Name); err != nil || got.ID != inst.ID {
				t.Fatalf("a refused upgrade changed the VM: %+v, %v; was %+v", got, err, inst)
			}
			t.Skip("the driver attaches no data volume")
		}
		key := "conformance-" + run + "-upgrade"
		spec := copied
		spec.Data = &machine.DataVolume{}
		if caps.Attaches(machine.VolumeCache) {
			spec.Cache = &machine.CacheVolume{Key: key}
			t.Cleanup(func() { deleteCache(t, driver, key) })
		}
		data := machine.VolumePath(guestOS, machine.VolumeData)
		cache := machine.VolumePath(guestOS, machine.VolumeCache)
		before := create(t, spec)
		m := boot(t, before)
		if err := m.CopyTo(ctx, data, tarOf(t, "data.txt", "kept")); err != nil {
			t.Fatalf("write to the data volume: %v", err)
		}
		if spec.Cache != nil {
			if err := m.CopyTo(ctx, cache, tarOf(t, "cache.txt", "kept")); err != nil {
				t.Fatalf("write to the cache volume: %v", err)
			}
		}
		if err := m.CopyTo(ctx, marker, tarOf(t, "root.txt", "old root")); err != nil {
			t.Fatalf("write to the root: %v", err)
		}
		shutdown(t, m)
		// Started, as a caller's VM runs, so another call sees it running.
		startCtx, cancel := context.WithTimeout(ctx, cfg.Timeout)
		m, err := driver.Start(startCtx, before, machine.BootOptions{})
		cancel()
		if err != nil {
			t.Fatalf("start %s: %v", before.Name, err)
		}
		t.Cleanup(func() { _ = m.Kill(context.Background()) })
		if _, err := driver.Upgrade(ctx, before, to); err == nil {
			t.Fatal("upgraded a running VM")
		}
		shutdown(t, m)
		gone := machine.Instance{ID: "conformance-gone", Name: fmt.Sprintf("conformance-%s-gone", run), GuestOS: guestOS}
		if !caps.Commit {
			// A copy of a committed VM is what a driver that does not commit
			// never makes (ADR 26-11-03-868).
			if _, err := driver.Upgrade(ctx, before, &gone); !errors.Is(err, machine.ErrUnsupported) {
				t.Fatalf("an upgrade from a committed VM on a driver that does not commit: %v, want ErrUnsupported", err)
			}
		} else {
			if _, err := driver.Upgrade(ctx, before, &gone); !errors.Is(err, machine.ErrNotFound) {
				t.Fatalf("an upgrade from a committed VM the provider does not have: %v, want ErrNotFound", err)
			}
			alien := gone
			alien.GuestOS = machine.Windows
			if guestOS == machine.Windows {
				alien.GuestOS = machine.Linux
			}
			if _, err := driver.Upgrade(ctx, before, &alien); err == nil {
				t.Fatalf("upgraded a %s VM from a %s one", guestOS, alien.GuestOS)
			}
		}
		if got, err := driver.Instance(ctx, before.Name); err != nil || got.ID != before.ID {
			t.Fatalf("a refused upgrade changed the VM: %+v, %v; was %+v", got, err, before)
		}

		// check finds the upgraded VM as it should be, then boots it, by
		// what Upgrade returned and by the Instance from before, which
		// still names it, and finds its volumes kept and its root new:
		// root is the committed VM's marker, or empty for a fresh root.
		check := func(t *testing.T, upgraded machine.Instance, from, root string) {
			t.Helper()
			if upgraded.Name != before.Name || upgraded.GuestOS != before.GuestOS || upgraded.CPUs != before.CPUs || upgraded.Memory != before.Memory {
				t.Fatalf("upgraded %+v from %+v", upgraded, before)
			}
			if upgraded.From != "" && upgraded.From != from {
				t.Fatalf("upgraded from %q, the VM reports it is from %q", from, upgraded.From)
			}
			if upgraded.Data == nil || (spec.Cache != nil && (upgraded.Cache == nil || *upgraded.Cache != *before.Cache)) {
				t.Fatalf("upgraded volumes %+v and %+v, were %+v and %+v", upgraded.Data, upgraded.Cache, before.Data, before.Cache)
			}
			if found, err := driver.Instance(ctx, before.Name); err != nil || found.ID != upgraded.ID {
				t.Fatalf("the upgraded VM is found as %+v, %v; upgraded %+v", found, err, upgraded)
			}
			if list, err := driver.Instances(ctx); err != nil || len(slices.DeleteFunc(list, func(i machine.Instance) bool { return i.Name != before.Name })) != 1 {
				t.Fatalf("the upgraded VM is listed as %+v: %v", list, err)
			}
			for _, inst := range []machine.Instance{upgraded, before} {
				m := boot(t, inst)
				if got := readMarker(t, m, data+"/data.txt"); got != "kept" {
					t.Fatalf("the data volume reads %q after an upgrade", got)
				}
				if spec.Cache != nil {
					if got := readMarker(t, m, cache+"/cache.txt"); got != "kept" {
						t.Fatalf("the cache volume reads %q after an upgrade", got)
					}
				}
				absent(t, m, marker+"/root.txt", "an upgraded VM's new root")
				if root == "" {
					absent(t, m, marker+"/proof.txt", "a fresh root")
				} else if got := readMarker(t, m, marker+"/proof.txt"); got != root {
					t.Fatalf("the new root reads %q, want %q from the committed VM", got, root)
				}
				shutdown(t, m)
			}
		}
		upgraded := before
		if caps.Commit {
			upgraded, err = driver.Upgrade(ctx, before, &committed)
			if err != nil {
				t.Fatalf("upgrade %s from %s: %v", before.Name, committed.Name, err)
			}
			check(t, upgraded, committed.Name, "committed")
		}
		switch fresh, err := driver.Upgrade(ctx, upgraded, nil); {
		case errors.Is(err, machine.ErrUnsupported) && start.From != nil:
			t.Logf("the driver has no base to upgrade to: %v", err)
		case err != nil:
			t.Fatalf("upgrade %s to a fresh root: %v", before.Name, err)
		default:
			check(t, fresh, "", "")
		}
	})

	// A committed copy of a committed VM keeps both writes, and outlives the
	// first's removal: a VM created from it still boots with both. A driver
	// whose copy still needs the other's disk refuses the removal with
	// ErrInUse instead. Last, since it removes the VM the others are made
	// from.
	t.Run("a-copy-of-a-copy", func(t *testing.T) {
		if !caps.Commit {
			t.Skip("the driver does not commit")
		}
		inst := create(t, of(committed))
		m := boot(t, inst)
		if err := m.CopyTo(ctx, marker, tarOf(t, "second.txt", "second")); err != nil {
			t.Fatalf("write marker: %v", err)
		}
		shutdown(t, m)
		second := commit(t, inst)
		switch err := driver.Destroy(ctx, committed); {
		case errors.Is(err, machine.ErrInUse):
			t.Logf("the driver keeps %s for %s: %v", committed.Name, second.Name, err)
		case err != nil:
			t.Fatalf("remove %s: %v", committed.Name, err)
		default:
			if _, err := driver.Instance(ctx, committed.Name); !errors.Is(err, machine.ErrNotFound) {
				t.Fatalf("a removed committed VM: %v, want ErrNotFound", err)
			}
		}
		m = boot(t, create(t, of(second)))
		if got := readMarker(t, m, marker+"/proof.txt"); got != "committed" {
			t.Fatalf("a VM of the second committed VM reads %q from the first", got)
		}
		if got := readMarker(t, m, marker+"/second.txt"); got != "second" {
			t.Fatalf("a VM of the second committed VM reads %q from its own", got)
		}
	})
}

// startSleeper starts a process in the guest that outlives the exec that
// started it, and returns its PID. It leaves the exec's process group where
// the guest has setsid, since a driver may end the group with the exec
// (archil), and returns only once the process runs sleep, past setsid's
// setsid(2), where the guest has /proc to tell.
func startSleeper(t *testing.T, g machine.Guest, guestOS machine.OS) string {
	t.Helper()
	argv := []string{"/bin/sh", "-c", `run=nohup; command -v setsid >/dev/null 2>&1 && run=setsid
$run sleep 3600 </dev/null >/dev/null 2>&1 &
pid=$!
i=0
while [ -d /proc/$pid ] && [ "$(cat /proc/$pid/comm 2>/dev/null)" != sleep ] && [ $i -lt 100 ]; do sleep 0.1; i=$((i+1)); done
echo $pid`}
	if guestOS == machine.Windows {
		argv = []string{"powershell.exe", "-NoProfile", "-NonInteractive", "-Command",
			"(Start-Process -PassThru -WindowStyle Hidden -FilePath ping.exe -ArgumentList '-n','3600','127.0.0.1').Id"}
	}
	out := guestOutput(t, g, argv)
	if _, err := strconv.Atoi(out); err != nil {
		t.Fatalf("start a process: %q is not a PID", out)
	}
	return out
}

// sleeperRuns reports whether the guest's process pid runs.
func sleeperRuns(t *testing.T, g machine.Guest, guestOS machine.OS, pid string) bool {
	t.Helper()
	argv := []string{"/bin/sh", "-c", `kill -0 "$1"`, "sh", pid}
	if guestOS == machine.Windows {
		argv = []string{"powershell.exe", "-NoProfile", "-NonInteractive", "-Command",
			"if (Get-Process -Id " + pid + " -ErrorAction SilentlyContinue) { exit 0 } else { exit 1 }"}
	}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	code, err := machine.Run(ctx, g, machine.ExecRequest{Argv: argv}, nil, io.Discard, io.Discard)
	if err != nil {
		t.Fatalf("look for process %s: %v", pid, err)
	}
	return code == 0
}

// stopSleeper ends the guest's process pid.
func stopSleeper(t *testing.T, g machine.Guest, guestOS machine.OS, pid string) {
	t.Helper()
	argv := []string{"/bin/sh", "-c", `kill "$1"`, "sh", pid}
	if guestOS == machine.Windows {
		argv = []string{"powershell.exe", "-NoProfile", "-NonInteractive", "-Command", "Stop-Process -Id " + pid}
	}
	guestOutput(t, g, argv)
}

// checkClock fails t unless the guest's wall clock is within five seconds
// of this host's (Capabilities.Pause).
func checkClock(t *testing.T, g machine.Guest, guestOS machine.OS) {
	t.Helper()
	argv := []string{"date", "-u", "+%s"}
	if guestOS == machine.Windows {
		argv = []string{"powershell.exe", "-NoProfile", "-NonInteractive", "-Command", "[DateTimeOffset]::UtcNow.ToUnixTimeSeconds()"}
	}
	before := time.Now()
	out := guestOutput(t, g, argv)
	after := time.Now()
	secs, err := strconv.ParseInt(out, 10, 64)
	if err != nil {
		t.Fatalf("the guest's clock: %q is not a time", out)
	}
	guest := time.Unix(secs, 0)
	// The guest read its clock between before and after, to the second.
	if guest.Before(before.Add(-6*time.Second)) || guest.After(after.Add(6*time.Second)) {
		t.Fatalf("the guest's clock reads %s, and this host's %s: more than five seconds apart", guest.UTC(), before.UTC())
	}
}

// guestOutput runs argv in the guest, wants it to exit 0, and returns its
// output, trimmed.
func guestOutput(t *testing.T, g machine.Guest, argv []string) string {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	var stdout, stderr bytes.Buffer
	code, err := machine.Run(ctx, g, machine.ExecRequest{Argv: argv}, nil, &stdout, &stderr)
	if err != nil || code != 0 {
		t.Fatalf("%s in the guest: exit %d, %v, stdout %q, stderr %q", argv[0], code, err, stdout.String(), stderr.String())
	}
	return strings.TrimSpace(stdout.String())
}

// wantState fails t unless the driver reads inst's state as want.
func wantState(t *testing.T, d machine.Driver, inst machine.Instance, want machine.State, when string) {
	t.Helper()
	state, err := d.State(context.Background(), inst)
	if err != nil || state != want {
		t.Fatalf("the state of an instance %s: %q, %v; want %q", when, state, err, want)
	}
}

// remove removes a VM the suite made, and fails t where the driver could
// not: a removal whose error is dropped leaves the VM in the pool unseen
// (vm#166). A VM already gone is removed.
func remove(t *testing.T, d machine.Driver, inst machine.Instance) {
	t.Helper()
	if err := d.Destroy(context.Background(), inst); err != nil && !errors.Is(err, machine.ErrNotFound) {
		t.Errorf("remove %s of pool %s: %v", inst.Name, inst.Pool, err)
	}
}

// deleteCache deletes a cache key the suite saved, and fails t where the
// driver could not.
func deleteCache(t *testing.T, d machine.Driver, key string) {
	t.Helper()
	if err := d.DeleteCache(context.Background(), key); err != nil && !errors.Is(err, machine.ErrNotFound) {
		t.Errorf("delete cache %s: %v", key, err)
	}
}

// revoke revokes a key the suite issued, and fails t where the driver
// could not.
func revoke(t *testing.T, d machine.Driver, key machine.Key) {
	t.Helper()
	if err := d.RevokeKey(context.Background(), key); err != nil && !errors.Is(err, machine.ErrNotFound) {
		t.Errorf("revoke key %s: %v", key.Name, err)
	}
}

// sweptPool is a pool the suite used, and which of its VMs are the run's.
type sweptPool struct {
	driver machine.Driver
	ours   func(name string) bool
}

// sweep fails t for every VM of the pools that is the run's, and removes
// it. A VM another was made from, in any of the pools, may be refused until
// that one is gone, so it removes in rounds across all of them until a
// round removes nothing.
func sweep(t *testing.T, pools []sweptPool) {
	t.Helper()
	type found struct {
		driver machine.Driver
		inst   machine.Instance
	}
	var left []found
	for _, p := range pools {
		list, err := p.driver.Instances(context.Background())
		if err != nil {
			t.Errorf("list a pool for VMs the suite left behind: %v", err)
			continue
		}
		for _, inst := range list {
			if p.ours(inst.Name) {
				left = append(left, found{p.driver, inst})
				t.Errorf("the suite left %s behind in pool %s", inst.Name, inst.Pool)
			}
		}
	}
	for len(left) > 0 {
		var kept []found
		var errs []error
		for _, f := range left {
			if err := f.driver.Destroy(context.Background(), f.inst); err != nil && !errors.Is(err, machine.ErrNotFound) {
				kept = append(kept, f)
				errs = append(errs, fmt.Errorf("%s of pool %s: %w", f.inst.Name, f.inst.Pool, err))
			}
		}
		if len(kept) == len(left) {
			t.Errorf("remove what the suite left behind: %v", errors.Join(errs...))
			return
		}
		left = kept
	}
}

// sweepCaches fails t for every cache key of d's pool that ours names, and
// deletes it.
func sweepCaches(t *testing.T, d machine.Driver, ours func(key string) bool) {
	t.Helper()
	caches, err := d.Caches(context.Background())
	if err != nil {
		t.Errorf("list the pool for caches the suite left behind: %v", err)
		return
	}
	for _, c := range caches {
		if ours(c.Key) {
			t.Errorf("the suite left cache %s behind", c.Key)
			deleteCache(t, d, c.Key)
		}
	}
}

// absent fails unless the guest has nothing at path.
func absent(t *testing.T, g machine.Guest, path, what string) {
	t.Helper()
	stream, err := g.CopyFrom(context.Background(), path)
	if err == nil {
		stream.Close()
		t.Fatalf("%s has %s", what, path)
	}
}

func randomHex(n int) string {
	b := make([]byte, n)
	_, _ = rand.Read(b)
	return hex.EncodeToString(b)
}

func tarOf(t *testing.T, name, content string) io.Reader {
	t.Helper()
	var buf bytes.Buffer
	tw := tar.NewWriter(&buf)
	if err := tw.WriteHeader(&tar.Header{Name: name, Mode: 0o644, Size: int64(len(content)), Typeflag: tar.TypeReg}); err != nil {
		t.Fatal(err)
	}
	_, _ = tw.Write([]byte(content))
	_ = tw.Close()
	return &buf
}

func readMarker(t *testing.T, g machine.Guest, path string) string {
	t.Helper()
	stream, err := g.CopyFrom(context.Background(), path)
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}
	defer stream.Close()
	tr := tar.NewReader(stream)
	if _, err := tr.Next(); err != nil {
		t.Fatal(err)
	}
	data, err := io.ReadAll(tr)
	if err != nil {
		t.Fatal(err)
	}
	return string(data)
}

// checkExec runs a process in dir with an added variable and stdin, and
// checks that its stdout, stderr, and exit code come back apart.
func checkExec(t *testing.T, g machine.Guest, guestOS machine.OS, dir string) {
	t.Helper()
	argv := []string{"/bin/sh", "-c", `echo "$GREETING"; echo oops >&2; cat proof.txt; echo; cat; exit 3`}
	if guestOS == machine.Windows {
		argv = []string{"cmd", "/c", `echo %GREETING%& echo oops 1>&2& type proof.txt& echo.& findstr x*& exit 3`}
	}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	var stdout, stderr bytes.Buffer
	code, err := machine.Run(ctx, g, machine.ExecRequest{Argv: argv, Env: []string{"GREETING=hello"}, Dir: dir}, strings.NewReader("piped\n"), &stdout, &stderr)
	if err != nil {
		t.Fatalf("exec: %v", err)
	}
	lines := strings.Fields(strings.ReplaceAll(stdout.String(), "\r", ""))
	if code != 3 || !slices.Equal(lines, []string{"hello", "committed", "piped"}) || strings.TrimSpace(stderr.String()) != "oops" {
		t.Fatalf("exec: exit %d, stdout %q, stderr %q; want exit 3, hello, committed, and piped, and oops", code, stdout.String(), stderr.String())
	}
}

// checkServiceURLs checks a running instance's service URLs against the
// driver's reach (ADR 26-11-14-610): an ingress URL wherever the driver
// gives one by default, which this process connects to; a pool URL wherever
// it always gives one, an HTTP URL or the ingress URL; and none in a scope
// it never gives. Where connect says so, this process connects to it.
func checkServiceURLs(ctx context.Context, t *testing.T, reach machine.URLCapabilities, service machine.Service, connect bool) {
	t.Helper()
	switch {
	case reach.Ingress == machine.URLNever && service.IngressURL != "":
		t.Fatalf("the driver gives no ingress URL, and a running instance has %q", service.IngressURL)
	case reach.Ingress != machine.URLNever && service.IngressURL == "":
		t.Fatalf("a running instance asked for the driver's default has no ingress URL: %+v", service)
	case reach.Pool == machine.URLNever && service.PoolURL != "":
		t.Fatalf("the driver gives no pool URL, and a running instance has %q", service.PoolURL)
	case !reach.Credential && service.Credential != nil:
		t.Fatalf("a driver whose URLs need no credential gave one: %+v", service.Credential)
	case reach.Credential && service.Credential == nil:
		t.Fatalf("a driver whose URLs need a credential gave none: %+v", service)
	}
	if p := service.PoolURL; p != "" && p != service.IngressURL && !strings.HasPrefix(p, "http://") && !strings.HasPrefix(p, "https://") {
		t.Fatalf("the pool URL of a running instance is %q, neither an HTTP URL nor its ingress URL", p)
	}
	if service.IngressURL == "" || !connect {
		return
	}
	dialCtx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	conn, err := machine.DialURL(dialCtx, service.IngressURL)
	if err != nil {
		t.Fatalf("connect to the ingress URL %s of a running instance: %v", service.IngressURL, err)
	}
	_ = conn.Close()
}

// resolveScript looks for a name resolved, or a resolver that answers, from
// inside a guest whose fence closes DNS, and exits 1 at any. It asks the
// guest's own resolver for a name, then sends a DNS query for example.com
// over UDP, and opens TCP, to port 53 of every resolver resolv.conf names and
// of 8.8.8.8 and 1.1.1.1. It needs bash, timeout, and getent.
const resolveScript = `
open=0
if timeout 10 getent hosts example.com; then echo "example.com resolved"; open=1; fi
for r in $(awk '$1 == "nameserver" {print $2}' /etc/resolv.conf) 8.8.8.8 1.1.1.1; do
	if timeout 5 bash -c 'exec 3<>/dev/udp/'"$r"'/53 && printf "\x12\x34\x01\x00\x00\x01\x00\x00\x00\x00\x00\x00\x07example\x03com\x00\x00\x01\x00\x01" >&3 && head -c 1 <&3 >/dev/null'; then
		echo "$r answered a query over UDP"; open=1
	fi
	if timeout 5 bash -c 'exec 3<>/dev/tcp/'"$r"'/53'; then echo "$r accepted a connection to 53/tcp"; open=1; fi
done
exit $open
`

// urlStatus is the status of a GET of a service's URL, with cred's header
// where it is not nil, over HTTP/1.1, which a credentialed URL may need.
func urlStatus(ctx context.Context, t *testing.T, client *http.Client, url string, cred *machine.Credential) int {
	t.Helper()
	if client == nil {
		client = &http.Client{Transport: &http.Transport{Proxy: http.ProxyFromEnvironment, ForceAttemptHTTP2: false}}
	}
	reqCtx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(reqCtx, http.MethodGet, url, nil)
	if err != nil {
		t.Fatal(err)
	}
	if cred != nil {
		req.Header.Set(cred.Header, cred.Token)
	}
	resp, err := client.Do(req)
	if err != nil {
		t.Fatalf("GET %s: %v", url, err)
	}
	_ = resp.Body.Close()
	return resp.StatusCode
}
