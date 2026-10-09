package boxd

import (
	"context"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"

	"github.com/discobox-ai/vm/pkg/guest"
	"github.com/discobox-ai/vm/pkg/machine"
	"github.com/discobox-ai/vm/pkg/machine/boxd/internal/boxdapi"
	"github.com/discobox-ai/vm/pkg/machine/machinetest"
)

// BaseEnv names a layer directory whose layer.json points at an installed
// boxd snapshot, so the real-boxd test skips Install. Unset, it installs.
const BaseEnv = "DISCO_VM_BOXD_BASE"

// TestConformance runs the conformance suite against boxd itself. It needs an
// account, so it runs only where BOXD_API_KEY is set.
func TestConformance(t *testing.T) {
	if os.Getenv(APIKeyEnv) == "" && os.Getenv(tokenEnv) == "" {
		t.Skipf("set %s to run the conformance suite against boxd", APIKeyEnv)
	}
	cfg := machinetest.Config{
		Timeout: 5 * time.Minute,
		// Not /tmp: Ubuntu clears it at boot, and the suite reads its marker
		// back after one.
		ScratchDir: "/var/lib",
	}
	if dir := os.Getenv(BaseEnv); dir != "" {
		cfg.Base = &machine.Layer{ID: "base", Dir: dir}
	} else {
		cfg.Install = machine.InstallSpec{GuestOS: machine.Linux, Media: "latest", Agent: linuxAgent(t), Log: testLog{t}}
	}
	machinetest.Run(t, New(), cfg)
}

// TestConformanceFakeAPI runs the conformance suite against an in-process fake
// of boxd's API, which proves the driver's use of the API and the relay, not
// boxd. Its guests are agent processes on this host, and the suite checks that
// a guest reports linux, so it runs on Linux.
func TestConformanceFakeAPI(t *testing.T) {
	if runtime.GOOS != "linux" {
		t.Skip("the fake boxd's guests run on the host, and boxd guests are linux")
	}
	agent := linuxAgent(t)
	// A short directory: unix socket paths are limited to about 100 bytes.
	dir, err := os.MkdirTemp("", "boxd")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(dir) })
	d, fake := fakeDriver(t, agent, dir)
	t.Cleanup(fake.close)

	machinetest.Run(t, d, machinetest.Config{
		Install: machine.InstallSpec{GuestOS: machine.Linux, Media: "latest", Agent: agent, Log: testLog{t}},
		// Not the fake's default size, so every cold boot resizes.
		Boot:       machine.BootOptions{CPUs: 4, Memory: 16 << 30},
		Timeout:    30 * time.Second,
		ScratchDir: "/scratch",
	})
	fake.mu.Lock()
	defer fake.mu.Unlock()
	if left := len(fake.vms); left != 0 {
		t.Errorf("%d machines were left behind", left)
	}
	if left := len(fake.snapshots); left != 2 {
		t.Errorf("%d snapshots remain, want the 2 layers'", left)
	}
}

// TestWarmAtAnotherSize stages an image at a size its layer was not
// snapshotted at, which takes a snapshot of its own that Cool deletes.
func TestWarmAtAnotherSize(t *testing.T) {
	if runtime.GOOS != "linux" {
		t.Skip("the fake boxd's guests run on the host, and boxd guests are linux")
	}
	agent := linuxAgent(t)
	dir, err := os.MkdirTemp("", "boxd")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(dir) })
	d, fake := fakeDriver(t, agent, dir)
	t.Cleanup(fake.close)
	ctx := context.Background()

	base := machine.Layer{ID: "base", Dir: filepath.Join(dir, "layer")}
	if err := os.MkdirAll(base.Dir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := d.Install(ctx, machine.InstallSpec{GuestOS: machine.Linux, Agent: agent}, base); err != nil {
		t.Fatal(err)
	}
	spec := machine.WarmSpec{GuestOS: machine.Linux, Chain: []machine.Layer{base}, Dir: filepath.Join(dir, "warm"), CPUs: 8, Memory: 32 << 30}
	if err := os.MkdirAll(spec.Dir, 0o755); err != nil {
		t.Fatal(err)
	}
	if _, err := d.Warm(ctx, spec); err != nil {
		t.Fatal(err)
	}
	var stage snapshotRef
	if err := readJSON(filepath.Join(spec.Dir, stageFile), &stage); err != nil {
		t.Fatal(err)
	}
	if !stage.Own || stage.CPUs != 8 || stage.Memory != 32<<30 {
		t.Fatalf("stage %+v, want its own snapshot at 8 CPUs and 32 GiB", stage)
	}
	if w, err := d.Warmth(ctx, spec); err != nil || w != (machine.Warmth{Mode: machine.Resume, Clones: -1}) {
		t.Fatalf("warmth %+v, %v", w, err)
	}
	if err := d.Cool(ctx, spec); err != nil {
		t.Fatal(err)
	}
	fake.mu.Lock()
	defer fake.mu.Unlock()
	if _, ok := fake.snapshots[stage.Name]; ok {
		t.Fatal("Cool left the stage's own snapshot")
	}
	if len(fake.vms) != 0 {
		t.Fatalf("%d machines were left behind", len(fake.vms))
	}
}

// TestDialFailure checks that a relay that cannot reach its port is an error
// from Dial, carrying the relay's own message, not a conn that reads EOF.
func TestDialFailure(t *testing.T) {
	dir, err := os.MkdirTemp("", "boxd")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(dir) })
	d, fake := fakeDriver(t, "unused", dir)
	t.Cleanup(fake.close)
	api, _ := d.api.API()
	ctx := context.Background()
	created, err := api.CreateVm(ctx, &boxdapi.CreateVmRequest{})
	if err != nil {
		t.Fatal(err)
	}
	m := newVM(api, vmRef{ID: created.GetVmId(), Name: created.GetName()})
	defer func() { _ = m.Kill(ctx) }()
	_, err = m.Dial(ctx, guest.AgentPort)
	if err == nil || !strings.Contains(err.Error(), "agent.sock") {
		t.Fatalf("dial with no agent listening: %v", err)
	}
}

// TestKillGivesUp checks that Kill returns, and Done closes with an error that
// says so, when boxd stops answering: an orderly stop's fallback
// (Booted.Shutdown) calls Kill with no deadline and then waits on Done.
func TestKillGivesUp(t *testing.T) {
	defer func(poll, limit time.Duration) { pollEvery, killLimit = poll, limit }(pollEvery, killLimit)
	pollEvery, killLimit = 100*time.Millisecond, time.Second
	d, fake := fakeDriver(t, "unused", t.TempDir())
	api, _ := d.api.API()
	created, err := api.CreateVm(context.Background(), &boxdapi.CreateVmRequest{})
	if err != nil {
		t.Fatal(err)
	}
	m := newVM(api, vmRef{ID: created.GetVmId(), Name: created.GetName()})
	fake.down.Store(true)

	returned := make(chan error, 1)
	go func() { returned <- m.Kill(context.Background()) }()
	select {
	case err := <-returned:
		if err == nil || !strings.Contains(err.Error(), "may still be running") {
			t.Fatalf("kill with boxd down: %v", err)
		}
	case <-time.After(30 * time.Second):
		t.Fatal("Kill did not give up")
	}
	select {
	case <-m.Done():
	default:
		t.Fatal("Done is open after Kill gave up")
	}
	if m.Err() == nil {
		t.Fatal("a machine given up reports no error")
	}
}

// TestKillDuringReboot checks that a guest's reboot does not start a machine
// that Kill has already been asked to stop.
func TestKillDuringReboot(t *testing.T) {
	defer func(poll time.Duration) { pollEvery = poll }(pollEvery)
	pollEvery = time.Hour // the test drives afterHalt itself
	d, fake := fakeDriver(t, "unused", t.TempDir())
	api, _ := d.api.API()
	ctx := context.Background()
	created, err := api.CreateVm(ctx, &boxdapi.CreateVmRequest{})
	if err != nil {
		t.Fatal(err)
	}
	m := newVM(api, vmRef{ID: created.GetVmId(), Name: created.GetName()})
	defer m.stop()
	m.killed.Store(true) // Kill has begun
	if err := m.afterHalt(ctx, "reboot"); err != nil {
		t.Fatal(err)
	}
	fake.mu.Lock()
	defer fake.mu.Unlock()
	if v, _ := fake.vm(created.GetVmId()); v.status != "stopped" {
		t.Fatalf("a killed machine is %s after its guest rebooted", v.status)
	}
}

// fakeDriver serves a fake boxd API on loopback and returns a driver bound to
// it.
func fakeDriver(t *testing.T, agent, dir string) (*Driver, *fakeAPI) {
	t.Helper()
	fake := newFakeAPI(agent, dir)
	listener, err := (&net.ListenConfig{}).Listen(t.Context(), "tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	server := grpc.NewServer()
	boxdapi.RegisterBoxdApiServer(server, fake)
	go func() { _ = server.Serve(listener) }()
	t.Cleanup(server.Stop)

	conn, err := grpc.NewClient(listener.Addr().String(), grpc.WithTransportCredentials(insecure.NewCredentials()))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = conn.Close() })
	d := &Driver{api: &client{}}
	d.api.once.Do(func() { d.api.api = boxdapi.NewBoxdApiClient(conn) })
	return d, fake
}

// linuxAgent is the disco-vm binary for a linux/amd64 guest, which is every
// boxd machine today: $DISCO_VM_TEST_BINARY, or built.
func linuxAgent(t *testing.T) string {
	t.Helper()
	if agent := os.Getenv("DISCO_VM_TEST_BINARY"); agent != "" {
		return agent
	}
	agent := filepath.Join(t.TempDir(), "disco-vm")
	build := exec.CommandContext(t.Context(), "go", "build", "-o", agent, "../../../cmd/disco-vm")
	build.Env = append(os.Environ(), "GOOS=linux", "CGO_ENABLED=0")
	if runtime.GOOS == "linux" {
		build.Env = append(build.Env, "GOARCH="+runtime.GOARCH)
	} else {
		build.Env = append(build.Env, "GOARCH=amd64")
	}
	build.Stdout, build.Stderr = os.Stdout, os.Stderr
	if err := build.Run(); err != nil {
		t.Fatal(err)
	}
	return agent
}

type testLog struct{ t *testing.T }

func (l testLog) Write(p []byte) (int, error) {
	l.t.Log(strings.TrimRight(string(p), "\n"))
	return len(p), nil
}
