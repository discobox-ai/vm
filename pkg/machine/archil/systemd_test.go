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
	"slices"
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

// TestAStartRegistersNothing: a start installs and registers nothing, so a
// VM whose guest has no systemd service fails it, once systemd has not been
// launched, and is not left running.
func TestAStartRegistersNothing(t *testing.T) {
	if runtime.GOOS != "linux" {
		t.Skip("the fake Archil's guests run on the host, and Archil's guests are linux")
	}
	limit := launchLimit
	launchLimit = 2 * time.Second
	t.Cleanup(func() { launchLimit = limit })
	f := newFakeAPI(t, t.TempDir())
	ctx := context.Background()
	d := f.driver(t, "nostart", "debian:sid")
	inst, err := d.Create(ctx, machine.InstanceSpec{Name: "dev"})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = d.Destroy(context.Background(), inst) })
	f.mu.Lock()
	s := f.sandboxes[0]
	f.mu.Unlock()
	service := filepath.Join(s.root, fakeServices, systemdService)
	if err := os.Remove(service); err != nil {
		t.Fatal(err)
	}
	if _, err := d.Start(ctx, inst, machine.BootOptions{}); err == nil || !strings.Contains(err.Error(), "systemd is not running") {
		t.Fatalf("Start of a VM with no systemd service: %v, want systemd not running", err)
	}
	if fileExists(service) {
		t.Error("a start registered systemd")
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	if s.status != "stopped" {
		t.Errorf("a refused start left the sandbox %s", s.status)
	}
}

// TestCreateAndStartWaitOnlyForSystemdToLaunch: Create and Start return once
// Archil's supervisor has launched systemd, which the fake does a while after
// each boot, and not once its boot has finished, which the fake's never does.
// A guest command right after either runs in systemd's namespaces.
func TestCreateAndStartWaitOnlyForSystemdToLaunch(t *testing.T) {
	if runtime.GOOS != "linux" {
		t.Skip("the fake Archil's guests run on the host, and Archil's guests are linux")
	}
	f := newFakeAPI(t, t.TempDir())
	f.launchDelay = time.Second
	ctx := context.Background()
	d := f.driver(t, "booting", "debian:sid")
	inst, err := d.Create(ctx, machine.InstanceSpec{Name: "dev"})
	if err != nil {
		t.Fatalf("Create of a VM whose systemd is still booting: %v", err)
	}
	t.Cleanup(func() { _ = d.Destroy(context.Background(), inst) })
	start := time.Now()
	m, err := d.Start(ctx, inst, machine.BootOptions{})
	if err != nil {
		t.Fatalf("Start of a VM whose systemd is still booting: %v", err)
	}
	if took := time.Since(start); took < f.launchDelay {
		t.Errorf("Start returned in %s, before systemd was launched", took)
	}
	var out, stderr bytes.Buffer
	if code, err := machine.Run(ctx, m, machine.ExecRequest{Argv: []string{"echo", "hi"}}, nil, &out, &stderr); err != nil || code != 0 || out.String() != "hi\n" {
		t.Fatalf("a command right after Start: exit %d, %v: %q %s", code, err, out.String(), stderr.String())
	}
	out.Reset()
	if code, _ := machine.Run(ctx, m, machine.ExecRequest{Argv: []string{"systemctl", "is-system-running"}}, nil, &out, io.Discard); code == 0 || strings.TrimSpace(out.String()) != "starting" {
		t.Errorf("the fake's systemd after Start: exit %d, %q, want it still starting", code, out.String())
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
	// that systemctl reaches it. Start returns before systemd's boot is
	// done, so it waits for that itself, asking again rather than with
	// --wait, which early in a boot returns offline or a bus error.
	pid1 := func(t *testing.T, m machine.Machine) {
		t.Helper()
		if got := run(t, m, "cat", "/proc/1/comm"); got != "systemd" {
			t.Errorf("PID 1 where guest operations run is %q, not systemd", got)
		}
		if got := run(t, m, "sh", "-c", `for i in $(seq 600); do s=$(systemctl is-system-running 2>/dev/null); case $s in running|degraded) break;; esac; sleep 0.2; done; echo "$s"`); got != "running" && got != "degraded" {
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

// TestSystemdServiceEndsAtItsStopSignal: Archil's stop signals each service
// of its supervisor and waits out a grace period before it kills them, so
// the service that runs systemd ends at that signal, SIGTERM, promptly
// rather than holding every stop for the grace period (vm#217). A stop the
// driver asks for (Shutdown, Kill, Commit's) and one it does not (sent
// straight to the API, as Archil's console or a session's limit stops a
// sandbox) each end every service within the fake's grace, or the fake's
// halt fails the test, and the data volume keeps what was written to it
// across them.
func TestSystemdServiceEndsAtItsStopSignal(t *testing.T) {
	image := os.Getenv(ContainerImageEnv)
	if image == "" {
		t.Skipf("set %s (an image with apt-get and no systemd, such as debian:sid) to run archil's guests as Docker containers", ContainerImageEnv)
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
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Minute)
	defer cancel()
	d := f.driver(t, "sig", image)
	inst, err := d.Create(ctx, machine.InstanceSpec{Name: "dev", Data: &machine.DataVolume{}})
	if err != nil {
		t.Fatal(err)
	}
	m, err := d.Start(ctx, inst, machine.BootOptions{})
	if err != nil {
		t.Fatal(err)
	}
	f.mu.Lock()
	ctr := containerOf(f.sandboxes[0])
	id := f.sandboxes[0].id
	f.mu.Unlock()

	// The service's process, as the fake's supervisor records it, is sent
	// SIGTERM, as Archil's supervisor sends it, and the systemd it ran must
	// be gone within a few seconds.
	const signal = `for d in /proc/[0-9]*; do
	[ "$(cat "$d/comm" 2>/dev/null)" = systemd ] || continue
	grep -Eq '^NSpid:[[:space:]]+[0-9]+[[:space:]]+1$' "$d/status" 2>/dev/null || continue
	p=${d#/proc/}; break
done
svc=$(cat /var/lib/fake-archil/services/disco-vm-systemd.pid)
kill -TERM "$svc" || exit
for i in $(seq 50); do kill -0 "$p" 2>/dev/null || { echo "systemd ended after $i tenths of a second"; exit 0; }; sleep 0.1; done
echo "systemd ($p) still runs 5s after its service ($svc, $(cat /proc/$svc/comm)) was sent SIGTERM"; exit 1`
	start := time.Now()
	out, err := exec.CommandContext(ctx, "docker", "exec", ctr, "sh", "-c", signal).CombinedOutput()
	if err != nil {
		t.Fatalf("%s (%v, after %s)", bytes.TrimSpace(out), err, time.Since(start).Round(100*time.Millisecond))
	}
	t.Log(string(bytes.TrimSpace(out)))
	// The supervisor runs the service again.
	if err := m.Kill(ctx); err != nil {
		t.Fatal(err)
	}

	write := func(t *testing.T, m machine.Machine, text string) {
		t.Helper()
		if code, err := machine.Run(ctx, m, machine.ExecRequest{Argv: []string{"sh", "-c", "echo " + text + " > /.discobox/data/f"}}, nil, io.Discard, io.Discard); err != nil || code != 0 {
			t.Fatalf("write to the data volume: exit %d, %v", code, err)
		}
	}
	read := func(t *testing.T, m machine.Machine) string {
		t.Helper()
		var out bytes.Buffer
		if code, err := machine.Run(ctx, m, machine.ExecRequest{Argv: []string{"cat", "/.discobox/data/f"}}, nil, &out, io.Discard); err != nil || code != 0 {
			t.Fatalf("read the data volume: exit %d, %v", code, err)
		}
		return strings.TrimSpace(out.String())
	}
	timed := func(t *testing.T, what string, stop func() error) {
		t.Helper()
		start := time.Now()
		if err := stop(); err != nil {
			t.Fatalf("%s: %v", what, err)
		}
		if _, err := d.api.waitFor(ctx, id, "stopped"); err != nil {
			t.Fatal(err)
		}
		// The fake's halt fails the test where a service outlived its
		// grace; the time is a box's under load, logged.
		t.Logf("%s took %s", what, time.Since(start).Round(100*time.Millisecond))
	}

	// The driver's Shutdown: flushed, systemd killed, then Archil's stop.
	if m, err = d.Start(ctx, inst, machine.BootOptions{}); err != nil {
		t.Fatal(err)
	}
	write(t, m, "one")
	timed(t, "Shutdown", func() error { return m.Shutdown(ctx, false) })

	// Archil's stop alone, which the driver did not ask for.
	if m, err = d.Start(ctx, inst, machine.BootOptions{}); err != nil {
		t.Fatal(err)
	}
	if got := read(t, m); got != "one" {
		t.Errorf("the data volume holds %q after a Shutdown, want one", got)
	}
	write(t, m, "two")
	timed(t, "Archil's own stop", func() error { return d.api.stop(ctx, id) })

	if m, err = d.Start(ctx, inst, machine.BootOptions{}); err != nil {
		t.Fatal(err)
	}
	if got := read(t, m); got != "two" {
		t.Errorf("the data volume holds %q after Archil's stop, want two", got)
	}
	timed(t, "Kill", func() error { return m.Kill(ctx) })
	// Commit starts the sandbox with no mounts and stops it again, a stop
	// of the driver's own.
	timed(t, "Commit", func() error { _, err := d.Commit(ctx, inst); return err })
}

// TestAStopKillsSystemdThenFlushes: every stop of a running VM first runs
// haltScript, which kills systemd and flushes, so that Archil's stop waits
// for no shutdown in the guest. Shutdown flushes the data volume and asks no
// stop when that fails; Kill flushes none; the driver's own stops flush it
// where it is mounted, as a start whose systemd is not launched has it.
func TestAStopKillsSystemdThenFlushes(t *testing.T) {
	if runtime.GOOS != "linux" {
		t.Skip("the fake Archil's guests run on the host, and Archil's guests are linux")
	}
	ctx := context.Background()
	f := newFakeAPI(t, t.TempDir())
	d := f.driver(t, "halt", "debian:sid")
	inst, err := d.Create(ctx, machine.InstanceSpec{Name: "dev", Data: &machine.DataVolume{}})
	if err != nil {
		t.Fatal(err)
	}
	f.mu.Lock()
	root := f.sandboxes[0].root
	f.mu.Unlock()
	stops := func(t *testing.T, want ...string) {
		t.Helper()
		b, err := os.ReadFile(filepath.Join(root, fakeStops))
		if err != nil {
			t.Fatal(err)
		}
		if got := strings.Fields(strings.ReplaceAll(string(b), " ", "_")); !slices.Equal(got, want) {
			t.Errorf("before each stop, the guest saw %q, want %q", got, want)
		}
	}
	// Create stops the sandbox it booted, with no data volume mounted.
	stops(t, "halt", "stop")

	m, err := d.Start(ctx, inst, machine.BootOptions{})
	if err != nil {
		t.Fatal(err)
	}
	data := machine.VolumePath(machine.Linux, machine.VolumeData)
	if err := os.WriteFile(filepath.Join(root, fakeSyncFails), nil, 0o644); err != nil {
		t.Fatal(err)
	}
	if err := m.Shutdown(ctx, false); err == nil || !strings.Contains(err.Error(), "archil sync") {
		t.Errorf("Shutdown whose flush fails: %v, want archil sync's error", err)
	}
	stops(t, "halt", "stop")
	if err := os.Remove(filepath.Join(root, fakeSyncFails)); err != nil {
		t.Fatal(err)
	}
	if err := m.Shutdown(ctx, false); err != nil {
		t.Fatal(err)
	}
	<-m.Done()
	stops(t, "halt", "stop", "halt_"+data, "stop")

	if m, err = d.Start(ctx, inst, machine.BootOptions{}); err != nil {
		t.Fatal(err)
	}
	if err := m.Kill(ctx); err != nil {
		t.Fatal(err)
	}
	stops(t, "halt", "stop", "halt_"+data, "stop", "halt", "stop")

	// A start whose systemd is not launched stops the sandbox it started,
	// its data volume mounted.
	limit := launchLimit
	launchLimit = 2 * time.Second
	t.Cleanup(func() { launchLimit = limit })
	if err := os.Remove(filepath.Join(root, fakeServices, systemdService)); err != nil {
		t.Fatal(err)
	}
	if _, err := d.Start(ctx, inst, machine.BootOptions{}); err == nil {
		t.Fatal("Start of a VM whose systemd does not run succeeded")
	}
	stops(t, "halt", "stop", "halt_"+data, "stop", "halt", "stop", "halt_"+data, "stop")
}
