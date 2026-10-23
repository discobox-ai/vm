//go:build darwin && cgo

package vz

import (
	"net"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/discobox-ai/vm/pkg/machine"
	"github.com/discobox-ai/vm/pkg/machine/local"
	"github.com/discobox-ai/vm/pkg/machine/machinetest"
)

// BaseEnv names an installed layer, by its ID, in the driver's directory
// ($DISCO_VM_ROOT, or the per-OS data directory): a layer an install made,
// which `disco-vm images -a` lists. The conformance suite clones it rather
// than installing macOS on every run, and skips without it.
const BaseEnv = "DISCO_VM_VZ_BASE"

// TestConformance is the vz driver's definition of done. It creates VMs, so
// the test binary has to be signed, and so does the disco-vm that runs as each
// started instance's shim ($DISCO_VM_TEST_BINARY): run it with
// `go tool task test:vz`.
func TestConformance(t *testing.T) {
	base := os.Getenv(BaseEnv)
	if base == "" {
		t.Skipf("$%s names no installed layer", BaseEnv)
	}
	binary := os.Getenv("DISCO_VM_TEST_BINARY")
	if binary == "" {
		t.Fatal("$DISCO_VM_TEST_BINARY names no signed disco-vm to run each started instance's shim")
	}
	dir := machine.DefaultDir()
	driver, err := local.New(machine.Config{
		Dir:         dir,
		ShimCommand: []string{binary, "--root", dir, "--driver", "vz", "shim"},
	}, &Hypervisor{})
	if err != nil {
		t.Fatal(err)
	}
	machinetest.Run(t, driver, machinetest.Config{
		Base:       base,
		CPUs:       4,
		Memory:     8 << 30,
		Timeout:    10 * time.Minute,
		ScratchDir: "/Users/Shared",
	})
}

func TestLeaseFor(t *testing.T) {
	leases := filepath.Join(t.TempDir(), "leases")
	data := `{
	name=old
	ip_address=192.168.64.2
	hw_address=1,b2:7:43:8c:71:83
	identifier=1,b2:7:43:8c:71:83
	lease=0x6ab20000
}
{
	name=other
	ip_address=192.168.64.3
	hw_address=1,3e:cb:27:4b:e5:6b
	lease=0x6ab30000
}
{
	name=new
	ip_address=192.168.64.4
	hw_address=1,b2:7:43:8c:71:83
	lease=0x6ab21000
}
`
	if err := os.WriteFile(leases, []byte(data), 0o644); err != nil {
		t.Fatal(err)
	}
	mac, _ := net.ParseMAC("b2:07:43:8c:71:83")
	if got := leaseFor(leases, mac); got != "192.168.64.4" {
		t.Fatalf("lease for %s = %q, want the newest, 192.168.64.4", mac, got)
	}
	none, _ := net.ParseMAC("02:00:00:00:00:01")
	if got := leaseFor(leases, none); got != "" {
		t.Fatalf("lease for an unknown MAC = %q", got)
	}
}

func TestParseOptions(t *testing.T) {
	o, err := parseOptions(map[string]string{"username": "dev", "autologin": "false"})
	if err != nil {
		t.Fatal(err)
	}
	if o.user != "dev" || o.fullName != "dev" || o.autoLogin || o.password == "" {
		t.Fatalf("options = %+v", o)
	}
	if _, err := parseOptions(map[string]string{"user": "dev"}); err == nil {
		t.Fatal("an unknown option was accepted")
	}
}
