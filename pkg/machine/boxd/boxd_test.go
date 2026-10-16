package boxd

import (
	"context"
	"errors"
	"net"
	"os"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
	"testing"
	"time"

	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"

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
		cfg.Install = machine.InstallSpec{GuestOS: machine.Linux, Media: "latest", Log: testLog{t}}
	}
	machinetest.Run(t, New(), cfg)
}

// TestConformanceFakeAPI runs the conformance suite against an in-process fake
// of boxd's API, which proves the driver's use of the API and the command
// lines it sends, not boxd. Its guests' commands run on this host, and the
// suite checks that a guest reports linux, so it runs on Linux.
func TestConformanceFakeAPI(t *testing.T) {
	if runtime.GOOS != "linux" {
		t.Skip("the fake boxd's guests run on the host, and boxd guests are linux")
	}
	// A short directory: unix socket paths are limited to about 100 bytes.
	dir, err := os.MkdirTemp("", "boxd")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(dir) })
	d, fake := fakeDriver(t, dir)

	machinetest.Run(t, d, machinetest.Config{
		Install: machine.InstallSpec{GuestOS: machine.Linux, Media: "latest", Log: testLog{t}},
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

// TestTagSnapshot checks that a tag is a snapshot of its own beside the
// layer's, named so that a listing says what it is, and that untagging deletes
// only it.
func TestTagSnapshot(t *testing.T) {
	if runtime.GOOS != "linux" {
		t.Skip("the fake boxd's guests run on the host, and boxd guests are linux")
	}
	dir, err := os.MkdirTemp("", "boxd")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(dir) })
	d, fake := fakeDriver(t, dir)
	ctx := context.Background()

	base := machine.Layer{ID: "0123456789abcdef", Dir: filepath.Join(dir, "layer")}
	if err := os.MkdirAll(base.Dir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := d.Install(ctx, machine.InstallSpec{GuestOS: machine.Linux}, base); err != nil {
		t.Fatal(err)
	}
	names := func() []string {
		fake.mu.Lock()
		defer fake.mu.Unlock()
		if len(fake.vms) != 0 {
			t.Fatalf("%d machines were left behind", len(fake.vms))
		}
		var names []string
		for name := range fake.snapshots {
			names = append(names, name)
		}
		slices.Sort(names)
		return names
	}
	layer := names()
	// Every call the tags make is reported, with its answer and its time.
	var calls strings.Builder
	machine.SetDebug(&calls)
	t.Cleanup(func() { machine.SetDebug(nil) })
	defer func() {
		for _, want := range []string{"boxd CreateVmFromSnapshot {snapshot:", "boxd GetSnapshot {name:\"dvm-default--0123456789ab-tag-", "status:\"ready\"", "boxd DeleteSnapshot {name:"} {
			if !strings.Contains(calls.String(), want) {
				t.Errorf("debug output lacks %q:\n%s", want, calls.String())
			}
		}
	}()
	for _, ref := range []string{"discobox/base:latest", "discobox-base:latest", "discobox/base:latest"} {
		if err := d.TagLayer(ctx, base, ref); err != nil {
			t.Fatal(err)
		}
	}
	got := names()
	if len(got) != 3 || !slices.Contains(got, layer[0]) {
		t.Fatalf("snapshots %v, want the layer's %v and one for each of two tags", got, layer)
	}
	for _, name := range got {
		if name != layer[0] && !strings.HasPrefix(name, "dvm-default--0123456789ab-tag-discobox-base-latest-") {
			t.Fatalf("a tag's snapshot is named %s", name)
		}
	}
	if err := d.UntagLayer(ctx, base, "discobox/base:latest"); err != nil {
		t.Fatal(err)
	}
	if got := names(); len(got) != 2 || !slices.Contains(got, layer[0]) {
		t.Fatalf("snapshots %v after untagging one of two", got)
	}

	// A snapshot deleted behind the driver's back is a layer that is gone.
	if ok, err := d.LayerExists(ctx, base); err != nil || !ok {
		t.Fatalf("layer exists = %v, %v", ok, err)
	}
	fake.mu.Lock()
	delete(fake.snapshots, layer[0])
	fake.mu.Unlock()
	if ok, err := d.LayerExists(ctx, base); err != nil || ok {
		t.Fatalf("layer exists after its snapshot was deleted = %v, %v", ok, err)
	}
}

// TestCopyFromMissing checks that copying out a path the guest does not have
// is an error from CopyFrom itself, carrying the guest's message, not an
// empty tar.
func TestCopyFromMissing(t *testing.T) {
	if runtime.GOOS != "linux" {
		t.Skip("the fake boxd's guests run on the host, and boxd guests are linux")
	}
	d, _ := fakeDriver(t, t.TempDir())
	api, _ := d.api.API()
	ctx := context.Background()
	created, err := api.CreateVm(ctx, &boxdapi.CreateVmRequest{})
	if err != nil {
		t.Fatal(err)
	}
	m := newVM(api, vmRef{ID: created.GetVmId(), Name: created.GetName()})
	defer m.stop()
	if _, err := m.CopyFrom(ctx, "/no/such/file"); err == nil || !strings.Contains(err.Error(), "no such file") {
		t.Fatalf("copy from a missing path: %v", err)
	}
}

// TestCommand checks that a request becomes one line the guest's shell reads
// back word for word, quotes and all, run through sudo as root by default.
func TestCommand(t *testing.T) {
	req := machine.ExecRequest{Argv: []string{"sh", "-c", `echo "it's $HOME"`, "a b"}, Env: []string{"K=it's"}, Dir: "/tmp/a dir", User: "alice"}
	got := command(req)
	want := `'sudo' '-n' '-H' '-u' 'alice' '--' 'env' '-C' '/tmp/a dir' '--' 'K=it'\''s' 'sh' '-c' 'echo "it'\''s $HOME"' 'a b'`
	if got != want {
		t.Fatalf("command:\n got %s\nwant %s", got, want)
	}
	back, err := parseCommand(got)
	if err != nil || !slices.Equal(back.Argv, req.Argv) || !slices.Equal(back.Env, req.Env) || back.Dir != req.Dir || back.User != req.User {
		t.Fatalf("read back %+v, %v", back, err)
	}
	if def := command(machine.ExecRequest{Argv: []string{"true"}}); def != `'sudo' '-n' '-H' '-u' 'root' '--' 'env' '-C' '/' '--' 'true'` {
		t.Fatalf("default command: %s", def)
	}
}

// TestKillGivesUp checks that Kill returns, and Done closes with an error that
// says so, when boxd stops answering: an orderly stop's fallback
// (Booted.Shutdown) calls Kill with no deadline and then waits on Done.
func TestKillGivesUp(t *testing.T) {
	defer func(poll, limit time.Duration) { pollEvery, killLimit = poll, limit }(pollEvery, killLimit)
	pollEvery, killLimit = 100*time.Millisecond, time.Second
	d, fake := fakeDriver(t, t.TempDir())
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

// TestShutdownFlushesFirst stops a machine only once the guest's writes are
// flushed. A flush that fails is Shutdown's error, with no stop asked, so the
// caller falls back to Kill and cuts no layer from the disk.
func TestShutdownFlushesFirst(t *testing.T) {
	if runtime.GOOS != "linux" {
		t.Skip("the fake boxd's guests run on the host, and boxd guests are linux")
	}
	defer func(poll time.Duration) { pollEvery = poll }(pollEvery)
	pollEvery = 50 * time.Millisecond
	for _, fails := range []bool{false, true} {
		d, fake := fakeDriver(t, t.TempDir())
		fake.syncFails.Store(fails)
		api, _ := d.api.API()
		ctx := context.Background()
		created, err := api.CreateVm(ctx, &boxdapi.CreateVmRequest{})
		if err != nil {
			t.Fatal(err)
		}
		m := newVM(api, vmRef{ID: created.GetVmId(), Name: created.GetName()})
		t.Cleanup(m.stop)
		err = m.Shutdown(ctx, false)
		if (err != nil) != fails {
			t.Fatalf("with the flush failing %v, Shutdown = %v", fails, err)
		}
		want := "stopped"
		if fails {
			want = "running"
		} else {
			select {
			case <-m.Done():
			case <-time.After(30 * time.Second):
				t.Fatal("the machine did not stop")
			}
			if err := m.Err(); err != nil {
				t.Fatalf("an orderly stop ended in %v", err)
			}
		}
		fake.mu.Lock()
		v, _ := fake.vm(created.GetVmId())
		state := v.status
		fake.mu.Unlock()
		if state != want {
			t.Fatalf("with the flush failing %v, the machine is %s, want %s", fails, state, want)
		}
	}
}

// TestFirstStartUndone leaves an instance of a base that failed its first
// start as it was before it: no machine, and no vm.json, so the next start
// makes one again, with its URL.
func TestFirstStartUndone(t *testing.T) {
	if runtime.GOOS != "linux" {
		t.Skip("the fake boxd's guests run on the host, and boxd guests are linux")
	}
	dir, err := os.MkdirTemp("", "boxd")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(dir) })
	d, fake := fakeDriver(t, dir)
	ctx := context.Background()
	base := machine.Base{Name: "ubuntu-24.04", ID: "ubuntu-24.04", GuestOS: machine.Linux, Options: map[string]string{"image": "ubuntu-24.04"}}
	inst := machine.InstanceSpec{ID: "first", Dir: filepath.Join(dir, "inst"), GuestOS: machine.Linux, Base: &base}
	if err := d.Prepare(ctx, inst); err != nil {
		t.Fatal(err)
	}
	fake.mu.Lock()
	fake.proxyErr = errors.New("proxy unavailable")
	fake.mu.Unlock()
	if _, err := d.Boot(ctx, inst, machine.BootOptions{}); err == nil {
		t.Fatal("a first start with no proxy succeeded")
	}
	fake.mu.Lock()
	left := len(fake.vms)
	fake.proxyErr = nil
	fake.mu.Unlock()
	if left != 0 {
		t.Fatalf("a failed first start left %d machines", left)
	}
	if _, err := os.Stat(filepath.Join(inst.Dir, vmFile)); !os.IsNotExist(err) {
		t.Fatalf("a failed first start left vm.json: %v", err)
	}
	m, err := d.Boot(ctx, inst, machine.BootOptions{})
	if err != nil {
		t.Fatalf("the next start: %v", err)
	}
	t.Cleanup(func() { _ = m.Kill(context.Background()); _ = d.Destroy(context.Background(), inst) })
	if svc, err := d.Service(ctx, inst); err != nil || svc.URL == "" {
		t.Fatalf("the next start's service = %+v, %v", svc, err)
	}
}

// fakeDriver serves a fake boxd API on loopback and returns a driver bound to
// it.
func fakeDriver(t *testing.T, dir string) (*Driver, *fakeAPI) {
	t.Helper()
	fake := newFakeAPI(dir)
	listener, err := (&net.ListenConfig{}).Listen(t.Context(), "tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	server := grpc.NewServer()
	boxdapi.RegisterBoxdApiServer(server, fake)
	go func() { _ = server.Serve(listener) }()
	t.Cleanup(server.Stop)

	conn, err := grpc.NewClient(listener.Addr().String(), append(debugOptions(), grpc.WithTransportCredentials(insecure.NewCredentials()))...)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = conn.Close() })
	d := &Driver{api: &client{}}
	d.api.once.Do(func() { d.api.api = boxdapi.NewBoxdApiClient(conn) })
	return d, fake
}

type testLog struct{ t *testing.T }

func (l testLog) Write(p []byte) (int, error) {
	l.t.Log(strings.TrimRight(string(p), "\n"))
	return len(p), nil
}
