//go:build darwin && cgo

package vz

import (
	"context"
	"net"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/discobox-ai/vm/pkg/machine"
	"github.com/discobox-ai/vm/pkg/machine/machinetest"
)

// BaseEnv names an installed layer's directory, the `payload` directory of an
// install layer in the image store. The conformance suite clones it rather
// than installing macOS on every run, and skips without it.
const BaseEnv = "DISCO_VM_VZ_BASE"

// TestConformance is the vz driver's definition of done. It creates VMs, so
// the test binary has to be signed: run it with `make test-vz`.
func TestConformance(t *testing.T) {
	base := os.Getenv(BaseEnv)
	if base == "" {
		t.Skipf("$%s names no installed layer", BaseEnv)
	}
	if err := bundle(base).installedLayer(); err != nil {
		t.Fatal(err)
	}
	machinetest.Run(t, &Driver{}, machinetest.Config{
		Base:       &machine.Layer{ID: "base", Dir: base},
		Boot:       machine.BootOptions{CPUs: 4, Memory: 8 << 30, Console: testWriter{t}},
		Timeout:    10 * time.Minute,
		ScratchDir: "/Users/Shared",
	})
}

type testWriter struct{ t *testing.T }

func (w testWriter) Write(p []byte) (int, error) {
	w.t.Log(string(p))
	return len(p), nil
}

// TestResumeOrFallBack resumes a staged state, or, on a Mac whose screen is
// locked, proves the fallback: the restore is refused, Boot boots the staged
// disk cold, and the state is consumed either way.
func TestResumeOrFallBack(t *testing.T) {
	base := os.Getenv(BaseEnv)
	if base == "" {
		t.Skipf("$%s names no installed layer", BaseEnv)
	}
	ctx := context.Background()
	d := &Driver{}
	work := t.TempDir()
	chain := []machine.Layer{{ID: "base", Dir: base}}
	spec := machine.WarmSpec{GuestOS: machine.Darwin, Chain: chain, Dir: filepath.Join(work, "warm"), Count: 1, CPUs: 4, Memory: 8 << 30, Log: testWriter{t}}
	if err := os.MkdirAll(spec.Dir, 0o755); err != nil {
		t.Fatal(err)
	}
	if _, err := d.Warm(ctx, spec); err != nil {
		t.Fatal(err)
	}
	locked := screenLocked()
	t.Logf("screen locked: %v", locked)
	inst := machine.InstanceSpec{ID: "r", Dir: filepath.Join(work, "inst"), GuestOS: machine.Darwin, Chain: chain, Mode: machine.Resume, WarmDir: spec.Dir}
	if err := d.Prepare(ctx, inst); err != nil {
		t.Fatal(err)
	}
	start := time.Now()
	m, err := d.Boot(ctx, inst, machine.BootOptions{CPUs: 4, Memory: 8 << 30, Console: testWriter{t}})
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = m.Kill(ctx) }()
	c := m.(*vmMachine).agent()
	defer c.Close()
	wctx, cancel := context.WithTimeout(ctx, 5*time.Minute)
	defer cancel()
	if err := c.WaitReady(wctx, m.Done()); err != nil {
		t.Fatal(err)
	}
	t.Logf("agent answered %s after Boot was called", time.Since(start).Round(time.Millisecond))
	if _, err := os.Stat(filepath.Join(inst.Dir, stateName)); err == nil {
		t.Fatal("state.bin was not consumed")
	}
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
