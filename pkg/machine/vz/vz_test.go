//go:build darwin && cgo

package vz

import (
	"bytes"
	"context"
	"net"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/discobox-ai/vm/pkg/guest"
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

func TestKCPassword(t *testing.T) {
	// What loginwindow read to log a real macOS 27 guest in as "devpw"'s user.
	want := []byte{0x19, 0xec, 0x24, 0x53, 0xa5, 0xbc, 0xdd, 0xea, 0xa3, 0xb9, 0x1f, 0x7d}
	if got := kcpassword("devpw"); !bytes.Equal(got, want) {
		t.Fatalf("kcpassword(devpw) = % x, want % x", got, want)
	}
	if got := len(kcpassword("twelve-chars")); got != 24 {
		t.Fatalf("a 12-byte password pads to %d bytes, want 24", got)
	}
}

// TestWarmUser warms with a user and checks that a resumed clone has the
// account at its uid and is already in its session. It uses uid 777, which an
// older base, whose install account is still 501, leaves free.
func TestWarmUser(t *testing.T) {
	base := os.Getenv(BaseEnv)
	if base == "" {
		t.Skipf("$%s names no installed layer", BaseEnv)
	}
	ctx := context.Background()
	d := &Driver{}
	work := t.TempDir()
	chain := []machine.Layer{{ID: "base", Dir: base}}
	spec := machine.WarmSpec{
		GuestOS: machine.Darwin, Chain: chain, Dir: filepath.Join(work, "warm"), Count: 1,
		CPUs: 4, Memory: 8 << 30, Log: testWriter{t},
		User: &machine.User{Name: "tester", FullName: "Warm Tester", UID: 777},
	}
	if err := os.MkdirAll(spec.Dir, 0o755); err != nil {
		t.Fatal(err)
	}
	if _, err := d.Warm(ctx, spec); err != nil {
		t.Fatal(err)
	}
	inst := machine.InstanceSpec{ID: "u", Dir: filepath.Join(work, "inst"), GuestOS: machine.Darwin, Chain: chain, Mode: machine.Resume, WarmDir: spec.Dir}
	if err := d.Prepare(ctx, inst); err != nil {
		t.Fatal(err)
	}
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
	var out bytes.Buffer
	check := guest.ExecRequest{Argv: []string{"/bin/sh", "-c", `echo "$(id -u tester) $(stat -f %Su /dev/console) $(pgrep -qx 'Setup Assistant' && echo setup || echo desktop)"`}}
	if code, err := c.Run(wctx, check, nil, &out, &out); err != nil || code != 0 {
		t.Fatalf("check: %v (exit %d): %s", err, code, out.String())
	}
	if got := strings.TrimSpace(out.String()); got != "777 tester desktop" {
		t.Fatalf("uid, console user, and screen = %q, want \"777 tester desktop\" (screen locked: %v)", got, screenLocked())
	}
	out.Reset()
	sudo := guest.ExecRequest{Argv: []string{"sudo", "-n", "id", "-u"}, User: "tester"}
	if code, err := c.Run(wctx, sudo, nil, &out, &out); err != nil || code != 0 || strings.TrimSpace(out.String()) != "0" {
		t.Fatalf("passwordless sudo as tester: %v (exit %d): %s", err, code, out.String())
	}
}

// TestTemplates proves a stage is reusable: two clones running at once take
// different templates (so different MACs), a third after one stops resumes
// too, and a resumed clone's next boot has an identity of its own.
func TestTemplates(t *testing.T) {
	base := os.Getenv(BaseEnv)
	if base == "" {
		t.Skipf("$%s names no installed layer", BaseEnv)
	}
	if screenLocked() {
		t.Skip("a restore needs the screen unlocked")
	}
	ctx := context.Background()
	d := &Driver{}
	work := t.TempDir()
	chain := []machine.Layer{{ID: "base", Dir: base}}
	spec := machine.WarmSpec{GuestOS: machine.Darwin, Chain: chain, Dir: filepath.Join(work, "warm"), CPUs: 4, Memory: 8 << 30, Log: testWriter{t}}
	if err := os.MkdirAll(spec.Dir, 0o755); err != nil {
		t.Fatal(err)
	}
	if _, err := d.Warm(ctx, spec); err != nil {
		t.Fatal(err)
	}
	if w, err := d.Warmth(ctx, spec); err != nil || w.Mode != machine.Resume || w.Clones != -1 {
		t.Fatalf("a warm stage reports %+v, %v; want any number of resume clones", w, err)
	}
	boot := func(id string) (machine.InstanceSpec, machine.Machine, string) {
		t.Helper()
		inst := machine.InstanceSpec{ID: id, Dir: filepath.Join(work, id), GuestOS: machine.Darwin, Chain: chain, Mode: machine.Resume, WarmDir: spec.Dir}
		if err := d.Prepare(ctx, inst); err != nil {
			t.Fatalf("prepare %s: %v", id, err)
		}
		var console bytes.Buffer
		m, err := d.Boot(ctx, inst, machine.BootOptions{CPUs: 4, Memory: 8 << 30, Console: &console})
		if err != nil {
			t.Fatalf("boot %s: %v", id, err)
		}
		t.Cleanup(func() { _ = m.Kill(context.Background()) })
		if !strings.Contains(console.String(), "resumed a template") {
			t.Fatalf("%s did not resume: %s", id, console.String())
		}
		c := m.(*vmMachine).agent()
		defer c.Close()
		wctx, cancel := context.WithTimeout(ctx, 2*time.Minute)
		defer cancel()
		if err := c.WaitReady(wctx, m.Done()); err != nil {
			t.Fatalf("agent on %s: %v", id, err)
		}
		mac, _ := os.ReadFile(filepath.Join(inst.Dir, macName))
		return inst, m, strings.TrimSpace(string(mac))
	}
	oneInst, one, oneMAC := boot("one")
	_, two, twoMAC := boot("two")
	if oneMAC == twoMAC {
		t.Fatalf("two running clones share the MAC %s", oneMAC)
	}
	_ = two.Kill(ctx)
	_, three, threeMAC := boot("three")
	if threeMAC != twoMAC {
		t.Fatalf("the third clone took %s, not the freed template's %s", threeMAC, twoMAC)
	}
	_ = three.Kill(ctx)

	// A resumed clone's next boot is cold, as itself.
	_ = one.Kill(ctx)
	again, err := d.Boot(ctx, oneInst, machine.BootOptions{CPUs: 4, Memory: 8 << 30})
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = again.Kill(context.Background()) }()
	mac, _ := os.ReadFile(filepath.Join(oneInst.Dir, macName))
	if got := strings.TrimSpace(string(mac)); got == oneMAC || got == twoMAC {
		t.Fatalf("a restarted clone kept a template's MAC %s", got)
	}
}
