package docker

import (
	"archive/tar"
	"bufio"
	"bytes"
	"context"
	"errors"
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
	"github.com/discobox-ai/vm/pkg/machine/machinetest"
)

// ImageEnv names a registry image, such as debian:sid, to run the
// conformance suite from as the driver's base (machine.Config.Base),
// against the Docker daemon $DOCKER_HOST names (Config.Host).
const ImageEnv = "DISCO_VM_DOCKER_IMAGE"

// TestConformance runs the conformance suite against a real Docker daemon. It
// needs a rootful daemon on a cgroup v2 host, so it runs only where ImageEnv
// is set.
func TestConformance(t *testing.T) {
	image := os.Getenv(ImageEnv)
	if image == "" {
		t.Skipf("set %s (an image, such as debian:sid) to run the conformance suite against docker", ImageEnv)
	}
	d := envNew(t, machine.Config{Base: image})
	d.Agent = linuxAgent(t)
	machinetest.Run(t, d, machinetest.Config{
		CPUs:    2,
		Memory:  2 << 30,
		Timeout: 2 * time.Minute,
		// Not /tmp, which the guest's systemd may clean at boot.
		ScratchDir: "/var/lib",
	})
}

// TestConformanceThroughAnAgent runs the conformance suite against the same
// daemon reached as GuestDaemon reaches a host VM's: every request and exec
// stream is an Exec, through a disco-vm agent, of the linux agent's
// `disco-vm pipe` to the daemon's socket. The agent runs on this host, so it
// proves the transport, not Docker in a VM. It runs only where ImageEnv is
// set, against a unix:// $DOCKER_HOST (or the default).
func TestConformanceThroughAnAgent(t *testing.T) {
	image := os.Getenv(ImageEnv)
	if image == "" {
		t.Skipf("set %s (an image, such as debian:sid) to run the conformance suite against docker through an agent", ImageEnv)
	}
	host := os.Getenv(HostEnv)
	if host == "" {
		host = defaultHost
	}
	socket, ok := strings.CutPrefix(host, "unix://")
	if !ok {
		t.Skipf("%s=%s: the relay is run against a unix socket", HostEnv, host)
	}
	agent := linuxAgent(t)
	d, err := NewWithDaemon(machine.Config{Base: image, Pool: "relay"}, GuestDaemon(startTestAgent(t), agent, "unix:"+socket))
	if err != nil {
		t.Fatal(err)
	}
	d.Agent = agent
	machinetest.Run(t, d, machinetest.Config{
		CPUs:       2,
		Memory:     2 << 30,
		Timeout:    2 * time.Minute,
		ScratchDir: "/var/lib",
	})
}

// mustNew is New, failing the test on an error.
func mustNew(t *testing.T, cfg machine.Config) *Driver {
	t.Helper()
	d, err := New(cfg)
	if err != nil {
		t.Fatal(err)
	}
	return d
}

// envNew is New with the configuration the command line reads from the
// environment, so a daemon test reaches the daemon $DOCKER_HOST names.
func envNew(t *testing.T, cfg machine.Config) *Driver {
	t.Helper()
	config, err := machine.ConfigFromValues("docker", machine.ConfigFromEnv("docker"))
	if err != nil {
		t.Fatal(err)
	}
	cfg.DriverConfig = config
	return mustNew(t, cfg)
}

// TestNoBase refuses a fresh VM where no base is configured, before it asks
// the daemon anything.
func TestNoBase(t *testing.T) {
	_, err := mustNew(t, machine.Config{}).Create(t.Context(), machine.InstanceSpec{Name: "fresh"})
	if !errors.Is(err, machine.ErrUnsupported) || !strings.Contains(err.Error(), machine.BaseEnv) {
		t.Fatalf("a fresh VM with no base: %v", err)
	}
}

// TestCommittedRefused refuses every operation of a running VM on a
// committed one, a Docker image with no container, before it asks the
// daemon anything.
func TestCommittedRefused(t *testing.T) {
	d := mustNew(t, machine.Config{})
	inst := machine.Instance{ID: "sha256:0", Name: "frozen"}
	if _, err := d.Boot(t.Context(), inst, machine.BootOptions{}); !errors.Is(err, machine.ErrCommitted) {
		t.Errorf("boot: %v", err)
	}
	if _, err := d.Start(t.Context(), inst, machine.BootOptions{}); !errors.Is(err, machine.ErrCommitted) {
		t.Errorf("start: %v", err)
	}
	if _, err := d.Attach(t.Context(), inst); !errors.Is(err, machine.ErrNotRunning) {
		t.Errorf("attach: %v", err)
	}
	if _, err := d.Commit(t.Context(), inst); !errors.Is(err, machine.ErrCommitted) {
		t.Errorf("commit: %v", err)
	}
	if _, err := d.Service(t.Context(), inst); !errors.Is(err, machine.ErrNotRunning) {
		t.Errorf("service: %v", err)
	}
	if _, err := d.SaveCache(t.Context(), inst, machine.CacheSave{Key: "k"}); !errors.Is(err, machine.ErrCommitted) {
		t.Errorf("save cache: %v", err)
	}
}

func TestDemux(t *testing.T) {
	frame := func(stream byte, s string) []byte {
		n := len(s)
		return append([]byte{stream, 0, 0, 0, byte(n >> 24), byte(n >> 16), byte(n >> 8), byte(n)}, s...)
	}
	var stream []byte
	stream = append(stream, frame(1, "hello ")...)
	stream = append(stream, frame(2, "oops")...)
	stream = append(stream, frame(1, "world")...)

	c := &execConn{r: bufio.NewReader(bytes.NewReader(stream))}
	got, err := io.ReadAll(c)
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != "hello world" || string(c.stderrBytes()) != "oops" {
		t.Fatalf("stdout %q, stderr %q", got, c.stderrBytes())
	}
}

func TestSplitRef(t *testing.T) {
	for ref, want := range map[string][2]string{
		"debian":                      {"debian", "latest"},
		"debian:sid":                  {"debian", "sid"},
		"localhost:5000/os/debian":    {"localhost:5000/os/debian", "latest"},
		"localhost:5000/os/debian:13": {"localhost:5000/os/debian", "13"},
		"debian@sha256:0123":          {"debian@sha256:0123", ""},
	} {
		if name, tag := splitRef(ref); name != want[0] || tag != want[1] {
			t.Errorf("splitRef(%q) = %q, %q", ref, name, tag)
		}
	}
}

func linuxAgent(t *testing.T) string {
	t.Helper()
	if agent := os.Getenv("DISCO_VM_TEST_BINARY"); agent != "" {
		return agent
	}
	agent := filepath.Join(t.TempDir(), "disco-vm")
	build := exec.CommandContext(t.Context(), "go", "build", "-o", agent, "../../../cmd/disco-vm")
	build.Env = append(os.Environ(), "GOOS=linux", "CGO_ENABLED=0")
	if runtime.GOOS == "linux" {
		build.Env = append(build.Env, "GOARCH="+runtime.GOARCH)
	} else {
		build.Env = append(build.Env, "GOARCH=amd64")
	}
	build.Stdout, build.Stderr = os.Stdout, os.Stderr
	if err := build.Run(); err != nil {
		t.Fatal(err)
	}
	return agent
}

// TestManagedLeavesTheOldOut lists a VM's container and a readying one of
// the pool's, and not a container from before ADR 26-11-03-606, an
// instance of an image, which labelImage names.
func TestManagedLeavesTheOldOut(t *testing.T) {
	d := mustNew(t, machine.Config{Pool: "p"})
	for _, c := range []struct {
		labels map[string]string
		want   bool
	}{
		{map[string]string{labelInstance: "dev", labelPool: "p", labelImage: ""}, true},
		{map[string]string{labelInstance: "dev", labelPool: "p"}, true},
		{map[string]string{labelInstance: "base-0a1b2c", labelPool: "p", labelImage: "debian:sid"}, true},
		{map[string]string{labelInstance: "dev", labelPool: "p", labelImage: "e2e/base"}, false},
		{map[string]string{labelInstance: "base-app", labelPool: "p", labelImage: "e2e/base"}, false},
		{map[string]string{labelInstance: "dev", labelPool: "q"}, false},
		{map[string]string{labelInstance: "dev", "ai.discobox.vm.project": "p"}, false},
	} {
		if got := d.managed(c.labels); got != c.want {
			t.Errorf("managed(%v) = %v, want %v", c.labels, got, c.want)
		}
	}
}

// TestUpgradeInterrupted stops an upgrade after each of its steps: once its
// new container is made, the old one is still the VM, and an upgrade again
// makes the new one again; once the old one is removed, the new one is the
// VM, found by its name and refused a boot, and only an upgrade from the
// same committed VM finishes it. The data volume is kept throughout, and
// Destroy of a VM caught between removes everything. It needs a daemon, as
// TestConformance does.
func TestUpgradeInterrupted(t *testing.T) {
	image := os.Getenv(ImageEnv)
	if image == "" {
		t.Skipf("set %s (an image, such as debian:sid) to run against docker", ImageEnv)
	}
	d := envNew(t, machine.Config{Base: image, Pool: "upgradetest"})
	d.Agent = linuxAgent(t)
	ctx := t.Context()
	cleanup := func(inst machine.Instance) { t.Cleanup(func() { _ = d.Destroy(context.Background(), inst) }) }
	shutdown := func(m machine.Machine) {
		t.Helper()
		if err := m.Shutdown(ctx, false); err != nil {
			t.Fatal(err)
		}
		<-m.Done()
	}
	src, err := d.Create(ctx, machine.InstanceSpec{Name: "src"})
	if err != nil {
		t.Fatal(err)
	}
	cleanup(src)
	committed, err := d.Commit(ctx, src)
	if err != nil {
		t.Fatal(err)
	}
	cleanup(committed)
	// A second committed VM, which outlives the first's removal below.
	src2, err := d.Create(ctx, machine.InstanceSpec{Name: "src2", From: &committed})
	if err != nil {
		t.Fatal(err)
	}
	cleanup(src2)
	committed2, err := d.Commit(ctx, src2)
	if err != nil {
		t.Fatal(err)
	}
	cleanup(committed2)
	vm, err := d.Create(ctx, machine.InstanceSpec{Name: "vm", Data: &machine.DataVolume{}})
	if err != nil {
		t.Fatal(err)
	}
	cleanup(vm)
	m, err := d.Boot(ctx, vm, machine.BootOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if err := m.CopyTo(ctx, machine.VolumePath(machine.Linux, machine.VolumeData), tarOf("kept.txt", "kept")); err != nil {
		t.Fatal(err)
	}
	shutdown(m)

	fail := func(at string) {
		upgradeStep = func(step string) error {
			if step == at {
				return errors.New("interrupted at " + step)
			}
			return nil
		}
	}
	t.Cleanup(func() { upgradeStep = func(string) error { return nil } })

	fail("created")
	if _, err := d.Upgrade(ctx, vm, &committed); err == nil {
		t.Fatal("an upgrade interrupted once its new container was made succeeded")
	}
	if got, err := d.Instance(ctx, "vm"); err != nil || got.ID != vm.ID {
		t.Fatalf("once the new container is made, the VM is %+v, %v; want the old one %s", got, err, vm.ID)
	}
	if _, err := d.Boot(ctx, vm, machine.BootOptions{}); err == nil || !strings.Contains(err.Error(), "unfinished") {
		t.Fatalf("boot of a VM whose upgrade made its new container: %v", err)
	}
	if _, err := d.Commit(ctx, vm); err == nil || !strings.Contains(err.Error(), "unfinished") {
		t.Fatalf("commit of a VM whose upgrade made its new container: %v", err)
	}
	fail("removed")
	if _, err := d.Upgrade(ctx, vm, &committed); err == nil {
		t.Fatal("an upgrade interrupted once the old container was removed succeeded")
	}
	between, err := d.Instance(ctx, "vm")
	if err != nil || between.ID == vm.ID || between.Data == nil {
		t.Fatalf("once the old container is removed, the VM is %+v, %v", between, err)
	}
	list, err := d.Instances(ctx)
	if err != nil || len(slices.DeleteFunc(list, func(i machine.Instance) bool { return i.Name != "vm" })) != 1 {
		t.Fatalf("a VM caught between its containers is listed as %+v: %v", list, err)
	}
	if _, err := d.Boot(ctx, vm, machine.BootOptions{}); err == nil || !strings.Contains(err.Error(), "unfinished") {
		t.Fatalf("boot of a VM caught between its containers: %v", err)
	}
	upgradeStep = func(string) error { return nil }
	if _, err := d.Upgrade(ctx, vm, nil); err == nil || !strings.Contains(err.Error(), "unfinished") {
		t.Fatalf("an upgrade from another root finished one caught between: %v", err)
	}
	upgraded, err := d.Upgrade(ctx, vm, &committed)
	if err != nil {
		t.Fatalf("finish the upgrade: %v", err)
	}
	if upgraded.From != committed.Name || upgraded.Data == nil {
		t.Fatalf("upgraded %+v", upgraded)
	}
	m, err = d.Boot(ctx, vm, machine.BootOptions{})
	if err != nil {
		t.Fatalf("boot the upgraded VM by its Instance from before: %v", err)
	}
	stream, err := m.CopyFrom(ctx, machine.VolumePath(machine.Linux, machine.VolumeData)+"/kept.txt")
	if err != nil {
		t.Fatalf("the upgraded VM lost its data: %v", err)
	}
	stream.Close()
	shutdown(m)

	// An upgrade caught between is finished once the committed VM it is
	// from is gone, which its new container no longer needs.
	fail("created")
	if _, err := d.Upgrade(ctx, upgraded, &committed); err == nil {
		t.Fatal("an interrupted upgrade succeeded")
	}
	if err := d.Destroy(ctx, committed); err != nil {
		t.Fatal(err)
	}
	upgradeStep = func(string) error { return nil }
	if upgraded, err = d.Upgrade(ctx, upgraded, &committed); err != nil {
		t.Fatalf("finish an upgrade from a committed VM since removed: %v", err)
	}

	fail("removed")
	if _, err := d.Upgrade(ctx, upgraded, &committed2); err == nil {
		t.Fatal("an interrupted upgrade succeeded")
	}
	if err := d.Destroy(ctx, upgraded); err != nil {
		t.Fatalf("destroy a VM caught between its containers: %v", err)
	}
	if got, err := d.Instance(ctx, "vm"); !errors.Is(err, machine.ErrNotFound) {
		t.Fatalf("a destroyed VM caught between its containers is %+v, %v", got, err)
	}
	for _, ref := range []string{containerName(d.pool, "vm"), upgradeName(d.pool, "vm")} {
		if _, err := d.api.inspectContainer(ctx, ref); !isNotFound(err) {
			t.Fatalf("container %s outlives its VM: %v", ref, err)
		}
	}
	images, err := d.api.images(ctx, baseRepo)
	if err != nil {
		t.Fatal(err)
	}
	for _, img := range images {
		for _, tag := range img.RepoTags {
			if strings.HasPrefix(tag, baseRepo+":"+baseTag(d.pool, "vm")) {
				t.Errorf("image %s outlives its VM", tag)
			}
		}
	}
}

func tarOf(name, content string) io.Reader {
	var buf bytes.Buffer
	tw := tar.NewWriter(&buf)
	_ = tw.WriteHeader(&tar.Header{Name: name, Mode: 0o644, Size: int64(len(content)), Typeflag: tar.TypeReg})
	_, _ = tw.Write([]byte(content))
	_ = tw.Close()
	return &buf
}
