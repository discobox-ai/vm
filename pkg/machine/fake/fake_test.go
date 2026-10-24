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
	"github.com/discobox-ai/vm/pkg/machine/local"
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
		build := exec.CommandContext(t.Context(), "go", "build", "-o", agent, "../../../cmd/disco-vm")
		build.Stdout, build.Stderr = os.Stdout, os.Stderr
		if err := build.Run(); err != nil {
			t.Fatal(err)
		}
	}
	catalog := []machine.Base{{Name: "stock", ID: "stock-1", GuestOS: machine.OS(runtime.GOOS), Options: map[string]string{"image": "stock"}}}
	dir := t.TempDir()
	hv := &fake.Hypervisor{Agent: agent, Catalog: catalog}
	newDriver := func(keys machine.KeySource) machine.Driver {
		driver, err := local.New(machine.Config{
			Dir:         dir,
			ShimCommand: []string{agent, "--root", dir, "--driver", "fake", "shim"},
			Keys:        keys,
		}, hv)
		if err != nil {
			t.Fatal(err)
		}
		return driver
	}
	machinetest.Run(t, newDriver(nil), machinetest.Config{
		Install:    machine.InstallSpec{GuestOS: machine.OS(runtime.GOOS), Media: "none"},
		Timeout:    30 * time.Second,
		ScratchDir: "/scratch",
		Keyed:      newDriver,
	})
}
