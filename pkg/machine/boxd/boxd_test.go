package boxd

import (
	"context"
	"errors"
	"net"
	"os"
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

// FromEnv names a committed VM of the default pool for the real-boxd
// test to start from. Unset, it starts from a fresh VM of boxd's default
// image.
const FromEnv = "DISCO_VM_BOXD_FROM"

// TestConformance runs the conformance suite against boxd itself. It needs an
// account, so it runs only where BOXD_API_KEY is set.
func TestConformance(t *testing.T) {
	if os.Getenv(APIKeyEnv) == "" && os.Getenv(TokenEnv) == "" {
		t.Skipf("set %s to run the conformance suite against boxd", APIKeyEnv)
	}
	cfg := machinetest.Config{
		Timeout: 5 * time.Minute,
		// Not /tmp: Ubuntu clears it at boot, and the suite reads its marker
		// back after one.
		ScratchDir: "/var/lib",
	}
	cfg.From = os.Getenv(FromEnv)
	machinetest.Run(t, New(machine.Config{Keys: machine.KeyFromEnv("boxd")}), cfg)
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
	// Registered first, so it runs once the suite's own cleanups have.
	t.Cleanup(func() {
		fake.mu.Lock()
		defer fake.mu.Unlock()
		if left := len(fake.vms); left != 0 {
			t.Errorf("%d machines were left behind", left)
		}
		if left := len(fake.snapshots); left != 0 {
			t.Errorf("%d snapshots were left behind", left)
		}
	})

	machinetest.Run(t, d, machinetest.Config{
		// Not the fake's default size, so every clone is made at its own.
		CPUs:       4,
		Memory:     16 << 30,
		Timeout:    30 * time.Second,
		ScratchDir: "/scratch",
		Keyed: func(keys machine.KeySource) machine.Driver {
			return fakeKeyed(t, fake, machine.DefaultPool, keys)
		},
	})
}

// commitFresh makes the committed VM name, as every boxd one is made: a
// fresh VM of the driver's image, committed.
func commitFresh(ctx context.Context, d *Driver, name string) (machine.Instance, error) {
	inst, err := d.Create(ctx, machine.InstanceSpec{Name: name})
	if err != nil {
		return machine.Instance{}, err
	}
	return d.Commit(ctx, inst)
}

// TestCommittedVMsAreSnapshots checks that a committed VM is a snapshot under
// its name and no machine, that its name is taken from machines too, that a
// VM created from it outlives its removal, that a committed VM never boots or
// commits again, and that what disco-vm or anyone else named otherwise is
// not listed.
func TestCommittedVMsAreSnapshots(t *testing.T) {
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

	// Every call is reported, with its answer and its time.
	var calls strings.Builder
	machine.SetDebug(&calls)
	t.Cleanup(func() { machine.SetDebug(nil) })
	base, err := commitFresh(ctx, d, "base")
	if err != nil {
		t.Fatal(err)
	}
	machine.SetDebug(nil)
	for _, want := range []string{"boxd CreateVm {name:\"dvm-default--base\"", "image_ref:\"ubuntu-24.04\"", "boxd CreateSnapshot {vm:", "status:\"ready\"", "boxd DestroyVm {vm_id:"} {
		if !strings.Contains(calls.String(), want) {
			t.Errorf("debug output lacks %q:\n%s", want, calls.String())
		}
	}
	if base.Name != "base" || base.ID == "" || base.GuestOS != machine.Linux {
		t.Fatalf("committed %+v", base)
	}
	state := func() (vms, snaps []string) {
		fake.mu.Lock()
		defer fake.mu.Unlock()
		for _, v := range fake.vms {
			vms = append(vms, v.name)
		}
		for name := range fake.snapshots {
			snaps = append(snaps, name)
		}
		slices.Sort(vms)
		slices.Sort(snaps)
		return vms, snaps
	}
	if vms, snaps := state(); len(vms) != 0 || !slices.Equal(snaps, []string{"dvm-default--base"}) {
		t.Fatalf("machines %v and snapshots %v, want none and the committed VM's", vms, snaps)
	}
	fake.mu.Lock()
	snapID := fake.snapshots["dvm-default--base"].id
	fake.mu.Unlock()
	if base.ID != snapID {
		t.Fatalf("the committed VM's ID is %s, not its snapshot's, %s", base.ID, snapID)
	}
	if got, err := d.Instance(ctx, "base"); err != nil || got.ID != base.ID {
		t.Fatalf("instance base = %+v, %v; want the snapshot", got, err)
	}
	if _, err := d.Create(ctx, machine.InstanceSpec{Name: "base"}); !errors.Is(err, machine.ErrExists) {
		t.Fatalf("a fresh VM of a committed VM's name: %v, want ErrExists", err)
	}
	if _, err := d.Boot(ctx, base, machine.BootOptions{}); !errors.Is(err, machine.ErrCommitted) {
		t.Fatalf("boot of a committed VM: %v, want ErrCommitted", err)
	}
	if _, err := d.Commit(ctx, base); !errors.Is(err, machine.ErrCommitted) {
		t.Fatalf("commit of a committed VM: %v, want ErrCommitted", err)
	}
	if _, err := d.Attach(ctx, base); !errors.Is(err, machine.ErrNotRunning) {
		t.Fatalf("attach to a committed VM: %v, want ErrNotRunning", err)
	}
	// Whatever boxd answers GetVm of a snapshot's ID, a committed VM is not
	// running, so the engine reads it as stopped.
	fake.snapshotIDRefused.Store(true)
	if _, err := d.Attach(ctx, base); !errors.Is(err, machine.ErrNotRunning) {
		t.Fatalf("attach to a committed VM whose ID GetVm refuses: %v, want ErrNotRunning", err)
	}
	fake.snapshotIDRefused.Store(false)
	if _, err := d.Service(ctx, base); !errors.Is(err, machine.ErrCommitted) {
		t.Fatalf("the service of a committed VM: %v, want ErrCommitted", err)
	}

	// What is not a VM's name in the pool is not listed: an image's
	// snapshot and a tag's from before, another pool's, and one the
	// account made by hand.
	fake.mu.Lock()
	for _, name := range []string{
		"dvm-default--" + strings.Repeat("ab", 32),
		"dvm-default--0123456789abcdef--t-discobox-2fbase-3alatest",
		"dvm-other--base",
		"hand-made",
	} {
		fake.snapshots[name] = &fakeSnapshot{id: "snap_" + name, name: name, dir: t.TempDir()}
	}
	fake.mu.Unlock()
	all, err := d.Instances(ctx)
	if err != nil || len(all) != 1 || all[0].Name != "base" {
		t.Fatalf("instances %+v, %v; want base alone", all, err)
	}

	copied, err := d.Create(ctx, machine.InstanceSpec{Name: "copy", From: &base})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = d.Destroy(context.Background(), copied) })
	if copied.ID == base.ID || copied.From != "" {
		t.Fatalf("a VM created from a committed one: %+v", copied)
	}
	if _, err := d.Create(ctx, machine.InstanceSpec{Name: "copy-of-copy", From: &copied}); !errors.Is(err, machine.ErrNotFound) {
		t.Fatalf("a VM created from a machine, which boxd cannot restore: %v, want ErrNotFound", err)
	}
	if err := d.Destroy(ctx, base); err != nil {
		t.Fatal(err)
	}
	if vms, snaps := state(); !slices.Equal(vms, []string{"dvm-default--copy"}) || slices.Contains(snaps, "dvm-default--base") {
		t.Fatalf("machines %v and snapshots %v after removing the committed VM", vms, snaps)
	}
	if _, err := d.Instance(ctx, "copy"); err != nil {
		t.Fatalf("the copy after its committed VM's removal: %v", err)
	}
	if _, err := d.Create(ctx, machine.InstanceSpec{Name: "again", From: &base}); !errors.Is(err, machine.ErrNotFound) {
		t.Fatalf("a VM created from a removed committed VM: %v, want ErrNotFound", err)
	}
}

// TestFreshVMsUseTheConfiguredImage checks that a fresh VM is made from the
// driver's configured image, and from boxd's default one when none is.
func TestFreshVMsUseTheConfiguredImage(t *testing.T) {
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
	for _, base := range []string{"", "debian-13"} {
		d.base = base
		inst, err := d.Create(ctx, machine.InstanceSpec{Name: "fresh"})
		if err != nil {
			t.Fatal(err)
		}
		fake.mu.Lock()
		image := fake.vms[inst.ID].imageRef
		fake.mu.Unlock()
		want := base
		if want == "" {
			want = fake.defaultImage
		}
		if image != want {
			t.Errorf("configured %q: made from %q, want %q", base, image, want)
		}
		if err := d.Destroy(ctx, inst); err != nil {
			t.Fatal(err)
		}
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
// caller falls back to Kill and cuts no snapshot from the disk.
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

// TestCreateUndone leaves nothing of an instance whose create failed partway,
// so the name can be made again, with its URL.
func TestCreateUndone(t *testing.T) {
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
	spec := machine.InstanceSpec{Name: "first"}
	fake.mu.Lock()
	fake.proxyErr = errors.New("proxy unavailable")
	fake.mu.Unlock()
	if _, err := d.Create(ctx, spec); err == nil {
		t.Fatal("a create with no proxy succeeded")
	}
	fake.mu.Lock()
	left := len(fake.vms)
	fake.proxyErr = nil
	fake.mu.Unlock()
	if left != 0 {
		t.Fatalf("a failed create left %d machines", left)
	}
	inst, err := d.Create(ctx, spec)
	if err != nil {
		t.Fatalf("the next create: %v", err)
	}
	t.Cleanup(func() { _ = d.Destroy(context.Background(), inst) })
	if svc, err := d.Service(ctx, inst); err != nil || svc.URL == "" {
		t.Fatalf("the next create's service = %+v, %v", svc, err)
	}
	if _, err := d.Create(ctx, spec); !errors.Is(err, machine.ErrExists) {
		t.Fatalf("a second instance of the name: %v, want ErrExists", err)
	}
}

// fakeDriver serves a fake boxd API on loopback and returns a driver bound to
// it, which calls it as the owner's interactive session.
func fakeDriver(t *testing.T, dir string) (*Driver, *fakeAPI) {
	t.Helper()
	fake := newFakeAPI(dir)
	listener, err := (&net.ListenConfig{}).Listen(t.Context(), "tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	server := grpc.NewServer(grpc.UnaryInterceptor(fake.unaryAuth), grpc.StreamInterceptor(fake.streamAuth))
	boxdapi.RegisterBoxdApiServer(server, fake)
	go func() { _ = server.Serve(listener) }()
	t.Cleanup(server.Stop)
	tokens := fake.tokenServer()
	t.Cleanup(tokens.Close)
	fake.addr, fake.tokenURL = listener.Addr().String(), tokens.URL
	return fakeKeyed(t, fake, machine.DefaultPool, machine.StaticKey(machine.Key{Driver: "boxd", Secret: fakeSession})), fake
}

// fakeKeyed is a driver of the fake API for a pool, with a key.
func fakeKeyed(t *testing.T, fake *fakeAPI, pool string, keys machine.KeySource) *Driver {
	t.Helper()
	c := &client{tokenURL: fake.tokenURL, keys: keys}
	opts := append(debugOptions(), c.authOptions()...)
	conn, err := grpc.NewClient(fake.addr, append(opts, grpc.WithTransportCredentials(insecure.NewCredentials()))...)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = conn.Close() })
	c.once.Do(func() { c.api = boxdapi.NewBoxdApiClient(conn) })
	return &Driver{api: c, pool: pool}
}
