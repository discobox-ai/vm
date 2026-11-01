// Package machinetest is the conformance suite every machine.Driver must pass.
// It is the definition of done for a driver: the fake driver passes it in CI on
// every OS, and the hcs and vz drivers run it on real hardware.
//
// The suite speaks only the machine interfaces, so it checks exactly the
// contract the engine relies on: an image can be installed (or an instance
// run from a base), found, listed, cloned, booted, and reached; an instance
// is found and listed by its name, which no second instance can take; a
// booted machine's guest operations answer, and exec carries stdin, stdout,
// stderr, the environment, the working directory, and the exit code; an
// orderly shutdown stops the machine; a snapshot carries its writes to clones
// of it and to no one else, and stands alone once the image it came from is
// deleted; an instance runs from a base the driver lists or finds, and is
// snapshotted; a tag is set, moved, and dropped; clones run side by side;
// Kill always works; an instance fenced at create keeps its fence across a
// boot, or the driver refuses the fence; a started machine is attached to from
// another call, its service has a URL, and a private URL where it has one, and
// it is found stopped once its guest powers off; a guest port is dialed
// through an attached machine, and a guest reaches a peer's port through a
// forward, or the driver refuses both; and a key the driver issues
// runs the project's machines, cannot issue another, and works no more once
// it is revoked.
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

// Config adapts the suite to a driver. The suite starts from Image, else
// from an Install on a driver that lists Capabilities.Install, else from
// Base, else from the first base the driver lists.
type Config struct {
	// Install makes the image the suite starts from.
	Install machine.InstallSpec
	// Image is the ID of an image already made to start from, which is how
	// a real driver keeps the suite from reinstalling an OS on every run.
	Image string
	// Base is a base to start from, on a driver that installs nothing.
	Base *machine.Base
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
// removes images, tags, and instances in it, every name its own.
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
	})
	if err := driver.Check(ctx); err != nil {
		t.Fatalf("check: %v", err)
	}

	suite := t
	run := randomHex(4)
	newID := func() string { return randomHex(32) }
	// start is what the first instances run from: an image or a base.
	var start machine.InstanceSpec
	switch {
	case cfg.Image != "":
		img, err := driver.Image(ctx, cfg.Image)
		if err != nil {
			t.Fatalf("image %s: %v", cfg.Image, err)
		}
		start = machine.InstanceSpec{ImageID: img.ID, GuestOS: img.GuestOS}
	case cfg.Install.GuestOS != "" && caps.Installs(cfg.Install.GuestOS):
		img, err := driver.Install(ctx, cfg.Install, machine.Image{ID: newID(), GuestOS: cfg.Install.GuestOS, Comment: "conformance install"})
		if err != nil {
			t.Fatalf("install: %v", err)
		}
		t.Cleanup(func() { _ = driver.DeleteImage(context.Background(), img.ID) })
		start = machine.InstanceSpec{ImageID: img.ID, GuestOS: img.GuestOS}
	default:
		b := cfg.Base
		if b == nil {
			bases, err := driver.Bases(ctx)
			if err != nil || len(bases) == 0 {
				t.Fatalf("no Image, Install, or Base to start from, and the driver lists no bases: %v", err)
			}
			b = &bases[0]
		}
		start = machine.InstanceSpec{Base: b, GuestOS: b.GuestOS}
	}
	if start.GuestOS == "" {
		start.GuestOS = caps.GuestOS[0]
	}
	start.Image = "conformance"
	guestOS := start.GuestOS
	of := func(id string) machine.InstanceSpec {
		return machine.InstanceSpec{Image: "conformance", ImageID: id, GuestOS: guestOS}
	}
	names := 0
	create := func(t *testing.T, spec machine.InstanceSpec) machine.Instance {
		t.Helper()
		names++
		spec.Name = fmt.Sprintf("conformance-%s-%d", run, names)
		if spec.GuestOS == "" {
			spec.GuestOS = guestOS
		}
		if spec.CPUs == 0 {
			spec.CPUs, spec.Memory = cfg.CPUs, cfg.Memory
		}
		inst, err := driver.Create(ctx, spec)
		if err != nil {
			t.Fatalf("create %s: %v", spec.Name, err)
		}
		// A step that fails before the instance is removed would leave, on a
		// cloud driver, a billed machine.
		t.Cleanup(func() { _ = driver.Destroy(context.Background(), inst) })
		if inst.Name != spec.Name || inst.ID == "" {
			t.Fatalf("created %+v for name %s", inst, spec.Name)
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
	snapshot := func(t *testing.T, inst machine.Instance, comment string) machine.Image {
		t.Helper()
		want := machine.Image{ID: newID(), GuestOS: inst.GuestOS, Comment: comment}
		img, err := driver.Commit(ctx, inst, want)
		if err != nil {
			t.Fatalf("commit %s: %v", inst.Name, err)
		}
		// The suite's, not the subtest's: later subtests run from it.
		suite.Cleanup(func() { _ = driver.DeleteImage(context.Background(), img.ID) })
		if img.ID != want.ID || img.GuestOS != inst.GuestOS {
			t.Fatalf("committed %+v, want %+v", img, want)
		}
		if _, err := driver.Instance(ctx, inst.Name); !errors.Is(err, machine.ErrNotFound) {
			t.Fatalf("an instance committed is still there: %v", err)
		}
		return img
	}
	marker := strings.TrimRight(cfg.ScratchDir, `/\`) + "/disco-vm-conformance"

	// Boot an instance of the start, write a marker, shut down, and commit.
	var committed machine.Image
	t.Run("boot-write-commit", func(t *testing.T) {
		inst := create(t, start)
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
		shutdown(t, m)
		committed = snapshot(t, inst, "conformance commit")
	})
	if committed.ID == "" {
		t.FailNow()
	}

	t.Run("an-image-is-found-and-listed", func(t *testing.T) {
		got, err := driver.Image(ctx, committed.ID)
		if err != nil || got.ID != committed.ID {
			t.Fatalf("image %s: %+v, %v", committed.ID, got, err)
		}
		if _, err := driver.Image(ctx, newID()); !errors.Is(err, machine.ErrNotFound) {
			t.Fatalf("an image never made: %v, want ErrNotFound", err)
		}
		if !hasImage(t, driver, committed.ID) {
			t.Fatalf("images do not list %s", committed.ID)
		}
	})

	t.Run("an-instance-is-found-listed-and-its-name-taken", func(t *testing.T) {
		inst := create(t, of(committed.ID))
		got, err := driver.Instance(ctx, inst.Name)
		if err != nil || got.ID != inst.ID || got.Name != inst.Name || got.GuestOS != guestOS {
			t.Fatalf("instance %s: %+v, %v; created %+v", inst.Name, got, err, inst)
		}
		list, err := driver.Instances(ctx)
		if err != nil {
			t.Fatal(err)
		}
		if !slices.ContainsFunc(list, func(i machine.Instance) bool { return i.Name == inst.Name && i.ID == inst.ID }) {
			t.Fatalf("instances do not list %s: %+v", inst.Name, list)
		}
		again := of(committed.ID)
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

	// Every base the driver lists names itself and is found by its name,
	// and an instance of the first runs from it with no image, and is
	// snapshotted.
	t.Run("an-instance-of-a-base", func(t *testing.T) {
		bases, err := driver.Bases(ctx)
		if err != nil {
			t.Fatalf("bases: %v", err)
		}
		for _, b := range bases {
			if b.Name == "" || b.ID == "" || !caps.SupportsOS(b.GuestOS) {
				t.Fatalf("base %+v has no name, no ID, or a guest OS the driver does not run", b)
			}
			if found, ok, err := driver.Base(ctx, b.Name); err != nil || !ok || found.ID != b.ID {
				t.Fatalf("base %s by name = %+v, %v, %v", b.Name, found, ok, err)
			}
		}
		if _, ok, err := driver.Base(ctx, "disco-vm-conformance/no-such-base:none"); err != nil || ok {
			t.Fatalf("a base no one has: found %v, %v", ok, err)
		}
		if len(bases) == 0 {
			t.Skip("the driver lists no bases")
		}
		b := bases[0]
		inst := create(t, machine.InstanceSpec{Image: b.Name, Base: &b, GuestOS: b.GuestOS})
		m := boot(t, inst)
		info, err := m.Info(ctx)
		if err != nil {
			t.Fatal(err)
		}
		if info.OS != string(b.GuestOS) {
			t.Fatalf("a guest of base %s reports OS %q, want %q", b.Name, info.OS, b.GuestOS)
		}
		if err := m.CopyTo(ctx, marker, tarOf(t, "proof.txt", "on a base")); err != nil {
			t.Fatalf("write marker: %v", err)
		}
		shutdown(t, m)
		img := snapshot(t, inst, "conformance on a base")
		if _, err := driver.Image(ctx, img.ID); err != nil {
			t.Fatalf("the image snapshotted from a base: %v", err)
		}
	})

	// A tag is set, moved, listed, and dropped any number of times, and a tag
	// never set is dropped without complaint.
	t.Run("tag-move-and-untag", func(t *testing.T) {
		other := committed.ID
		if start.ImageID != "" {
			other = start.ImageID
		}
		ref := "conformance/" + run + ":latest"
		t.Cleanup(func() { _ = driver.Untag(context.Background(), ref) })
		for range 2 {
			if err := driver.Tag(ctx, ref, other); err != nil {
				t.Fatalf("tag: %v", err)
			}
		}
		if got := tagged(t, driver, ref); got != other {
			t.Fatalf("%s names %q, want %s", ref, got, other)
		}
		if err := driver.Tag(ctx, ref, committed.ID); err != nil {
			t.Fatalf("move a tag: %v", err)
		}
		if got := tagged(t, driver, ref); got != committed.ID {
			t.Fatalf("%s names %q after a move, want %s", ref, got, committed.ID)
		}
		for range 2 {
			if err := driver.Untag(ctx, ref); err != nil {
				t.Fatalf("untag: %v", err)
			}
		}
		if got := tagged(t, driver, ref); got != "" {
			t.Fatalf("%s names %s after untag", ref, got)
		}
	})

	// A clone of the snapshot sees the marker; a clone of the start does
	// not; both run at once.
	t.Run("clones-are-isolated-and-concurrent", func(t *testing.T) {
		child := create(t, of(committed.ID))
		sibling := create(t, start)
		limit := caps.MaxRunning[guestOS]
		childM := boot(t, child)
		if limit == 1 {
			shutdown(t, childM)
		}
		siblingM := boot(t, sibling)

		if limit != 1 {
			if got := readMarker(t, childM, marker+"/proof.txt"); got != "committed" {
				t.Fatalf("the snapshot's clone reads %q", got)
			}
		}
		if _, err := siblingM.CopyFrom(ctx, marker+"/proof.txt"); err == nil {
			t.Fatal("a clone of the start saw a write committed to a snapshot")
		}
	})

	t.Run("kill", func(t *testing.T) {
		m := boot(t, create(t, of(committed.ID)))
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
	// from a snapshot, reports the fence as asked, while it runs and once it
	// stops. A driver refuses each fence it does not hold.
	t.Run("a-fenced-instance", func(t *testing.T) {
		held := 0
		for i, from := range []struct {
			spec  machine.InstanceSpec
			allow []string
		}{
			{start, nil},
			{of(committed.ID), []string{"example.com"}},
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
		inst := create(t, of(committed.ID))
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
			spec := of(committed.ID)
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
		peer := create(t, of(committed.ID))
		pm := start(t, peer)
		proof := randomHex(8)
		if err := pm.CopyTo(ctx, marker, tarOf(t, "peer.txt", proof)); err != nil {
			t.Fatal(err)
		}
		spec := of(committed.ID)
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
		inst := create(t, of(committed.ID))
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

	// A snapshot of an instance of a snapshot keeps both writes, and stands
	// alone: once the image it came from is deleted, it still boots with
	// both. A driver whose snapshot still needs the other's disk refuses the
	// delete with ErrInUse instead. Last, since it deletes the snapshot the
	// others run from.
	t.Run("a-snapshot-stands-alone", func(t *testing.T) {
		inst := create(t, of(committed.ID))
		m := boot(t, inst)
		if err := m.CopyTo(ctx, marker, tarOf(t, "second.txt", "second")); err != nil {
			t.Fatalf("write marker: %v", err)
		}
		shutdown(t, m)
		second := snapshot(t, inst, "conformance second")
		switch err := driver.DeleteImage(ctx, committed.ID); {
		case errors.Is(err, machine.ErrInUse):
			t.Logf("the driver keeps %s for %s: %v", committed.ID, second.ID, err)
		case err != nil:
			t.Fatalf("delete %s: %v", committed.ID, err)
		default:
			if _, err := driver.Image(ctx, committed.ID); !errors.Is(err, machine.ErrNotFound) {
				t.Fatalf("a deleted image: %v, want ErrNotFound", err)
			}
		}
		m = boot(t, create(t, of(second.ID)))
		if got := readMarker(t, m, marker+"/proof.txt"); got != "committed" {
			t.Fatalf("a clone of the second snapshot reads %q from the first", got)
		}
		if got := readMarker(t, m, marker+"/second.txt"); got != "second" {
			t.Fatalf("a clone of the second snapshot reads %q from its own", got)
		}
	})
}

func hasImage(t *testing.T, driver machine.Driver, id string) bool {
	t.Helper()
	images, err := driver.Images(context.Background())
	if err != nil {
		t.Fatalf("images: %v", err)
	}
	return slices.ContainsFunc(images, func(img machine.Image) bool { return img.ID == id })
}

// tagged is the image ref names, or empty.
func tagged(t *testing.T, driver machine.Driver, ref string) string {
	t.Helper()
	tags, err := driver.Tags(context.Background())
	if err != nil {
		t.Fatalf("tags: %v", err)
	}
	for _, tag := range tags {
		if tag.Ref == ref {
			return tag.Image
		}
	}
	return ""
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
