package local_test

import (
	"archive/tar"
	"bytes"
	"context"
	"errors"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/discobox-ai/vm/pkg/machine"
	"github.com/discobox-ai/vm/pkg/machine/fake"
	"github.com/discobox-ai/vm/pkg/machine/local"
)

// failing is the fake hypervisor failing once at the step it names.
type failing struct {
	*fake.Hypervisor
	at string
}

func (f *failing) fail(step string) error {
	if f.at == step {
		f.at = ""
		return errors.New("failed at " + step)
	}
	return nil
}

func (f *failing) Prepare(ctx context.Context, inst local.InstanceSpec) error {
	if err := f.fail("prepare"); err != nil {
		return err
	}
	return f.Hypervisor.Prepare(ctx, inst)
}

func (f *failing) MoveVolumes(ctx context.Context, from, to local.InstanceSpec) error {
	if f.at == "move" {
		// Halfway: the data volume moves, then the move fails.
		f.at = ""
		data := from
		data.Cache = nil
		if err := f.Hypervisor.MoveVolumes(ctx, data, to); err != nil {
			return err
		}
		return errors.New("failed at move")
	}
	return f.Hypervisor.MoveVolumes(ctx, from, to)
}

func (f *failing) Destroy(ctx context.Context, inst local.InstanceSpec) error {
	if filepath.Base(inst.Dir) == "machine.old" {
		if err := f.fail("remove-old"); err != nil {
			return err
		}
	}
	return f.Hypervisor.Destroy(ctx, inst)
}

// An upgrade that fails at any step leaves the VM listed under its name with
// its volumes, refused a boot and a commit. Once its new disk is whole an
// upgrade from another committed VM is refused, and upgrading again with the
// same one finishes it even once that committed VM is removed; before, it
// is retargeted to another. Either way the volumes are kept and the root is
// new. A

// An upgrade to another pool's committed VM, which only an embedder's
// Engine.From names, caught once its new disk is whole, says to finish it
// as that engine does, never with a --from its pool's engine would look up
// in the pool, and is finished so.
func TestAnInterruptedUpgradeFromAnotherPoolSaysHow(t *testing.T) {
	ctx := context.Background()
	hv := &failing{Hypervisor: &fake.Hypervisor{Agent: buildAgent(t), BaseName: "stock"}}
	d, err := local.New(machine.Config{Dir: t.TempDir(), Pool: "pool-a"}, hv)
	if err != nil {
		t.Fatal(err)
	}
	server, err := d.InPool("server")
	if err != nil {
		t.Fatal(err)
	}
	b, err := server.Create(ctx, machine.InstanceSpec{Name: "base"})
	if err != nil {
		t.Fatal(err)
	}
	write(t, server, b, rootDir(), "marker.txt", "base")
	base, err := server.Commit(ctx, b)
	if err != nil {
		t.Fatal(err)
	}
	dev, err := d.Create(ctx, machine.InstanceSpec{Name: "dev", Data: &machine.DataVolume{}})
	if err != nil {
		t.Fatal(err)
	}
	hv.at = "move"
	if _, err := d.Upgrade(ctx, dev, &base); err == nil {
		t.Fatal("an upgrade failing at move succeeded")
	}
	_, err = d.Boot(ctx, dev, machine.BootOptions{})
	if err == nil || !strings.Contains(err.Error(), "base of pool server") || strings.Contains(err.Error(), "--from base") {
		t.Fatalf("booting a VM caught between its roots: %v, want it to name base of pool server, never --from base", err)
	}
	// What a --from base would ask of the pool's engine: a VM of the pool's
	// own, which is not the recorded root.
	if _, err := d.Upgrade(ctx, dev, &machine.Instance{Name: "base"}); err == nil {
		t.Fatal("an upgrade to the pool's own base finished one to the server's")
	}
	upgraded, err := d.Upgrade(ctx, dev, &base)
	if err != nil {
		t.Fatalf("finish the upgrade as the embedder's engine does: %v", err)
	}
	if upgraded.ID != dev.ID || upgraded.From != "base" || upgraded.FromPool != "server" || upgraded.Pool != "pool-a" {
		t.Fatalf("upgraded %+v", upgraded)
	}
	m, err := d.Boot(ctx, upgraded, machine.BootOptions{})
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = m.Kill(context.Background()) }()
	if got := read(t, m, rootDir()+"/marker.txt"); got != "base" {
		t.Fatalf("the upgraded VM's root reads %q, want the server's base's", got)
	}
}

// VM caught between its roots is removed whole.
func TestAnInterruptedUpgradeIsFinishedAgain(t *testing.T) {
	ctx := context.Background()
	agent := buildAgent(t)
	for _, step := range []string{"prepare", "move", "remove-old"} {
		t.Run(step, func(t *testing.T) {
			hv := &failing{Hypervisor: &fake.Hypervisor{Agent: agent, BaseName: "stock"}}
			d, err := local.New(machine.Config{Dir: t.TempDir()}, hv)
			if err != nil {
				t.Fatal(err)
			}
			committed := func(name, marker string) machine.Instance {
				t.Helper()
				inst, err := d.Create(ctx, machine.InstanceSpec{Name: name})
				if err != nil {
					t.Fatal(err)
				}
				write(t, d, inst, rootDir(), "marker.txt", marker)
				c, err := d.Commit(ctx, inst)
				if err != nil {
					t.Fatal(err)
				}
				return c
			}
			one, two := committed("one", "one"), committed("two", "two")
			dev, err := d.Create(ctx, machine.InstanceSpec{Name: "dev", From: &one, Data: &machine.DataVolume{}, Cache: &machine.CacheVolume{Key: "k"}})
			if err != nil {
				t.Fatal(err)
			}
			write(t, d, dev, machine.VolumePath(dev.GuestOS, machine.VolumeData), "data.txt", "data")
			write(t, d, dev, machine.VolumePath(dev.GuestOS, machine.VolumeCache), "cache.txt", "cache")
			write(t, d, dev, rootDir(), "old.txt", "old")

			hv.at = step
			if _, err := d.Upgrade(ctx, dev, &two); err == nil {
				t.Fatalf("an upgrade failing at %s succeeded", step)
			}
			got, err := d.Instance(ctx, "dev")
			if err != nil || got.ID != dev.ID || got.Data == nil || got.Cache == nil {
				t.Fatalf("a VM caught between its roots is found as %+v, %v", got, err)
			}
			if list, err := d.Instances(ctx); err != nil || len(list) != 3 {
				t.Fatalf("VMs while one is caught between its roots: %+v, %v", list, err)
			}
			if _, err := d.Boot(ctx, dev, machine.BootOptions{}); err == nil {
				t.Fatal("booted a VM caught between its roots")
			}
			if _, err := d.Commit(ctx, dev); err == nil {
				t.Fatal("committed a VM caught between its roots")
			}
			want, marker := two, "two"
			if step == "prepare" {
				// The new disk was never whole, so the old one is as it
				// was, and another root takes the place of one removed.
				if err := d.Destroy(ctx, two); err != nil {
					t.Fatal(err)
				}
				if _, err := d.Upgrade(ctx, dev, &two); !errors.Is(err, machine.ErrNotFound) {
					t.Fatalf("an upgrade from a removed committed VM, its new disk not made: %v", err)
				}
				want, marker = one, "one"
			} else {
				if _, err := d.Upgrade(ctx, dev, &one); err == nil {
					t.Fatal("upgraded a VM caught between its roots to another committed VM")
				}
				// The new disk is whole, so the committed VM it was copied
				// from is needed no more to finish.
				if err := d.Destroy(ctx, two); err != nil {
					t.Fatal(err)
				}
			}
			upgraded, err := d.Upgrade(ctx, dev, &want)
			if err != nil {
				t.Fatalf("finish the upgrade: %v", err)
			}
			if upgraded.ID != dev.ID || upgraded.From != want.Name {
				t.Fatalf("upgraded %+v", upgraded)
			}
			m, err := d.Boot(ctx, upgraded, machine.BootOptions{})
			if err != nil {
				t.Fatal(err)
			}
			defer func() { _ = m.Kill(context.Background()) }()
			for path, want := range map[string]string{
				machine.VolumePath(dev.GuestOS, machine.VolumeData) + "/data.txt":   "data",
				machine.VolumePath(dev.GuestOS, machine.VolumeCache) + "/cache.txt": "cache",
				rootDir() + "/marker.txt": marker,
			} {
				if got := read(t, m, path); got != want {
					t.Errorf("%s reads %q, want %q", path, got, want)
				}
			}
			if stream, err := m.CopyFrom(ctx, rootDir()+"/old.txt"); err == nil {
				stream.Close()
				t.Error("the upgraded VM kept its old root's old.txt")
			}
			_ = m.Kill(ctx)

			// Caught between its roots again, and removed whole.
			hv.at = step
			if _, err := d.Upgrade(ctx, upgraded, &one); err == nil {
				t.Fatalf("an upgrade failing at %s succeeded", step)
			}
			if err := d.Destroy(ctx, upgraded); err != nil {
				t.Fatal(err)
			}
			if _, err := d.Instance(ctx, "dev"); !errors.Is(err, machine.ErrNotFound) {
				t.Fatalf("a removed VM: %v, want ErrNotFound", err)
			}
			if _, err := os.Stat(filepath.Join(d.Dir(), "instances", dev.ID)); !errors.Is(err, os.ErrNotExist) {
				t.Fatalf("a removed VM's directory: %v", err)
			}
		})
	}
}

func buildAgent(t *testing.T) string {
	t.Helper()
	agent := filepath.Join(t.TempDir(), "disco-vm")
	if runtime.GOOS == "windows" {
		agent += ".exe"
	}
	if out, err := exec.CommandContext(t.Context(), "go", "build", "-o", agent, "../../../cmd/disco-vm").CombinedOutput(); err != nil {
		t.Fatalf("%v\n%s", err, out)
	}
	return agent
}

// write boots the stopped inst, writes a file into dir, and shuts it down.
func write(t *testing.T, d machine.Driver, inst machine.Instance, dir, name, content string) {
	t.Helper()
	ctx := context.Background()
	m, err := d.Boot(ctx, inst, machine.BootOptions{})
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = m.Kill(context.Background()) }()
	var buf bytes.Buffer
	tw := tar.NewWriter(&buf)
	_ = tw.WriteHeader(&tar.Header{Name: name, Mode: 0o644, Size: int64(len(content)), Typeflag: tar.TypeReg})
	_, _ = tw.Write([]byte(content))
	_ = tw.Close()
	if err := m.CopyTo(ctx, dir, &buf); err != nil {
		t.Fatal(err)
	}
	if err := m.Shutdown(ctx, false); err != nil {
		t.Fatal(err)
	}
	<-m.Done()
}

func read(t *testing.T, g machine.Guest, path string) string {
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

// rootDir is a directory on a fake guest's root, which is the host's OS.
func rootDir() string {
	if runtime.GOOS == "windows" {
		return `C:\marker`
	}
	return "/marker"
}
