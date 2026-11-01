//go:build darwin && cgo

package vz

import (
	"bytes"
	"context"
	"io"
	"net"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/discobox-ai/vm/pkg/guest"
	"github.com/discobox-ai/vm/pkg/machine"
	"github.com/discobox-ai/vm/pkg/machine/local"
	"github.com/discobox-ai/vm/pkg/machine/machinetest"
)

// BaseEnv names an installed image, by its ID, in the driver's directory
// ($DISCO_VM_ROOT, or the per-OS data directory): an image `disco-vm install`
// made, which `disco-vm images` lists. The conformance suite clones it rather
// than installing macOS on every run, and skips without it.
const BaseEnv = "DISCO_VM_VZ_BASE"

// LinuxBaseEnv names a Linux base to run the conformance suite from:
// LinuxBaseName, a guest image's reference, or a directory holding one's
// artifacts. Without it the Linux suite skips. Its agent is $DISCO_VM_AGENT.
const LinuxBaseEnv = "DISCO_VM_VZ_LINUX_BASE"

// TestConformance is the vz driver's definition of done. It creates VMs, so
// the test binary has to be signed, and so does the disco-vm that runs as each
// started instance's shim ($DISCO_VM_TEST_BINARY): run it with
// `go tool task test:vz`.
func TestConformance(t *testing.T) {
	base := os.Getenv(BaseEnv)
	if base == "" {
		t.Skipf("$%s names no installed image", BaseEnv)
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
		Image:      base,
		CPUs:       4,
		Memory:     8 << 30,
		Timeout:    10 * time.Minute,
		ScratchDir: "/Users/Shared",
	})
}

// TestConformanceLinux is the suite from a Linux base: its guests boot
// beside macOS ones and do not count against their limit.
func TestConformanceLinux(t *testing.T) {
	driver, ref := linuxDriver(t)
	base, found, err := driver.Base(t.Context(), ref)
	if err != nil || !found {
		t.Fatalf("base %s: found %v, %v", ref, found, err)
	}
	machinetest.Run(t, driver, machinetest.Config{
		Base:       &base,
		CPUs:       2,
		Memory:     2 << 30,
		Timeout:    5 * time.Minute,
		ScratchDir: "/var/tmp",
		GuestAgent: "/run/disco-vm/disco-vm",
	})
}

// TestLinuxBesideMacOS is issue #27's proof: a Linux guest with no network
// device boots while a macOS guest runs, and its caller reaches a vsock port
// in it.
func TestLinuxBesideMacOS(t *testing.T) {
	macOS := os.Getenv(BaseEnv)
	if macOS == "" {
		t.Skipf("$%s names no installed macOS image", BaseEnv)
	}
	driver, ref := linuxDriver(t)
	ctx := t.Context()
	start := func(spec machine.InstanceSpec) machine.Machine {
		t.Helper()
		inst, err := driver.Create(ctx, spec)
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = driver.Destroy(context.Background(), inst) })
		m, err := driver.Start(ctx, inst, machine.BootOptions{})
		if err != nil {
			t.Fatalf("start %s: %v", spec.Name, err)
		}
		t.Cleanup(func() { _ = m.Kill(context.Background()) })
		return m
	}
	start(machine.InstanceSpec{Name: "beside-macos", ImageID: macOS, GuestOS: machine.Darwin})
	base, found, err := driver.Base(ctx, ref)
	if err != nil || !found {
		t.Fatalf("base %s: found %v, %v", ref, found, err)
	}
	linux := start(machine.InstanceSpec{Name: "beside-linux", Base: &base, GuestOS: machine.Linux, Egress: &machine.Egress{}})
	var out bytes.Buffer
	if code, err := machine.Run(ctx, linux, machine.ExecRequest{Argv: []string{"ls", "/sys/class/net"}}, nil, &out, &out); err != nil || code != 0 {
		t.Fatalf("ls /sys/class/net: exit %d, %v\n%s", code, err, out.String())
	}
	if links := strings.Fields(out.String()); len(links) != 1 || links[0] != "lo" {
		t.Fatalf("a guest with no network device has links %q", links)
	}
	conn, err := linux.Dial(ctx, guest.AgentPort)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	if _, err := io.WriteString(conn, "GET /v1/health HTTP/1.0\r\nHost: guest\r\n\r\n"); err != nil {
		t.Fatal(err)
	}
	answer, _ := io.ReadAll(conn)
	if !strings.Contains(string(answer), " 200 ") {
		t.Fatalf("the Linux guest's vsock port answered %q", answer)
	}
}

// linuxDriver is the vz driver for the Linux suites, and the base they run,
// or skips.
func linuxDriver(t *testing.T) (*local.Driver, string) {
	t.Helper()
	ref := os.Getenv(LinuxBaseEnv)
	if ref == "" {
		t.Skipf("$%s names no Linux base", LinuxBaseEnv)
	}
	if os.Getenv(AgentEnv) == "" {
		t.Fatalf("$%s names no linux/arm64 disco-vm to boot the guest with", AgentEnv)
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
	return driver, ref
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
