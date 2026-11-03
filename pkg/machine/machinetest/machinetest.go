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
// an orderly shutdown stops the machine; on a driver that reports
// Capabilities.Commit, a committed VM carries its writes to VMs created from
// it and to no one else, and a copy of a copy outlives the first's removal,
// or the driver refuses the removal with ErrInUse, while a driver that does
// not refuses a commit and a copy and changes nothing (ADR 26-11-03-868),
// and every other subtest starts from a fresh VM, so needs no commit; VMs
// run side by side; Kill always works; a VM fenced at create keeps its fence
// across a boot, or the driver refuses the fence; a started machine is
// attached to from another call, its service has a URL, and a private URL
// where it has one, and it is found stopped once its guest powers off; a
// guest port is dialed through an attached machine, and a guest reaches a
// peer's port through a forward, or the driver refuses both; a data volume
// is kept across a boot and never in a committed VM; a cache volume is a
// private clone of its key's generation, which only a save moves; and a key
// the driver issues runs the project's machines, cannot issue another, and
// works no more once it is revoked. That a VM is committed is the caller's
// to record, so the suite never boots one again.
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
	"slices"
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
	// Timeout bounds each boot until the guest answers, and each shutdown.
	Timeout time.Duration
	// ScratchDir is a guest directory the suite may write into.
	ScratchDir string
	// GuestAgent is where the disco-vm agent is in the guest, which the
	// suite runs as `dial-host` to reach a peer through a forward. Empty
	// skips that subtest.
	GuestAgent string
	// Keyed is the driver again, for the same project, calling its
	// provider with the key keys answers. A driver whose Capabilities.Keys
	// issues needs it.
	Keyed func(keys machine.KeySource) machine.Driver
}

// Run runs the suite. The driver is one project's, and the suite makes and
// removes VMs in it, every name its own.
func Run(t *testing.T, driver machine.Driver, cfg Config) {
	t.Helper()
	if cfg.Timeout == 0 {
		cfg.Timeout = 10 * time.Minute
	}
	ctx := context.Background()
	caps := driver.Capabilities()

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
	if err := driver.Check(ctx); err != nil {
		t.Fatalf("check: %v", err)
	}

	suite := t
	run := randomHex(4)
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
		t.Cleanup(func() { _ = driver.Destroy(context.Background(), installed) })
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
				_ = driver.Destroy(context.Background(), inst)
			}
		})
		if inst.Name != spec.Name || inst.ID == "" {
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
	boot := func(t *testing.T, inst machine.Instance) machine.Machine {
		t.Helper()
		bootCtx, cancel := context.WithTimeout(ctx, cfg.Timeout)
		defer cancel()
		m, err := driver.Boot(bootCtx, inst, machine.BootOptions{})
		if err != nil {
			t.Fatalf("boot %s: %v", inst.Name, err)
		}
		t.Cleanup(func() { _ = m.Kill(context.Background()) })
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
		suite.Cleanup(func() { _ = driver.Destroy(context.Background(), committed) })
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
				_ = driver.Destroy(context.Background(), made)
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
				_ = driver.Destroy(context.Background(), inst)
			}
			t.Fatalf("a VM named as the committed VM %s: %v, want ErrExists", committed.Name, err)
		}
		if _, err := driver.Attach(ctx, committed); !errors.Is(err, machine.ErrNotRunning) {
			t.Fatalf("attach to a committed VM: %v, want ErrNotRunning", err)
		}
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
		t.Cleanup(func() { _ = driver.Destroy(context.Background(), inst) })
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
						_ = driver.Destroy(context.Background(), inst)
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

	// A guest port is dialed through a machine another call attached to: the
	// agent's, where an HTTP request for its health is answered. A driver
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
	// again is followed.
	t.Run("a-peer-forward", func(t *testing.T) {
		if !caps.Ports {
			spec := start
			spec.Name = fmt.Sprintf("conformance-%s-forwarded", run)
			spec.Forwards = []machine.Forward{{Port: 7402, Instance: "peer", InstancePort: guest.AgentPort}}
			if inst, err := driver.Create(ctx, spec); !errors.Is(err, machine.ErrUnsupported) {
				if err == nil {
					_ = driver.Destroy(context.Background(), inst)
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
			return m
		}
		peer := create(t, copied)
		pm := start(t, peer)
		proof := randomHex(8)
		if err := pm.CopyTo(ctx, marker, tarOf(t, "peer.txt", proof)); err != nil {
			t.Fatal(err)
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

	// A started machine outlives the call that started it: another call
	// attaches to it, its service has a URL, and a private URL that is one
	// where it has any, and a guest that powers itself off while nothing
	// watches is found stopped.
	t.Run("start-attach-and-stop", func(t *testing.T) {
		inst := create(t, start)
		startCtx, cancel := context.WithTimeout(ctx, cfg.Timeout)
		defer cancel()
		started, err := driver.Start(startCtx, inst, machine.BootOptions{})
		if err != nil {
			t.Fatalf("start: %v", err)
		}
		t.Cleanup(func() { _ = started.Kill(context.Background()) })
		m, err := driver.Attach(ctx, inst)
		if err != nil {
			t.Fatalf("attach to a running instance: %v", err)
		}
		if info, err := m.Info(ctx); err != nil || info.OS != string(guestOS) {
			t.Fatalf("info through an attached machine: %+v, %v", info, err)
		}
		service, err := driver.Service(ctx, inst)
		if err != nil || service.Listen == "" || service.URL == "" {
			t.Fatalf("service of a running instance: %+v, %v", service, err)
		}
		if p := service.PrivateURL; p != "" && !strings.HasPrefix(p, "http://") && !strings.HasPrefix(p, "https://") {
			t.Fatalf("the private URL of a running instance is %q, not an HTTP URL", p)
		}
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
	})

	// A key the driver issues runs the project's machines from another
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
		t.Cleanup(func() { _ = driver.RevokeKey(context.Background(), key) })
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
			_ = driver.RevokeKey(context.Background(), again)
			t.Fatalf("a second key named %s: %v, want ErrExists", key.Name, err)
		}
		if cfg.Keyed == nil {
			t.Fatal("the driver issues keys, and Config.Keyed is nil")
		}
		keyed := cfg.Keyed(machine.StaticKey(key))
		if again, err := keyed.IssueKey(ctx, machine.IssueRequest{Name: "conformance-" + run + "-again", TTL: ttl}); err == nil {
			_ = driver.RevokeKey(context.Background(), again)
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
		t.Cleanup(func() { _ = driver.Destroy(context.Background(), inst) })
		startCtx, cancel := context.WithTimeout(ctx, cfg.Timeout)
		defer cancel()
		m, err := keyed.Start(startCtx, inst, machine.BootOptions{})
		if err != nil {
			t.Fatalf("start with the issued key: %v", err)
		}
		t.Cleanup(func() { _ = m.Kill(context.Background()) })
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
			_ = driver.Destroy(context.Background(), inst)
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
					_ = driver.Destroy(context.Background(), inst)
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
		t.Cleanup(func() { _ = driver.Destroy(context.Background(), reborn) })
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
					_ = driver.Destroy(context.Background(), inst)
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
			_ = driver.DeleteCache(context.Background(), key)
			_ = driver.DeleteCache(context.Background(), other)
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
