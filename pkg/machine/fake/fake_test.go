package fake_test

import (
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"testing"
	"time"

	"github.com/discobox-ai/vm/pkg/machine"
	"github.com/discobox-ai/vm/pkg/machine/fake"
	"github.com/discobox-ai/vm/pkg/machine/machinetest"
)

func TestConformance(t *testing.T) {
	// The fake guest is the disco-vm binary, so build it (or take a prebuilt
	// one, as internal/e2e does).
	agent := os.Getenv("DISCO_VM_TEST_BINARY")
	if agent == "" {
		agent = filepath.Join(t.TempDir(), "disco-vm")
		if runtime.GOOS == "windows" {
			agent += ".exe"
		}
		build := exec.Command("go", "build", "-o", agent, "../../../cmd/disco-vm")
		build.Stdout, build.Stderr = os.Stdout, os.Stderr
		if err := build.Run(); err != nil {
			t.Fatal(err)
		}
	}
	machinetest.Run(t, &fake.Driver{Agent: agent}, machinetest.Config{
		Install:    machine.InstallSpec{GuestOS: machine.OS(runtime.GOOS), Media: "none"},
		Timeout:    30 * time.Second,
		ScratchDir: "/scratch",
	})
}
