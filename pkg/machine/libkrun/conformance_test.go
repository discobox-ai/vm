//go:build linux

package libkrun

import (
	"bytes"
	"context"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/discobox-ai/vm/pkg/machine"
	"github.com/discobox-ai/vm/pkg/machine/local"
	"github.com/discobox-ai/vm/pkg/machine/machinetest"
)

// BaseEnv is the base the suite's fresh VMs start from (machine.Config.Base):
// a guest image's reference, a directory holding one's artifacts
// (`go tool task build:linux-guest`), or "default" for
// linuximage.DefaultBase. Without it the suite skips. The runtime is
// $DISCO_VM_KRUN_RUNTIME's, or DefaultRuntime pulled.
const BaseEnv = "DISCO_VM_LIBKRUN_BASE"

// TestConformance is the libkrun driver's definition of done, on a Linux
// amd64 host with KVM. Each started instance's shim, and every guest's
// agent, is $DISCO_VM_TEST_BINARY, a static linux disco-vm: run it with
// `go tool task test:libkrun`.
func TestConformance(t *testing.T) {
	driver := realDriver(t)
	machinetest.Run(t, driver, machinetest.Config{
		CPUs:       2,
		Memory:     2 << 30,
		Timeout:    5 * time.Minute,
		ScratchDir: "/var/tmp",
		GuestAgent: "/run/disco-vm/disco-vm",
	})
}

// realDriver is the libkrun driver on this host, configured from the
// environment, or skips.
func realDriver(t *testing.T) *local.Driver {
	t.Helper()
	base := os.Getenv(BaseEnv)
	if base == "" {
		t.Skipf("$%s names no Linux base", BaseEnv)
	}
	if base == "default" {
		base = ""
	}
	if _, err := parseBase(base); err != nil {
		t.Fatal(err)
	}
	binary := os.Getenv("DISCO_VM_TEST_BINARY")
	if binary == "" {
		t.Fatal("$DISCO_VM_TEST_BINARY names no disco-vm to run each started instance's shim")
	}
	// A test boots in this process, whose binary is no agent. The shim is
	// given the configuration this driver has (ADR 26-11-11-215 §6), or it
	// refuses to run.
	values := map[string]string{"agent": binary}
	if rt := os.Getenv(RuntimeEnv); rt != "" {
		values["runtime"] = rt
	}
	config, err := machine.ConfigFromValues("libkrun", values)
	if err != nil {
		t.Fatal(err)
	}
	conf, err := decode(config)
	if err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	cfg := machine.Config{
		Dir:          dir,
		Base:         base,
		ShimCommand:  []string{binary, "--root", dir, "--driver", "libkrun", "--driver-config", string(config), "shim"},
		DriverConfig: config,
	}
	h := newHypervisor(cfg, conf)
	driver, err := local.New(cfg, h)
	if err != nil {
		t.Fatal(err)
	}
	h.cacheDir = driver.CacheDir()
	if err := driver.Check(t.Context()); err != nil {
		t.Fatal(err)
	}
	return driver
}

// A guest reading to EOF through a forward, its own half still open, reads
// it once the host's end has answered and closed: libkrun cannot pass the
// host's half-close on, so the forward closes the stream instead
// (forwardConn). A forward that kept its half open hung the guest until its
// own input ended.
func TestForwardEndsWhenTheHostCloses(t *testing.T) {
	driver := realDriver(t)
	ctx := t.Context()
	sock := filepath.Join(t.TempDir(), "host.sock")
	l, err := (&net.ListenConfig{}).Listen(ctx, "unix", sock)
	if err != nil {
		t.Fatal(err)
	}
	defer l.Close()
	go func() {
		for {
			c, err := l.Accept()
			if err != nil {
				return
			}
			_, _ = c.Write([]byte("hello from the host\n"))
			_ = c.Close()
		}
	}()
	inst, err := driver.Create(ctx, machine.InstanceSpec{Name: "forward-eof", Forwards: []machine.Forward{{Port: 7500, Socket: sock}}})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = driver.Destroy(context.Background(), inst) })
	m, err := driver.Start(ctx, inst, machine.BootOptions{})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = m.Kill(context.Background()) })
	var out bytes.Buffer
	script := `s=$(date +%s); sleep 10 | { /run/disco-vm/disco-vm dial-host 7500; echo "returned after $(( $(date +%s) - s ))s"; }`
	if code, err := machine.Run(ctx, m, machine.ExecRequest{Argv: []string{"/bin/sh", "-c", script}}, nil, &out, &out); err != nil || code != 0 {
		t.Fatalf("dial-host: exit %d, %v\n%s", code, err, out.String())
	}
	if got := out.String(); !strings.Contains(got, "hello from the host") || !strings.Contains(got, "returned after 0s") && !strings.Contains(got, "returned after 1s") {
		t.Fatalf("the guest read %q, want the host's answer and EOF at once", got)
	}
}

// A VM on passt reaches nothing the host binds to its loopback, and no host
// port is forwarded into it (discobox ADR 26-11-11-963 §11): the guest's
// gateway, which passt would otherwise map to the host's loopback, refuses a
// port the host serves there, and the host's loopback refuses a port the
// guest serves. passt gives the guest no lease on a host with no default
// route, so the test skips on one.
func TestPasstReachesNoHostLoopback(t *testing.T) {
	driver := realDriver(t)
	if !hasDefaultRoute(t) {
		t.Skip("this host has no IPv4 default route, so passt gives the guest no lease")
	}
	ctx := t.Context()
	host, err := (&net.ListenConfig{}).Listen(ctx, "tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer host.Close()
	go func() {
		for {
			c, err := host.Accept()
			if err != nil {
				return
			}
			_, _ = c.Write([]byte("the host's loopback\n"))
			_ = c.Close()
		}
	}()
	hostPort := host.Addr().(*net.TCPAddr).Port
	// A port nothing on the host serves, for the guest to serve. It is
	// below the ephemeral range, which passt's "all" leaves out.
	guestPort := 0
	for port := 20000; port < 30000 && guestPort == 0; port++ {
		if free, err := (&net.ListenConfig{}).Listen(ctx, "tcp", fmt.Sprintf(":%d", port)); err == nil {
			guestPort = port
			_ = free.Close()
		}
	}
	if guestPort == 0 {
		t.Fatal("no free port from 20000 to 29999")
	}

	inst, err := driver.Create(ctx, machine.InstanceSpec{Name: "passt-fence"})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = driver.Destroy(context.Background(), inst) })
	m, err := driver.Start(ctx, inst, machine.BootOptions{})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = m.Kill(context.Background()) })
	run := func(script string) (int, string) {
		t.Helper()
		var out bytes.Buffer
		code, err := machine.Run(ctx, m, machine.ExecRequest{Argv: []string{"/bin/bash", "-c", script}}, nil, &out, &out)
		if err != nil {
			t.Fatalf("%s: %v\n%s", script, err, out.String())
		}
		return code, out.String()
	}

	// The guest has a lease and a gateway, and the gateway does not answer
	// at the host's loopback port.
	reach := fmt.Sprintf(`for i in $(seq 60); do gw=$(ip -4 route show default | { read -r _ _ gw _; echo "$gw"; }); [ -n "$gw" ] && break; sleep 1; done
[ -n "$gw" ] || { echo "no default route"; ip addr; exit 2; }
if timeout 5 bash -c "exec 3<>/dev/tcp/$gw/%d && cat <&3"; then echo "reached $gw"; exit 1; fi
echo "refused at $gw"`, hostPort)
	if code, out := run(reach); code != 0 {
		t.Fatalf("the guest at its gateway, port %d: exit %d\n%s", hostPort, code, out)
	}

	// The guest serves a port, and the host's loopback does not.
	serve := fmt.Sprintf(`setsid perl -MIO::Socket::INET -e '$s = IO::Socket::INET->new(LocalAddr => "0.0.0.0", LocalPort => %d, Listen => 5, ReuseAddr => 1) or die "listen: $!"; alarm 120; while ($c = $s->accept) { print $c "the guest\n"; close $c }' </dev/null >/dev/null 2>&1 &
for i in $(seq 50); do timeout 2 bash -c "exec 3<>/dev/tcp/127.0.0.1/%d && cat <&3" && exit 0; sleep 0.2; done
echo "the guest's listener never answered"; exit 1`, guestPort, guestPort)
	if code, out := run(serve); code != 0 || !strings.Contains(out, "the guest") {
		t.Fatalf("serving port %d in the guest: exit %d\n%s", guestPort, code, out)
	}
	dialer := net.Dialer{Timeout: 5 * time.Second}
	if c, err := dialer.DialContext(ctx, "tcp", fmt.Sprintf("127.0.0.1:%d", guestPort)); err == nil {
		_ = c.Close()
		t.Fatalf("the host's loopback port %d reached the guest's listener: passt forwards it", guestPort)
	}
}

// hasDefaultRoute reports whether this host has an IPv4 default route.
func hasDefaultRoute(t *testing.T) bool {
	t.Helper()
	data, err := os.ReadFile("/proc/net/route")
	if err != nil {
		t.Fatal(err)
	}
	for _, line := range strings.Split(string(data), "\n")[1:] {
		if f := strings.Fields(line); len(f) > 2 && f[1] == "00000000" {
			return true
		}
	}
	return false
}
