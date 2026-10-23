// Package machinetest is the conformance suite every machine.Driver must pass.
// It is the definition of done for a driver: the fake driver passes it in CI on
// every OS, and the hcs and vz drivers run it on real hardware.
//
// The suite speaks only the machine interfaces, so it checks exactly the
// contract the engine relies on: a layer can be installed, found, listed,
// cloned, booted, and reached; an instance is found and listed by its name,
// which no second instance can take; a booted machine's guest operations
// answer, and exec carries stdin, stdout, stderr, the environment, the working
// directory, and the exit code; an orderly shutdown stops the machine; a
// committed layer carries its writes to clones of it and to no one else; an
// instance runs from a base the driver lists, and commits a layer; a tag is
// set, moved, and dropped; clones run side by side; Kill always works; and a
// started machine is attached to from another call, and found stopped once
// its guest powers off.
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

	"github.com/discobox-ai/vm/pkg/machine"
)

// Config adapts the suite to a driver.
type Config struct {
	// Install creates the base layer. Leave Install.GuestOS empty and set Base
	// instead to reuse an installed layer, which is how a real driver keeps
	// the suite from reinstalling an OS on every run.
	Install machine.InstallSpec
	// Base is the ID of an already-installed layer to start from.
	Base string
	// CPUs and Memory size every instance the suite makes.
	CPUs   int
	Memory uint64
	// Timeout bounds each boot until the guest answers, and each shutdown.
	Timeout time.Duration
	// ScratchDir is a guest directory the suite may write into.
	ScratchDir string
}

// Run runs the suite. The driver is one project's, and the suite makes and
// removes layers, tags, and instances in it, every name its own.
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

	run := randomHex(4)
	newID := func() string { return randomHex(32) }
	var base machine.Layer
	if cfg.Base != "" {
		var err error
		if base, err = driver.Layer(ctx, cfg.Base); err != nil {
			t.Fatalf("base layer %s: %v", cfg.Base, err)
		}
	} else {
		var err error
		base, err = driver.Install(ctx, cfg.Install, machine.Layer{ID: newID(), GuestOS: cfg.Install.GuestOS, Comment: "conformance install"})
		if err != nil {
			t.Fatalf("install: %v", err)
		}
		t.Cleanup(func() { _ = driver.DeleteLayer(context.Background(), base.ID) })
	}
	guestOS := base.GuestOS
	if guestOS == "" {
		guestOS = caps.GuestOS[0]
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
	marker := strings.TrimRight(cfg.ScratchDir, `/\`) + "/disco-vm-conformance"

	t.Run("a-layer-is-found-and-listed", func(t *testing.T) {
		got, err := driver.Layer(ctx, base.ID)
		if err != nil || got.ID != base.ID {
			t.Fatalf("layer %s: %+v, %v", base.ID, got, err)
		}
		if _, err := driver.Layer(ctx, newID()); !errors.Is(err, machine.ErrNotFound) {
			t.Fatalf("a layer never made: %v, want ErrNotFound", err)
		}
		if !hasLayer(t, driver, base.ID) {
			t.Fatalf("layers do not list %s", base.ID)
		}
	})

	// Boot a clone of the base, write a marker, shut down, and commit.
	var committed machine.Layer
	t.Run("boot-write-commit", func(t *testing.T) {
		inst := create(t, machine.InstanceSpec{Image: "conformance", Layer: base.ID})
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
		want := machine.Layer{ID: newID(), Parent: base.ID, GuestOS: guestOS, Comment: "conformance commit"}
		committed, err = driver.Commit(ctx, inst, want)
		if err != nil {
			t.Fatalf("commit: %v", err)
		}
		if committed.ID != want.ID || committed.GuestOS != guestOS || (committed.Parent != "" && committed.Parent != base.ID) {
			t.Fatalf("committed %+v, want %+v", committed, want)
		}
		if _, err := driver.Instance(ctx, inst.Name); !errors.Is(err, machine.ErrNotFound) {
			t.Fatalf("an instance committed is still there: %v", err)
		}
	})
	if committed.ID == "" {
		t.FailNow()
	}
	t.Cleanup(func() { _ = driver.DeleteLayer(context.Background(), committed.ID) })

	t.Run("an-instance-is-found-listed-and-its-name-taken", func(t *testing.T) {
		inst := create(t, machine.InstanceSpec{Image: "conformance", Layer: committed.ID})
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
		if _, err := driver.Create(ctx, machine.InstanceSpec{Name: inst.Name, Layer: base.ID, GuestOS: guestOS}); !errors.Is(err, machine.ErrExists) {
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

	// Every base the driver lists names itself, and an instance of the first
	// runs from it with no layer, and commits a layer, as a build from it does.
	t.Run("an-instance-of-a-base", func(t *testing.T) {
		bases, err := driver.Bases(ctx)
		if err != nil {
			t.Fatalf("bases: %v", err)
		}
		for _, b := range bases {
			if b.Name == "" || b.ID == "" || !caps.SupportsOS(b.GuestOS) {
				t.Fatalf("base %+v has no name, no ID, or a guest OS the driver does not run", b)
			}
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
		layer, err := driver.Commit(ctx, inst, machine.Layer{ID: newID(), GuestOS: b.GuestOS, Comment: "conformance on a base"})
		if err != nil {
			t.Fatalf("commit an instance of a base: %v", err)
		}
		t.Cleanup(func() { _ = driver.DeleteLayer(context.Background(), layer.ID) })
		if layer.Parent != "" {
			t.Fatalf("a layer committed from a base has parent %s", layer.Parent)
		}
		if _, err := driver.Layer(ctx, layer.ID); err != nil {
			t.Fatalf("the layer committed from a base: %v", err)
		}
	})

	// A tag is set, moved, listed, and dropped any number of times, and a tag
	// never set is dropped without complaint.
	t.Run("tag-move-and-untag", func(t *testing.T) {
		ref := "conformance/" + run + ":latest"
		t.Cleanup(func() { _ = driver.Untag(context.Background(), ref) })
		for range 2 {
			if err := driver.Tag(ctx, ref, base.ID); err != nil {
				t.Fatalf("tag: %v", err)
			}
		}
		if got := tagged(t, driver, ref); got != base.ID {
			t.Fatalf("%s names %q, want %s", ref, got, base.ID)
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

	// A clone of the committed layer sees the marker; a clone of the base does
	// not; both run at once.
	t.Run("clones-are-isolated-and-concurrent", func(t *testing.T) {
		child := create(t, machine.InstanceSpec{Image: "conformance", Layer: committed.ID})
		sibling := create(t, machine.InstanceSpec{Image: "conformance", Layer: base.ID})
		limit := caps.MaxRunning[guestOS]
		childM := boot(t, child)
		if limit == 1 {
			shutdown(t, childM)
		}
		siblingM := boot(t, sibling)

		if limit != 1 {
			if got := readMarker(t, childM, marker+"/proof.txt"); got != "committed" {
				t.Fatalf("the committed layer's clone reads %q", got)
			}
		}
		if _, err := siblingM.CopyFrom(ctx, marker+"/proof.txt"); err == nil {
			t.Fatal("a clone of the base saw a write committed to another layer")
		}
	})

	t.Run("kill", func(t *testing.T) {
		m := boot(t, create(t, machine.InstanceSpec{Image: "conformance", Layer: base.ID}))
		if err := m.Kill(ctx); err != nil {
			t.Fatalf("kill: %v", err)
		}
		select {
		case <-m.Done():
		case <-time.After(time.Minute):
			t.Fatal("Done was not closed after Kill")
		}
	})

	// A started machine outlives the call that started it: another call
	// attaches to it, its service has a URL, and a guest that powers itself
	// off while nothing watches is found stopped.
	t.Run("start-attach-and-stop", func(t *testing.T) {
		inst := create(t, machine.InstanceSpec{Image: "conformance", Layer: base.ID})
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
}

func hasLayer(t *testing.T, driver machine.Driver, id string) bool {
	t.Helper()
	layers, err := driver.Layers(context.Background())
	if err != nil {
		t.Fatalf("layers: %v", err)
	}
	return slices.ContainsFunc(layers, func(l machine.Layer) bool { return l.ID == id })
}

// tagged is the layer ref names, or empty.
func tagged(t *testing.T, driver machine.Driver, ref string) string {
	t.Helper()
	tags, err := driver.Tags(context.Background())
	if err != nil {
		t.Fatalf("tags: %v", err)
	}
	for _, tag := range tags {
		if tag.Ref == ref {
			return tag.Layer
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
