package fake_test

import (
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
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
	// no-commit is a driver that does not report Capabilities.Commit, so
	// the suite checks its refusals and runs everything else fresh (ADR
	// 26-11-03-868). install caps running guests at two, as vz does macOS
	// guests, in this process and in the shims it spawns.
	for _, from := range []struct {
		name       string
		install    machine.InstallSpec
		noCommit   bool
		maxRunning int
	}{
		{"install", machine.InstallSpec{GuestOS: machine.OS(runtime.GOOS), Media: "none"}, false, 2},
		{"fresh", machine.InstallSpec{}, false, 0},
		{"no-commit", machine.InstallSpec{}, true, 0},
	} {
		t.Run(from.name, func(t *testing.T) {
			dir := t.TempDir()
			hv := &fake.Hypervisor{Agent: agent, BaseName: "stock", NoCommit: from.noCommit}
			// The shim is given the configuration its starter has (ADR
			// 26-11-11-215 §6).
			config := "{}"
			if from.maxRunning > 0 {
				config = `{"maxRunning":` + strconv.Itoa(from.maxRunning) + `}`
				hv.MaxRunning = map[machine.OS]int{machine.OS(runtime.GOOS): from.maxRunning}
			}
			newDriver := func(keys machine.KeySource) machine.Driver {
				driver, err := local.New(machine.Config{
					Dir:          dir,
					ShimCommand:  []string{agent, "--root", dir, "--driver", "fake", "--driver-config", config, "shim"},
					Keys:         keys,
					DriverConfig: json.RawMessage(config),
				}, hv)
				if err != nil {
					t.Fatal(err)
				}
				return driver
			}
			machinetest.Run(t, newDriver(nil), machinetest.Config{
				Install:    from.install,
				Timeout:    30 * time.Second,
				ScratchDir: "/scratch",
				GuestAgent: agent,
				Keyed:      newDriver,
			})
		})
	}
}
