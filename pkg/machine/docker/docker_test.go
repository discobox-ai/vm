package docker

import (
	"bufio"
	"bytes"
	"errors"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/discobox-ai/vm/pkg/machine"
	"github.com/discobox-ai/vm/pkg/machine/machinetest"
)

// ImageEnv names a registry image, such as debian:trixie, to run the
// conformance suite from as the driver's base (machine.Config.Base),
// against the Docker daemon $DOCKER_HOST names.
const ImageEnv = "DISCO_VM_DOCKER_IMAGE"

// TestConformance runs the conformance suite against a real Docker daemon. It
// needs a rootful daemon on a cgroup v2 host, so it runs only where ImageEnv
// is set.
func TestConformance(t *testing.T) {
	image := os.Getenv(ImageEnv)
	if image == "" {
		t.Skipf("set %s (an image, such as debian:trixie) to run the conformance suite against docker", ImageEnv)
	}
	d := New(machine.Config{Base: image})
	d.Agent = linuxAgent(t)
	machinetest.Run(t, d, machinetest.Config{
		CPUs:    2,
		Memory:  2 << 30,
		Timeout: 2 * time.Minute,
		// Not /tmp, which the guest's systemd may clean at boot.
		ScratchDir: "/var/lib",
	})
}

// TestNoBase refuses a fresh VM where no base is configured, before it asks
// the daemon anything.
func TestNoBase(t *testing.T) {
	_, err := New(machine.Config{}).Create(t.Context(), machine.InstanceSpec{Name: "fresh"})
	if !errors.Is(err, machine.ErrUnsupported) || !strings.Contains(err.Error(), BaseEnv) {
		t.Fatalf("a fresh VM with no base: %v", err)
	}
}

// TestCommittedRefused refuses every operation of a running VM on a
// committed one, a Docker image with no container, before it asks the
// daemon anything.
func TestCommittedRefused(t *testing.T) {
	d := New(machine.Config{})
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
		"debian:trixie":               {"debian", "trixie"},
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
// the project's, and not a container from before ADR 26-11-03-606, an
// instance of an image, which labelImage names.
func TestManagedLeavesTheOldOut(t *testing.T) {
	d := New(machine.Config{Project: "p"})
	for _, c := range []struct {
		labels map[string]string
		want   bool
	}{
		{map[string]string{labelInstance: "dev", labelProject: "p", labelImage: ""}, true},
		{map[string]string{labelInstance: "dev", labelProject: "p"}, true},
		{map[string]string{labelInstance: "base-0a1b2c", labelProject: "p", labelImage: "debian:trixie"}, true},
		{map[string]string{labelInstance: "dev", labelProject: "p", labelImage: "e2e/base"}, false},
		{map[string]string{labelInstance: "base-app", labelProject: "p", labelImage: "e2e/base"}, false},
		{map[string]string{labelInstance: "dev", labelProject: "q"}, false},
	} {
		if got := d.managed(c.labels); got != c.want {
			t.Errorf("managed(%v) = %v, want %v", c.labels, got, c.want)
		}
	}
}
