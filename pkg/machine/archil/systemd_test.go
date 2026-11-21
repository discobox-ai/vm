package archil

import (
	"archive/tar"
	"bytes"
	"context"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/discobox-ai/vm/pkg/machine"
)

// TestABaseWithoutSystemdIsRefused: a base with no systemd and nothing to
// install it with fails Create, naming what it lacks, and leaves nothing
// behind.
func TestABaseWithoutSystemdIsRefused(t *testing.T) {
	if runtime.GOOS != "linux" {
		t.Skip("the fake Archil's guests run on the host, and Archil's guests are linux")
	}
	f := newFakeAPI(t, t.TempDir())
	d := f.driver(t, "nosystemd", "alpine:3")
	_, err := d.Create(context.Background(), machine.InstanceSpec{Name: "dev", Data: &machine.DataVolume{}})
	if err == nil || !strings.Contains(err.Error(), "/lib/systemd/systemd") {
		t.Fatalf("Create of a base without systemd: %v, want an error naming /lib/systemd/systemd", err)
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	if len(f.sandboxes) != 0 || len(f.disks) != 0 {
		t.Errorf("a refused Create left %d sandboxes and %d disks", len(f.sandboxes), len(f.disks))
	}
}

// TestAVMFromBeforeSystemdGetsItAtStart: a VM whose guest has no systemd
// service, made before archil's guests ran systemd, has it registered at its
// next start, and a guest operation reaches it.
func TestAVMFromBeforeSystemdGetsItAtStart(t *testing.T) {
	if runtime.GOOS != "linux" {
		t.Skip("the fake Archil's guests run on the host, and Archil's guests are linux")
	}
	f := newFakeAPI(t, t.TempDir())
	ctx := context.Background()
	d := f.driver(t, "old", "debian:sid")
	inst, err := d.Create(ctx, machine.InstanceSpec{Name: "old"})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = d.Destroy(context.Background(), inst) })
	f.mu.Lock()
	service := filepath.Join(f.sandboxes[0].root, fakeServices, systemdService)
	f.mu.Unlock()
	if err := os.Remove(service); err != nil {
		t.Fatal(err)
	}
	m, err := d.Start(ctx, inst, machine.BootOptions{})
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = m.Kill(ctx) }()
	if _, err := os.Stat(service); err != nil {
		t.Errorf("Start left the guest with no systemd service: %v", err)
	}
	if _, err := m.Info(ctx); err != nil {
		t.Errorf("a guest operation after Start: %v", err)
	}
}

// TestAStartInstallsNothing: a VM whose root has no systemd, made before
// archil's guests ran it, is refused at its start, which installs nothing,
// and is not left running.
func TestAStartInstallsNothing(t *testing.T) {
	if runtime.GOOS != "linux" {
		t.Skip("the fake Archil's guests run on the host, and Archil's guests are linux")
	}
	f := newFakeAPI(t, t.TempDir())
	ctx := context.Background()
	d := f.driver(t, "old", "debian:sid")
	inst, err := d.Create(ctx, machine.InstanceSpec{Name: "old"})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = d.Destroy(context.Background(), inst) })
	f.mu.Lock()
	s := f.sandboxes[0]
	f.mu.Unlock()
	if err := os.RemoveAll(filepath.Join(s.root, "lib/systemd")); err != nil {
		t.Fatal(err)
	}
	if err := os.RemoveAll(filepath.Join(s.root, fakeServices)); err != nil {
		t.Fatal(err)
	}
	if _, err := d.Start(ctx, inst, machine.BootOptions{}); err == nil || !strings.Contains(err.Error(), "/lib/systemd/systemd") {
		t.Fatalf("Start of a VM with no systemd: %v, want an error naming /lib/systemd/systemd", err)
	}
	if fileExists(filepath.Join(s.root, "lib/systemd/systemd")) {
		t.Error("a start installed systemd")
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	if s.status != "stopped" {
		t.Errorf("a refused start left the sandbox %s", s.status)
	}
}

// TestSystemdInAContainer runs the driver against the fake whose sandboxes
// are Docker containers of ContainerImageEnv's image, whose PID 1 is sh, not
// systemd, as an Archil guest's is Archil's own init: a fresh VM of a base
// with no systemd gets it installed at Create; systemd is PID 1 where guest
// operations run, systemctl reaches it, a unit sees a file copied under /run,
// the data volume, and a mount made after systemd started; systemd runs
// again after a stop and a start, and in a VM made from a committed one;
// and a VM fenced to nothing can't install it, so its Create fails.
func TestSystemdInAContainer(t *testing.T) {
	image := os.Getenv(ContainerImageEnv)
	if image == "" {
		t.Skipf("set %s (an image with apt-get and no systemd, such as debian:sid) to run archil's guests as Docker containers", ContainerImageEnv)
	}
	if err := exec.CommandContext(context.Background(), "docker", "version").Run(); err != nil {
		t.Fatalf("%s is set and docker does not answer: %v", ContainerImageEnv, err)
	}
	f := newFakeAPI(t, t.TempDir())
	f.containers = newFakeContainers(t, f, image)
	t.Cleanup(func() {
		f.mu.Lock()
		defer f.mu.Unlock()
		for _, s := range f.sandboxes {
			f.containers.remove(s)
		}
	})
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Minute)
	defer cancel()
	d := f.driver(t, "ctr", image)

	run := func(t *testing.T, m machine.Machine, argv ...string) string {
		t.Helper()
		var out, stderr bytes.Buffer
		code, err := machine.Run(ctx, m, machine.ExecRequest{Argv: argv}, nil, &out, &stderr)
		if err != nil || code != 0 {
			t.Fatalf("%q: exit %d, %v: %s", argv, code, err, stderr.String())
		}
		return strings.TrimSpace(out.String())
	}
	copyIn := func(t *testing.T, m machine.Machine, dir, name, text string) {
		t.Helper()
		var b bytes.Buffer
		tw := tar.NewWriter(&b)
		if err := tw.WriteHeader(&tar.Header{Name: name, Mode: 0o644, Size: int64(len(text)), Typeflag: tar.TypeReg}); err != nil {
			t.Fatal(err)
		}
		if _, err := tw.Write([]byte(text)); err != nil {
			t.Fatal(err)
		}
		if err := tw.Close(); err != nil {
			t.Fatal(err)
		}
		if err := m.CopyTo(ctx, dir, &b); err != nil {
			t.Fatal(err)
		}
	}
	// pid1 checks that systemd is PID 1 where guest operations run, and
	// that systemctl reaches it.
	pid1 := func(t *testing.T, m machine.Machine) {
		t.Helper()
		if got := run(t, m, "cat", "/proc/1/comm"); got != "systemd" {
			t.Errorf("PID 1 where guest operations run is %q, not systemd", got)
		}
		if got := run(t, m, "sh", "-c", "systemctl is-system-running || true"); got != "running" && got != "degraded" {
			t.Errorf("systemctl is-system-running: %q", got)
		}
	}

	inst, err := d.Create(ctx, machine.InstanceSpec{Name: "dev", Data: &machine.DataVolume{}})
	if err != nil {
		t.Fatal(err)
	}
	m, err := d.Start(ctx, inst, machine.BootOptions{})
	if err != nil {
		t.Fatal(err)
	}
	pid1(t, m)
	// Another account's command runs there too, under runuser.
	var out bytes.Buffer
	if code, err := machine.Run(ctx, m, machine.ExecRequest{Argv: []string{"sh", "-c", "id -un; cat /proc/1/comm"}, User: "nobody"}, nil, &out, io.Discard); err != nil || code != 0 || out.String() != "nobody\nsystemd\n" {
		t.Errorf("exec as nobody: exit %d, %v: %q", code, err, out.String())
	}

	f.mu.Lock()
	ctr := containerOf(f.sandboxes[0])
	f.mu.Unlock()
	// The container's own PID 1 is sh, running fakeInit.
	if out, err := exec.CommandContext(ctx, "docker", "exec", ctr, "readlink", "/proc/1/exe").Output(); err != nil || !strings.HasSuffix(strings.TrimSpace(string(out)), "sh") {
		t.Errorf("the container's own PID 1 is %q (%v), not sh", out, err)
	}
	copyIn(t, m, "/run/disco-vm-test", "marker", "from-run\n")
	copyIn(t, m, "/.discobox/data", "data", "from-data\n")
	// A mount made outside systemd's namespaces once it runs, as Archil's
	// client may mount a disk.
	if out, err := exec.CommandContext(ctx, "docker", "exec", ctr, "sh", "-c",
		"mkdir -p /mnt/late && mount -t tmpfs late /mnt/late && echo from-late > /mnt/late/f").CombinedOutput(); err != nil {
		t.Fatalf("a late mount: %v: %s", err, out)
	}
	got := run(t, m, "systemd-run", "--quiet", "--wait", "--pipe", "--", "cat", "/run/disco-vm-test/marker", "/.discobox/data/data", "/mnt/late/f")
	if got != "from-run\nfrom-data\nfrom-late" {
		t.Errorf("a unit read %q, want the file copied under /run, the data volume's, and the late mount's", got)
	}

	// A guest restart: stopped, then started again.
	if err := m.Shutdown(ctx, false); err != nil {
		t.Fatal(err)
	}
	select {
	case <-m.Done():
	case <-ctx.Done():
		t.Fatal("the VM did not stop")
	}
	if m, err = d.Start(ctx, inst, machine.BootOptions{}); err != nil {
		t.Fatal(err)
	}
	pid1(t, m)
	if got := run(t, m, "cat", "/.discobox/data/data"); got != "from-data" {
		t.Errorf("the data volume after a restart holds %q", got)
	}
	if err := m.Kill(ctx); err != nil {
		t.Fatal(err)
	}

	// A VM made from the committed one runs systemd with nothing installed.
	committed, err := d.Commit(ctx, inst)
	if err != nil {
		t.Fatal(err)
	}
	copied, err := d.Create(ctx, machine.InstanceSpec{Name: "copy", From: &committed})
	if err != nil {
		t.Fatal(err)
	}
	cm, err := d.Start(ctx, copied, machine.BootOptions{})
	if err != nil {
		t.Fatal(err)
	}
	pid1(t, cm)
	if err := cm.Kill(ctx); err != nil {
		t.Fatal(err)
	}
	for _, i := range []machine.Instance{copied, committed} {
		if err := d.Destroy(ctx, i); err != nil {
			t.Fatal(err)
		}
	}

	// Fenced to nothing, a base without systemd can't install it.
	_, err = d.Create(ctx, machine.InstanceSpec{Name: "fenced", Egress: &machine.Egress{}})
	if err == nil || !strings.Contains(err.Error(), "/lib/systemd/systemd") {
		t.Errorf("Create fenced to nothing of a base without systemd: %v, want an error naming /lib/systemd/systemd", err)
	}
}
