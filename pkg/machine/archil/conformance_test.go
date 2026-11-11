package archil

import (
	"os"
	"testing"
	"time"

	"github.com/discobox-ai/vm/pkg/machine"
	"github.com/discobox-ai/vm/pkg/machine/machinetest"
)

// ImageEnv names the OCI image the conformance suite runs on Archil as the
// driver's base (machine.Config.Base), such as debian:trixie.
const ImageEnv = "DISCO_VM_ARCHIL_IMAGE"

// TestConformance runs the conformance suite against Archil itself. It needs
// ARCHIL_API_KEY and ImageEnv, so it runs only where both are set; it makes
// sandboxes and disks in a pool of its own and removes them.
func TestConformance(t *testing.T) {
	image := os.Getenv(ImageEnv)
	if image == "" || os.Getenv(APIKeyEnv) == "" {
		t.Skipf("set %s and %s (an image, such as debian:trixie) to run the conformance suite against Archil", APIKeyEnv, ImageEnv)
	}
	config, err := machine.ConfigFromValues("archil", machine.ConfigFromEnv("archil"))
	if err != nil {
		t.Fatal(err)
	}
	d, err := machine.New("archil", machine.Config{Pool: "conformance", Base: image, Keys: machine.KeyFromEnv("archil"), DriverConfig: config})
	if err != nil {
		t.Fatal(err)
	}
	machinetest.Run(t, d, machinetest.Config{
		CPUs:       2,
		Memory:     2 << 30,
		Timeout:    5 * time.Minute,
		ScratchDir: "/var/lib",
	})
}
