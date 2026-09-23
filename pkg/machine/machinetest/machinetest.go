// Package machinetest is the conformance suite every machine.Driver must pass.
// It is the definition of done for a driver: the fake driver passes it in CI on
// every OS, and the hcs and vz drivers run it on real hardware.
//
// The suite speaks only machine and guest interfaces, so it checks exactly the
// contract the engine relies on: a layer can be installed, cloned, booted, and
// reached; the guest agent answers on AgentPort; an orderly shutdown through
// the agent stops the machine; a committed layer carries its writes to clones
// of it and to no one else; clones run side by side; a warm image serves the
// fast clone modes the driver lists; Kill always works.
package machinetest

import (
	"archive/tar"
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/discobox-ai/vm/pkg/guest"
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
	// Timeout bounds each boot until the agent answers, and each shutdown.
	Timeout time.Duration
	// ScratchDir is a guest directory the suite may write into.
	ScratchDir string
}

// Run runs the suite.
func Run(t *testing.T, driver machine.Driver, cfg Config) {
	if cfg.Timeout == 0 {
		cfg.Timeout = 10 * time.Minute
	}
	ctx := context.Background()
	caps := driver.Capabilities()

	t.Run("capabilities", func(t *testing.T) {
		if len(caps.GuestOS) == 0 {
			t.Fatal("no guest OS")
		}
		if !caps.SupportsMode(machine.Cold) {
			t.Fatal("cold clones are required of every driver")
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
		base = &layer
	}
	guestOS := cfg.Install.GuestOS
	if guestOS == "" {
		guestOS = caps.GuestOS[0]
	}
	instance := func(id string, chain ...machine.Layer) machine.InstanceSpec {
		return machine.InstanceSpec{ID: id, Dir: filepath.Join(work, "inst-"+id), GuestOS: guestOS, Chain: chain, Mode: machine.Cold}
	}
	boot := func(t *testing.T, inst machine.InstanceSpec) (machine.Machine, *guest.Client) {
		t.Helper()
		bootCtx, cancel := context.WithTimeout(ctx, cfg.Timeout)
		defer cancel()
		m, err := driver.Boot(bootCtx, inst, cfg.Boot)
		if err != nil {
			t.Fatalf("boot %s: %v", inst.ID, err)
		}
		t.Cleanup(func() { _ = m.Kill(context.Background()) })
		client := guest.NewClient(func(ctx context.Context) (net.Conn, error) { return m.Dial(ctx, guest.AgentPort) })
		t.Cleanup(client.Close)
		if err := client.WaitReady(bootCtx, m.Done()); err != nil {
			t.Fatalf("agent on %s: %v", inst.ID, err)
		}
		return m, client
	}
	shutdown := func(t *testing.T, m machine.Machine, client *guest.Client) {
		t.Helper()
		if err := client.Shutdown(ctx, false); err != nil {
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
		m, client := boot(t, inst)
		info, err := client.Info(ctx)
		if err != nil {
			t.Fatal(err)
		}
		if info.OS != string(guestOS) {
			t.Fatalf("guest reports OS %q, want %q", info.OS, guestOS)
		}
		if err := client.CopyTo(ctx, marker, tarOf(t, "proof.txt", "committed")); err != nil {
			t.Fatalf("write marker: %v", err)
		}
		shutdown(t, m, client)
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
		childM, childC := boot(t, child)
		if limit == 1 {
			shutdown(t, childM, childC)
		}
		_, siblingC := boot(t, sibling)

		if limit != 1 {
			if got := readMarker(t, childC, marker+"/proof.txt"); got != "committed" {
				t.Fatalf("the committed layer's clone reads %q", got)
			}
		}
		if _, err := siblingC.CopyFrom(ctx, marker+"/proof.txt"); err == nil {
			t.Fatal("a clone of the base saw a write committed to another layer")
		}
	})

	// A driver that clones in a faster mode than cold stages an image for it,
	// and its clones carry the image's writes.
	t.Run("warm", func(t *testing.T) {
		fast := slices.DeleteFunc(slices.Clone(caps.CloneModes), func(m machine.CloneMode) bool { return m == machine.Cold })
		if len(fast) == 0 {
			t.Skip("the driver clones cold only")
		}
		warmer, ok := driver.(machine.Warmer)
		if !ok {
			t.Fatalf("the driver offers %v clones but does not implement machine.Warmer", fast)
		}
		spec := machine.WarmSpec{
			GuestOS: guestOS, Chain: []machine.Layer{*base, committed}, Dir: filepath.Join(work, "warm"),
			Count: 2, CPUs: cfg.Boot.CPUs, Memory: cfg.Boot.Memory,
		}
		if err := os.MkdirAll(spec.Dir, 0o755); err != nil {
			t.Fatal(err)
		}
		if w, err := warmer.Warmth(ctx, spec); err != nil || w.Clones != 0 {
			t.Fatalf("an image never warmed reports %+v, %v", w, err)
		}
		warmCtx, cancel := context.WithTimeout(ctx, time.Duration(spec.Count+1)*cfg.Timeout)
		stage, err := warmer.Warm(warmCtx, spec)
		cancel()
		if err != nil {
			t.Fatalf("warm: %v", err)
		}
		if stage != nil {
			t.Cleanup(func() { _ = stage.Close(context.Background()) })
		}
		w, err := warmer.Warmth(ctx, spec)
		if err != nil || !slices.Contains(fast, w.Mode) || w.Clones == 0 {
			t.Fatalf("a warmed image reports %+v, %v; want one of %v", w, err, fast)
		}

		inst := instance("warm-1", *base, committed)
		inst.Mode, inst.WarmDir = w.Mode, spec.Dir
		if err := driver.Prepare(ctx, inst); err != nil {
			t.Fatalf("prepare %s: %v", w.Mode, err)
		}
		t.Cleanup(func() { _ = driver.Destroy(context.Background(), inst) })
		m, client := boot(t, inst)
		if got := readMarker(t, client, marker+"/proof.txt"); got != "committed" {
			t.Fatalf("a %s clone of the committed layer reads %q", w.Mode, got)
		}
		_ = m.Kill(ctx)

		// A stage used once per clone runs out, and says so.
		if w.Clones > 0 {
			for i := 2; i <= w.Clones; i++ {
				used := instance(fmt.Sprintf("warm-%d", i), *base, committed)
				used.Mode, used.WarmDir = w.Mode, spec.Dir
				if err := driver.Prepare(ctx, used); err != nil {
					t.Fatalf("prepare %s clone %d of %d: %v", w.Mode, i, w.Clones, err)
				}
				t.Cleanup(func() { _ = driver.Destroy(context.Background(), used) })
			}
			if left, err := warmer.Warmth(ctx, spec); err != nil || left.Clones != 0 {
				t.Fatalf("after %d clones the stage reports %+v, %v", w.Clones, left, err)
			}
			extra := instance("warm-extra", *base, committed)
			extra.Mode, extra.WarmDir = w.Mode, spec.Dir
			if err := driver.Prepare(ctx, extra); !errors.Is(err, machine.ErrNotWarm) {
				t.Fatalf("prepare from a used-up stage: %v, want ErrNotWarm", err)
			}
			_ = driver.Destroy(ctx, extra)
		}

		if stage != nil {
			if err := stage.Close(ctx); err != nil {
				t.Fatalf("close stage: %v", err)
			}
		}
		if err := warmer.Cool(ctx, spec); err != nil {
			t.Fatalf("cool: %v", err)
		}
		if left, err := warmer.Warmth(ctx, spec); err != nil || left.Clones != 0 {
			t.Fatalf("a cooled image reports %+v, %v", left, err)
		}
	})

	t.Run("kill", func(t *testing.T) {
		inst := instance("killed", *base)
		if err := driver.Prepare(ctx, inst); err != nil {
			t.Fatalf("prepare: %v", err)
		}
		t.Cleanup(func() { _ = driver.Destroy(context.Background(), inst) })
		m, _ := boot(t, inst)
		if err := m.Kill(ctx); err != nil {
			t.Fatalf("kill: %v", err)
		}
		select {
		case <-m.Done():
		case <-time.After(time.Minute):
			t.Fatal("Done was not closed after Kill")
		}
	})
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

func readMarker(t *testing.T, client *guest.Client, path string) string {
	t.Helper()
	stream, err := client.CopyFrom(context.Background(), path)
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
