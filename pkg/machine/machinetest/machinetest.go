// Package machinetest is the conformance suite every machine.Driver must pass.
// It is the definition of done for a driver: the fake driver passes it in CI on
// every OS, and the hcs and vz drivers run it on real hardware.
//
// The suite speaks only the machine interfaces, so it checks exactly the
// contract the engine relies on: a layer can be installed, cloned, booted, and
// reached; a booted machine's guest operations answer, and exec carries stdin,
// stdout, stderr, the environment, the working directory, and the exit code;
// an orderly shutdown stops the machine; a committed layer carries its writes to clones
// of it and to no one else; clones run side by side; Kill always works.
package machinetest

import (
	"archive/tar"
	"bytes"
	"context"
	"errors"
	"io"
	"maps"
	"os"
	"path/filepath"
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
	// Base is an already-installed layer to start from.
	Base *machine.Layer
	// Boot is used for every boot.
	Boot machine.BootOptions
	// Timeout bounds each boot until the guest answers, and each shutdown.
	Timeout time.Duration
	// ScratchDir is a guest directory the suite may write into.
	ScratchDir string
}

// Run runs the suite.
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

	work := t.TempDir()
	base := cfg.Base
	if base == nil {
		layer := machine.Layer{ID: "base", Dir: filepath.Join(work, "layer-base")}
		if err := os.MkdirAll(layer.Dir, 0o755); err != nil {
			t.Fatal(err)
		}
		if err := driver.Install(ctx, cfg.Install, layer); err != nil {
			t.Fatalf("install: %v", err)
		}
		// A driver may hold a layer outside its Dir, as boxd holds a
		// snapshot, so the suite releases the layers it made.
		t.Cleanup(func() { _ = driver.DeleteLayer(context.Background(), layer) })
		base = &layer
	}
	guestOS := cfg.Install.GuestOS
	if guestOS == "" {
		guestOS = caps.GuestOS[0]
	}
	instance := func(id string, chain ...machine.Layer) machine.InstanceSpec {
		return machine.InstanceSpec{ID: id, Dir: filepath.Join(work, "inst-"+id), GuestOS: guestOS, Chain: chain}
	}
	boot := func(t *testing.T, inst machine.InstanceSpec) machine.Machine {
		t.Helper()
		bootCtx, cancel := context.WithTimeout(ctx, cfg.Timeout)
		defer cancel()
		m, err := driver.Boot(bootCtx, inst, cfg.Boot)
		if err != nil {
			t.Fatalf("boot %s: %v", inst.ID, err)
		}
		t.Cleanup(func() { _ = m.Kill(context.Background()) })
		if !caps.Remote {
			if _, ok := m.(machine.Dialer); !ok {
				t.Fatal("the driver is not Remote, but its machine is not a machine.Dialer for the shim")
			}
		}
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

	// Boot a clone of the base, write a marker, shut down, and commit.
	var committed machine.Layer
	t.Run("boot-write-commit", func(t *testing.T) {
		inst := instance("writer", *base)
		if err := driver.Prepare(ctx, inst); err != nil {
			t.Fatalf("prepare: %v", err)
		}
		// Destroyed below once committed; this covers a step that fails
		// first, which on a cloud driver would leave a billed machine.
		t.Cleanup(func() { _ = driver.Destroy(context.Background(), inst) })
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
		committed = machine.Layer{ID: "committed", Dir: filepath.Join(work, "layer-committed")}
		if err := os.MkdirAll(committed.Dir, 0o755); err != nil {
			t.Fatal(err)
		}
		if err := driver.Commit(ctx, inst, committed); err != nil {
			t.Fatalf("commit: %v", err)
		}
		if err := driver.Destroy(ctx, inst); err != nil {
			t.Fatalf("destroy after commit: %v", err)
		}
	})
	if committed.Dir == "" {
		t.FailNow()
	}
	t.Cleanup(func() { _ = driver.DeleteLayer(context.Background(), committed) })

	t.Run("a-committed-layer-exists", func(t *testing.T) {
		for _, layer := range []machine.Layer{*base, committed} {
			if ok, err := driver.LayerExists(ctx, layer); err != nil || !ok {
				t.Fatalf("layer %s exists = %v, %v", layer.ID, ok, err)
			}
		}
	})

	// What the store knows is never an orphan, and what a remote driver
	// holds for it is one the moment the store forgets it: a prepared
	// instance's machine and the two layers'.
	t.Run("what-the-store-forgets-is-an-orphan", func(t *testing.T) {
		inst := instance("known", *base, committed)
		if err := driver.Prepare(ctx, inst); err != nil {
			t.Fatalf("prepare: %v", err)
		}
		t.Cleanup(func() { _ = driver.Destroy(context.Background(), inst) })
		orphans := func(known machine.Known) map[string]machine.OrphanKind {
			t.Helper()
			list, err := driver.Orphans(ctx, known)
			if err != nil {
				t.Fatalf("orphans: %v", err)
			}
			out := map[string]machine.OrphanKind{}
			for _, o := range list {
				if o.Name == "" || o.Kind != machine.OrphanMachine && o.Kind != machine.OrphanLayer {
					t.Fatalf("orphan %+v has no name or no kind", o)
				}
				out[o.Name] = o.Kind
			}
			return out
		}
		known := orphans(machine.Known{Instances: []machine.InstanceSpec{inst}, Layers: []machine.Layer{*base, committed}})
		forgotten := orphans(machine.Known{})
		gained := map[machine.OrphanKind]int{}
		for name, kind := range forgotten {
			if _, ok := known[name]; !ok {
				gained[kind]++
			}
		}
		for name := range known {
			if _, ok := forgotten[name]; !ok {
				t.Fatalf("%s is an orphan only while the store knows more", name)
			}
		}
		want := map[machine.OrphanKind]int{}
		if caps.Remote {
			want = map[machine.OrphanKind]int{machine.OrphanMachine: 1, machine.OrphanLayer: 2}
		}
		if !maps.Equal(gained, want) {
			t.Fatalf("forgetting an instance and two layers made orphans of %v, want %v", gained, want)
		}
	})

	// A tag is recorded and dropped any number of times, and a tag never
	// recorded is dropped without complaint.
	t.Run("tag-and-untag-a-layer", func(t *testing.T) {
		const ref = "conformance/app:latest"
		t.Cleanup(func() { _ = driver.UntagLayer(context.Background(), committed, ref) })
		for range 2 {
			if err := driver.TagLayer(ctx, committed, ref); err != nil {
				t.Fatalf("tag: %v", err)
			}
		}
		for range 2 {
			if err := driver.UntagLayer(ctx, committed, ref); err != nil {
				t.Fatalf("untag: %v", err)
			}
		}
	})

	// A clone of the committed layer sees the marker; a clone of the base does
	// not; both run at once.
	t.Run("clones-are-isolated-and-concurrent", func(t *testing.T) {
		child := instance("child", *base, committed)
		sibling := instance("sibling", *base)
		for _, inst := range []machine.InstanceSpec{child, sibling} {
			if err := driver.Prepare(ctx, inst); err != nil {
				t.Fatalf("prepare %s: %v", inst.ID, err)
			}
			t.Cleanup(func() { _ = driver.Destroy(context.Background(), inst) })
		}
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
		inst := instance("killed", *base)
		if err := driver.Prepare(ctx, inst); err != nil {
			t.Fatalf("prepare: %v", err)
		}
		t.Cleanup(func() { _ = driver.Destroy(context.Background(), inst) })
		m := boot(t, inst)
		if err := m.Kill(ctx); err != nil {
			t.Fatalf("kill: %v", err)
		}
		select {
		case <-m.Done():
		case <-time.After(time.Minute):
			t.Fatal("Done was not closed after Kill")
		}
	})

	if caps.Remote {
		// A remote machine outlives the process that booted it: another
		// process attaches to it, and a guest that powers itself off while
		// nothing watches is found stopped.
		t.Run("attach", func(t *testing.T) {
			attacher, ok := driver.(machine.Attacher)
			if !ok {
				t.Fatal("Capabilities.Remote is listed, but the driver is not a machine.Attacher")
			}
			inst := instance("attached", *base)
			if err := driver.Prepare(ctx, inst); err != nil {
				t.Fatalf("prepare: %v", err)
			}
			t.Cleanup(func() { _ = driver.Destroy(context.Background(), inst) })
			if _, err := attacher.Attach(ctx, inst); !errors.Is(err, machine.ErrNotRunning) {
				t.Fatalf("attach to a prepared, unbooted instance: %v, want ErrNotRunning", err)
			}

			// Booted and let go, as by a process that has since exited:
			// nothing ever waits on this machine's Done.
			bootCtx, cancel := context.WithTimeout(ctx, cfg.Timeout)
			defer cancel()
			if _, err := driver.Boot(bootCtx, inst, cfg.Boot); err != nil {
				t.Fatalf("boot: %v", err)
			}

			m, err := attacher.Attach(ctx, inst)
			if err != nil {
				t.Fatalf("attach to a running instance: %v", err)
			}
			t.Cleanup(func() { _ = m.Kill(context.Background()) })
			if info, err := m.Info(ctx); err != nil || info.OS != string(guestOS) {
				t.Fatalf("info through an attached machine: %+v, %v", info, err)
			}

			if err := m.Shutdown(ctx, false); err != nil {
				t.Fatalf("shutdown request: %v", err)
			}
			deadline := time.Now().Add(cfg.Timeout)
			for {
				_, err := attacher.Attach(ctx, inst)
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
