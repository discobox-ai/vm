//go:build darwin && cgo

package vz

import (
	"archive/tar"
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"io"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/discobox-ai/vm/pkg/guest"
	"github.com/discobox-ai/vm/pkg/machine"
	"github.com/discobox-ai/vm/pkg/machine/local"
	"github.com/discobox-ai/vm/pkg/machine/machinetest"
)

// BaseEnv names a committed macOS VM, in the default pool of the driver's
// directory ($DISCO_VM_ROOT, or the per-OS data directory): one `disco-vm
// install NAME` made, which `disco-vm ls -a` lists. The conformance suite
// creates from it rather than installing macOS on every run, and skips
// without it.
const BaseEnv = "DISCO_VM_VZ_BASE"

// LinuxBaseEnv is the base the Linux suite's fresh VMs start from
// (machine.Config.Base): a guest image's reference, a directory holding one's
// artifacts, or "default" for DefaultLinuxBase. Without it the Linux suite
// skips, and the macOS suite's fresh VM starts from DefaultLinuxBase. Its
// agent is $DISCO_VM_AGENT.
const LinuxBaseEnv = "DISCO_VM_VZ_LINUX_BASE"

// TestConformance is the vz driver's definition of done. It creates VMs, so
// the test binary has to be signed, and so does the disco-vm that runs as each
// started instance's shim ($DISCO_VM_TEST_BINARY): run it with
// `go tool task test:vz`.
func TestConformance(t *testing.T) {
	base := os.Getenv(BaseEnv)
	if base == "" {
		t.Skipf("$%s names no committed macOS VM", BaseEnv)
	}
	binary := os.Getenv("DISCO_VM_TEST_BINARY")
	if binary == "" {
		t.Fatal("$DISCO_VM_TEST_BINARY names no signed disco-vm to run each started instance's shim")
	}
	dir := machine.DefaultDir()
	config, conf := envConfig(t)
	// The suite's fresh VM is a Linux guest from the Linux suite's base,
	// where one is named; otherwise DefaultLinuxBase.
	linuxBase, _ := linuxBaseFromEnv(t)
	cfg := machine.Config{
		Dir:          dir,
		Base:         linuxBase,
		ShimCommand:  []string{binary, "--root", dir, "--driver", "vz", "--driver-config", string(config), "shim"},
		DriverConfig: config,
	}
	driver, err := local.New(cfg, newHypervisor(cfg, conf))
	if err != nil {
		t.Fatal(err)
	}
	machinetest.Run(t, driver, machinetest.Config{
		From:       base,
		CPUs:       4,
		Memory:     8 << 30,
		Timeout:    10 * time.Minute,
		ScratchDir: "/Users/Shared",
		GuestAgent: agentPath,
		Pool: func(pool string) machine.Driver {
			d, err := driver.InPool(pool)
			if err != nil {
				t.Fatal(err)
			}
			return d
		},
	})
}

// TestConformanceLinux is the suite from fresh Linux VMs: their guests boot
// beside macOS ones and do not count against their limit.
func TestConformanceLinux(t *testing.T) {
	driver := linuxDriver(t)
	machinetest.Run(t, driver, machinetest.Config{
		CPUs:       2,
		Memory:     2 << 30,
		Timeout:    5 * time.Minute,
		ScratchDir: "/var/tmp",
		GuestAgent: "/run/disco-vm/disco-vm",
		Pool: func(pool string) machine.Driver {
			d, err := driver.InPool(pool)
			if err != nil {
				t.Fatal(err)
			}
			return d
		},
	})
}

// TestLinuxBesideMacOS is issue #27's proof: a Linux guest with no network
// device boots while a macOS guest runs, and its caller reaches a vsock port
// in it.
func TestLinuxBesideMacOS(t *testing.T) {
	macOS := os.Getenv(BaseEnv)
	if macOS == "" {
		t.Skipf("$%s names no committed macOS VM", BaseEnv)
	}
	driver := linuxDriver(t)
	ctx := t.Context()
	from, err := driver.Instance(ctx, macOS)
	if err != nil {
		t.Fatal(err)
	}
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
	start(machine.InstanceSpec{Name: "beside-macos", From: &from})
	linux := start(machine.InstanceSpec{Name: "beside-linux", Egress: &machine.Egress{}})
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

// linuxDriver is the vz driver for the Linux suites, configured with the base
// their fresh VMs start from, or skips.
func linuxDriver(t *testing.T) *local.Driver {
	t.Helper()
	ref, ok := linuxBaseFromEnv(t)
	if !ok {
		t.Skipf("$%s names no Linux base", LinuxBaseEnv)
	}
	config, conf := envConfig(t)
	if conf.Agent == "" {
		t.Fatalf("$%s names no linux/arm64 disco-vm to boot the guest with", AgentEnv)
	}
	binary := os.Getenv("DISCO_VM_TEST_BINARY")
	if binary == "" {
		t.Fatal("$DISCO_VM_TEST_BINARY names no signed disco-vm to run each started instance's shim")
	}
	dir := machine.DefaultDir()
	cfg := machine.Config{
		Dir:          dir,
		Base:         ref,
		ShimCommand:  []string{binary, "--root", dir, "--driver", "vz", "--driver-config", string(config), "shim"},
		DriverConfig: config,
	}
	driver, err := local.New(cfg, newHypervisor(cfg, conf))
	if err != nil {
		t.Fatal(err)
	}
	return driver
}

// linuxBaseFromEnv is the base $DISCO_VM_VZ_LINUX_BASE names, as
// Hypervisor.base holds it ("default" is empty), and whether it names one.
// It is checked here, though the driver parses it only when it makes a fresh
// VM, so a bad base fails the test before it boots anything.
func linuxBaseFromEnv(t *testing.T) (string, bool) {
	t.Helper()
	ref := os.Getenv(LinuxBaseEnv)
	if ref == "" {
		return "", false
	}
	if ref == "default" {
		ref = ""
	}
	if _, err := parseLinuxBase(ref); err != nil {
		t.Fatal(err)
	}
	return ref, true
}

// envConfig is the vz configuration the command line would read from the
// environment, which a test's driver and its shims are both given (ADR
// 26-11-11-215 §6): a shim made from another is refused.
func envConfig(t *testing.T) (json.RawMessage, Config) {
	t.Helper()
	config, err := machine.ConfigFromValues("vz", machine.ConfigFromEnv("vz"))
	if err == nil && config == nil {
		config = json.RawMessage("{}")
	}
	if err != nil {
		t.Fatal(err)
	}
	conf, err := decode(config)
	if err != nil {
		t.Fatal(err)
	}
	return config, conf
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

// TestNoRouteOffTheMachine is issue #5's proof, on each OS whose base is
// named: an instance fenced to nothing has no network device and no route
// off the machine, yet its agent is reached over vsock (exec, and Dial
// through the shim of an attached machine), and a Forward takes it to a
// peer, itself fenced, which answers with a file only it holds.
func TestNoRouteOffTheMachine(t *testing.T) {
	linuxBase, linux := linuxBaseFromEnv(t)
	if !linux && os.Getenv(BaseEnv) == "" {
		t.Skipf("neither $%s nor $%s names a base", BaseEnv, LinuxBaseEnv)
	}
	binary := os.Getenv("DISCO_VM_TEST_BINARY")
	if binary == "" {
		t.Fatal("$DISCO_VM_TEST_BINARY names no signed disco-vm to run each started instance's shim")
	}
	config, conf := envConfig(t)
	dir := machine.DefaultDir()
	cfg := machine.Config{
		Dir:          dir,
		Base:         linuxBase,
		ShimCommand:  []string{binary, "--root", dir, "--driver", "vz", "--driver-config", string(config), "shim"},
		DriverConfig: config,
	}
	driver, err := local.New(cfg, newHypervisor(cfg, conf))
	if err != nil {
		t.Fatal(err)
	}
	t.Run("darwin", func(t *testing.T) {
		name := os.Getenv(BaseEnv)
		if name == "" {
			t.Skipf("$%s names no committed macOS VM", BaseEnv)
		}
		from, err := driver.Instance(t.Context(), name)
		if err != nil {
			t.Fatal(err)
		}
		noRoute(t, driver, machine.InstanceSpec{From: &from}, agentPath, "/Users/Shared",
			// Every macOS guest has the Apple silicon peripheral interfaces
			// (anpi0 and its en2) and the system's utun tunnels: none may be
			// active or hold an address beyond link-local, nothing off the
			// machine is routed, and neither family reaches it.
			`set -e
for i in $(ifconfig -l); do
  [ "$i" = lo0 ] && continue
  if ifconfig "$i" | grep -q "status: active"; then echo "link $i is active"; exit 1; fi
  if ifconfig "$i" | grep -q "inet "; then echo "link $i has an IPv4 address"; exit 1; fi
  if ifconfig "$i" | awk '$1 == "inet6" { print $2 }' | grep -qv '^fe80:'; then echo "link $i has an IPv6 address beyond link-local"; exit 1; fi
done
if route -n get 1.1.1.1 2>/dev/null | grep -q interface:; then echo "route to 1.1.1.1"; exit 1; fi
if nc -z -G 5 1.1.1.1 443 2>/dev/null; then echo "reached 1.1.1.1:443"; exit 1; fi
if nc -6 -z -G 5 2606:4700:4700::1111 443 2>/dev/null; then echo "reached [2606:4700:4700::1111]:443"; exit 1; fi
ifconfig -l -u`)
	})
	t.Run("linux", func(t *testing.T) {
		if !linux {
			t.Skipf("$%s names no Linux base", LinuxBaseEnv)
		}
		if conf.Agent == "" {
			t.Fatalf("$%s names no linux/arm64 disco-vm to boot the guest with", AgentEnv)
		}
		noRoute(t, driver, machine.InstanceSpec{}, "/run/disco-vm/disco-vm", "/var/tmp",
			// Only loopback, and no route but its own.
			`set -e
links=$(ls /sys/class/net)
[ "$links" = lo ] || { echo "links $links"; exit 1; }
[ "$(tail -n +2 /proc/net/route)" = "" ] || { echo "routes:"; cat /proc/net/route; exit 1; }
echo "$links"`)
	})
}

// noRoute starts a fenced peer holding a file, then a fenced instance with a
// Forward to it, both from spec, and checks the instance from the host and
// from inside: script, run in it by /bin/sh, fails where it finds a route
// off the machine.
func noRoute(t *testing.T, driver *local.Driver, spec machine.InstanceSpec, agent, scratch, script string) {
	t.Helper()
	ctx := t.Context()
	run := randomName()
	start := func(spec machine.InstanceSpec) (machine.Instance, machine.Machine) {
		t.Helper()
		spec.Egress = &machine.Egress{}
		inst, err := driver.Create(ctx, spec)
		if err != nil {
			t.Fatalf("create %s: %v", spec.Name, err)
		}
		t.Cleanup(func() { _ = driver.Destroy(context.Background(), inst) })
		startCtx, cancel := context.WithTimeout(ctx, 10*time.Minute)
		defer cancel()
		m, err := driver.Start(startCtx, inst, machine.BootOptions{})
		if err != nil {
			t.Fatalf("start %s: %v", spec.Name, err)
		}
		t.Cleanup(func() { _ = m.Kill(context.Background()) })
		return inst, m
	}
	exec := func(m machine.Machine, argv ...string) string {
		t.Helper()
		var out bytes.Buffer
		code, err := machine.Run(ctx, m, machine.ExecRequest{Argv: argv}, nil, &out, &out)
		if err != nil || code != 0 {
			t.Fatalf("%q: exit %d, %v\n%s", argv, code, err, out.String())
		}
		return out.String()
	}

	peerSpec := spec
	peerSpec.Name = "noroute-peer-" + run
	peer, pm := start(peerSpec)
	proof := "proof-" + run
	exec(pm, "/bin/sh", "-c", `printf %s "$1" > "$0"/noroute.txt && sync`, scratch, proof)

	spec.Name = "noroute-" + run
	spec.Forwards = []machine.Forward{{Port: 7402, Instance: peer.Name, InstancePort: guest.AgentPort}}
	inst, m := start(spec)
	got, err := driver.Instance(ctx, inst.Name)
	if err != nil || got.Egress == nil || len(got.Egress.Allow) != 0 {
		t.Fatalf("the instance reports its fence as %+v: %v", got.Egress, err)
	}

	t.Logf("links up: %s", strings.TrimSpace(exec(m, "/bin/sh", "-c", script)))

	attached, err := driver.Attach(ctx, inst)
	if err != nil {
		t.Fatal(err)
	}
	conn, err := attached.Dial(ctx, guest.AgentPort)
	if err != nil {
		t.Fatalf("dial the agent through the shim: %v", err)
	}
	_ = conn.SetDeadline(time.Now().Add(time.Minute))
	if _, err := io.WriteString(conn, "GET /v1/health HTTP/1.0\r\nHost: guest\r\n\r\n"); err != nil {
		t.Fatal(err)
	}
	answer, _ := io.ReadAll(conn)
	_ = conn.Close()
	if !strings.Contains(string(answer), " 200 ") {
		t.Fatalf("the agent's port through the shim answered %q", answer)
	}

	request := `GET /v1/files?path=` + scratch + `/noroute.txt HTTP/1.0\r\nHost: guest\r\n\r\n`
	if out := exec(m, "/bin/sh", "-c", `printf "$1" | "$0" dial-host 7402`, agent, request); !strings.Contains(out, proof) {
		t.Fatalf("the peer did not answer through the forward with its file; the guest read %q", out)
	}
}

// randomName is a suffix no other run's instances share.
func randomName() string {
	return strconv.FormatInt(time.Now().UnixNano()%1e9, 36)
}

// TestMoveAnOlderAgent is issue #83's proof: a committed macOS VM whose
// agent speaks the protocol before this host's (an agent built with the
// disco_vm_older_agent tag) is not installed again; a VM created from it
// boots with its agent moved to this host's protocol, the binary its shim
// runs.
func TestMoveAnOlderAgent(t *testing.T) {
	name := os.Getenv(BaseEnv)
	if name == "" {
		t.Skipf("$%s names no committed macOS VM", BaseEnv)
	}
	binary := os.Getenv("DISCO_VM_TEST_BINARY")
	if binary == "" {
		t.Fatal("$DISCO_VM_TEST_BINARY names no signed disco-vm to run each started instance's shim")
	}
	ctx := t.Context()
	older := filepath.Join(t.TempDir(), "disco-vm")
	build := exec.CommandContext(ctx, "go", "build", "-tags", "disco_vm_older_agent", "-o", older, "../../../cmd/disco-vm")
	if out, err := build.CombinedOutput(); err != nil {
		t.Fatalf("build the older agent: %v\n%s", err, out)
	}
	dir := machine.DefaultDir()
	config, conf := envConfig(t)
	cfg := machine.Config{
		Dir:          dir,
		ShimCommand:  []string{binary, "--root", dir, "--driver", "vz", "--driver-config", string(config), "shim"},
		DriverConfig: config,
	}
	driver, err := local.New(cfg, newHypervisor(cfg, conf))
	if err != nil {
		t.Fatal(err)
	}
	from, err := driver.Instance(ctx, name)
	if err != nil {
		t.Fatal(err)
	}
	run := strconv.FormatInt(time.Now().UnixNano()%1e9, 36)
	start := func(inst machine.Instance) machine.Machine {
		t.Helper()
		startCtx, cancel := context.WithTimeout(ctx, 10*time.Minute)
		defer cancel()
		m, err := driver.Start(startCtx, inst, machine.BootOptions{})
		if err != nil {
			t.Fatalf("start %s: %v", inst.Name, err)
		}
		t.Cleanup(func() { _ = m.Kill(context.Background()) })
		return m
	}
	stop := func(m machine.Machine) {
		t.Helper()
		if err := m.Shutdown(ctx, false); err != nil {
			t.Fatal(err)
		}
		select {
		case <-m.Done():
		case <-time.After(5 * time.Minute):
			t.Fatal("the guest did not power off")
		}
	}
	inode := func(m machine.Machine) string {
		t.Helper()
		var out bytes.Buffer
		if code, err := machine.Run(ctx, m, machine.ExecRequest{Argv: []string{"stat", "-f", "%i", agentPath}}, nil, &out, &out); err != nil || code != 0 {
			t.Fatalf("stat the agent: exit %d, %v\n%s", code, err, out.String())
		}
		return strings.TrimSpace(out.String())
	}
	sum := func(m machine.Machine) string {
		t.Helper()
		var out bytes.Buffer
		if code, err := machine.Run(ctx, m, machine.ExecRequest{Argv: []string{"shasum", "-a", "256", agentPath}}, nil, &out, &out); err != nil || code != 0 {
			t.Fatalf("shasum the agent: exit %d, %v\n%s", code, err, out.String())
		}
		return strings.Fields(out.String())[0]
	}

	// A VM of the base, given the older agent the way moveAgent gives the
	// current one, then committed: a base installed by an older disco-vm.
	base, err := driver.Create(ctx, machine.InstanceSpec{Name: "older-agent-" + run, From: &from})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = driver.Destroy(context.Background(), base) })
	m := start(base)
	data, err := os.ReadFile(older)
	if err != nil {
		t.Fatal(err)
	}
	var archive bytes.Buffer
	tw := tar.NewWriter(&archive)
	if err := tw.WriteHeader(&tar.Header{Name: "disco-vm.new", Mode: 0o755, Size: int64(len(data)), Typeflag: tar.TypeReg}); err != nil {
		t.Fatal(err)
	}
	if _, err := tw.Write(data); err != nil {
		t.Fatal(err)
	}
	if err := tw.Close(); err != nil {
		t.Fatal(err)
	}
	if err := m.CopyTo(ctx, filepath.Dir(agentPath), &archive); err != nil {
		t.Fatal(err)
	}
	install := moveRequest()
	install.Argv[len(install.Argv)-1] = strconv.Itoa(guest.Protocol - 1)
	_, _ = machine.Run(ctx, m, install, nil, io.Discard, io.Discard)
	deadline := time.Now().Add(agentMoveTimeout)
	for {
		info, err := m.Info(ctx)
		if err == nil && info.Protocol == guest.Protocol-1 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("the base's agent after installing the older one: %+v, %v", info, err)
		}
		time.Sleep(time.Second)
	}
	// A binary that is not an agent of this host's protocol is refused in
	// the guest, and the older agent goes on serving.
	if err := moveAgent(ctx, m, "/bin/echo", m.Done(), io.Discard); err == nil || !strings.Contains(err.Error(), "does not speak protocol") {
		t.Fatalf("moving the agent to /bin/echo: %v", err)
	}
	// One that never exits is killed in the guest within its 10 s.
	blocks := filepath.Join(t.TempDir(), "blocks")
	if err := os.WriteFile(blocks, []byte("#!/bin/sh\nsleep 600\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := moveAgent(ctx, m, blocks, m.Done(), io.Discard); err == nil || !strings.Contains(err.Error(), "did not exit") {
		t.Fatalf("moving the agent to a program that does not exit: %v", err)
	}
	var left bytes.Buffer
	if _, err := machine.Run(ctx, m, machine.ExecRequest{Argv: []string{"/bin/sh", "-c", "ps -axo command | grep -E '[d]isco-vm.new|[s]leep 600'; ls /usr/local/libexec"}}, nil, &left, &left); err != nil || strings.Contains(left.String(), ".new") || strings.Contains(left.String(), "sleep 600") {
		t.Fatalf("a refused move left behind: %v\n%s", err, left.String())
	}
	if info, err := m.Info(ctx); err != nil || info.Protocol != guest.Protocol-1 {
		t.Fatalf("the older agent after a refused move: %+v, %v", info, err)
	}
	stop(m)
	base, err = driver.Commit(ctx, base)
	if err != nil {
		t.Fatal(err)
	}

	clone, err := driver.Create(ctx, machine.InstanceSpec{Name: "older-agent-clone-" + run, From: &base})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = driver.Destroy(context.Background(), clone) })
	booted := time.Now()
	m = start(clone)
	t.Logf("booted with its agent moved in %s", time.Since(booted).Round(time.Second))
	info, err := m.Info(ctx)
	if err != nil || info.Protocol != guest.Protocol {
		t.Fatalf("the clone's agent once booted: %+v, %v; want protocol %d", info, err, guest.Protocol)
	}
	want, err := fileSHA256(binary)
	if err != nil {
		t.Fatal(err)
	}
	if got := sum(m); got != want {
		t.Fatalf("the clone's agent is %s, and the shim's binary %s", got, want)
	}
	moved := inode(m)
	stop(m)
	// Booted again, the agent is current and is not moved again: the same
	// file, which a move would have replaced with another.
	m = start(clone)
	if got := inode(m); got != moved {
		t.Fatalf("booted again, the clone's agent was moved again (inode %s, was %s)", got, moved)
	}
}

func fileSHA256(path string) (string, error) {
	f, err := os.Open(path)
	if err != nil {
		return "", err
	}
	defer f.Close()
	h := sha256.New()
	if _, err := io.Copy(h, f); err != nil {
		return "", err
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}
