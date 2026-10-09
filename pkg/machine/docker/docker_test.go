package docker

import (
	"bufio"
	"bytes"
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

// ImageEnv names the image to install a base layer from, and BaseEnv a layer
// directory whose layer.json names an installed one, which skips Install.
// Either runs the conformance suite against the Docker daemon $DOCKER_HOST
// names.
const (
	ImageEnv = "DISCO_VM_DOCKER_IMAGE"
	BaseEnv  = "DISCO_VM_DOCKER_BASE"
)

// TestConformance runs the conformance suite against a real Docker daemon. It
// needs a rootful daemon on a cgroup v2 host, so it runs only where ImageEnv
// or BaseEnv is set.
func TestConformance(t *testing.T) {
	image, base := os.Getenv(ImageEnv), os.Getenv(BaseEnv)
	if image == "" && base == "" {
		t.Skipf("set %s (an image, such as debian:trixie) or %s to run the conformance suite against docker", ImageEnv, BaseEnv)
	}
	cfg := machinetest.Config{
		Boot:    machine.BootOptions{CPUs: 2, Memory: 2 << 30},
		Timeout: 2 * time.Minute,
		// Not /tmp, which the guest's systemd may clean at boot.
		ScratchDir: "/var/lib",
	}
	if base != "" {
		cfg.Base = &machine.Layer{ID: "base", Dir: base}
	} else {
		cfg.Install = machine.InstallSpec{
			GuestOS: machine.Linux, Media: "latest", Agent: linuxAgent(t),
			Options: map[string]string{"image": image}, Log: testLog{t},
		}
	}
	machinetest.Run(t, New(), cfg)
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

type testLog struct{ t *testing.T }

func (l testLog) Write(p []byte) (int, error) {
	l.t.Log(strings.TrimRight(string(p), "\n"))
	return len(p), nil
}
