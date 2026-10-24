package boxd

import (
	"context"
	"errors"
	"fmt"
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

// BaseEnv names an image of the default project by its ID, a snapshot
// dvm-default--<ID>, for the real-boxd test to start from. Unset, it starts
// from boxd's default image.
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
	cfg.Image = os.Getenv(BaseEnv)
	machinetest.Run(t, New(""), cfg)
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
		// Not the fake's default size, so every clone is made at its own.
		CPUs:       4,
		Memory:     16 << 30,
		Timeout:    30 * time.Second,
		ScratchDir: "/scratch",
	})
	fake.mu.Lock()
	defer fake.mu.Unlock()
	if left := len(fake.vms); left != 0 {
		t.Errorf("%d machines were left behind", left)
	}
	// The first snapshot was deleted to show the second stands alone.
	if left := len(fake.snapshots); left != 2 {
		t.Errorf("%d snapshots remain, want the 2 the suite has yet to delete", left)
	}
}

// snapshotOfBase makes the image id, as every boxd image is made: a snapshot
// of an instance of boxd's default image.
func snapshotOfBase(ctx context.Context, d *Driver, id string) (machine.Image, error) {
	bases, err := d.Bases(ctx)
	if err != nil || len(bases) == 0 {
		return machine.Image{}, fmt.Errorf("bases = %v, %w", bases, err)
	}
	inst, err := d.Create(ctx, machine.InstanceSpec{Name: "for-" + id[:8], Base: &bases[0], GuestOS: machine.Linux})
	if err != nil {
		return machine.Image{}, err
	}
	return d.Commit(ctx, inst, machine.Image{ID: id, GuestOS: machine.Linux})
}

// TestTagsAreSnapshots checks that a tag is a snapshot of its own beside the
// image's, named so that a listing says what it is and the ref reads back,
// that moving a tag deletes it from the other image, and that untagging
// deletes only it.
func TestTagsAreSnapshots(t *testing.T) {
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

	base, err := snapshotOfBase(ctx, d, "0123456789abcdef")
	if err != nil {
		t.Fatal(err)
	}
	other, err := snapshotOfBase(ctx, d, "fedcba9876543210")
	if err != nil {
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
	if got, want := names(), []string{"dvm-default--0123456789abcdef", "dvm-default--fedcba9876543210"}; !slices.Equal(got, want) {
		t.Fatalf("image snapshots %v, want %v", got, want)
	}
	// Every call the tags make is reported, with its answer and its time.
	var calls strings.Builder
	machine.SetDebug(&calls)
	t.Cleanup(func() { machine.SetDebug(nil) })
	defer func() {
		for _, want := range []string{"boxd CreateVmFromSnapshot {snapshot:", "boxd GetSnapshot {name:\"dvm-default--0123456789abcdef--t-discobox-2fbase-3alatest\"", "status:\"ready\"", "boxd DeleteSnapshot {name:"} {
			if !strings.Contains(calls.String(), want) {
				t.Errorf("debug output lacks %q:\n%s", want, calls.String())
			}
		}
	}()
	for _, tag := range []machine.Tag{
		{Ref: "discobox/base:latest", Image: other.ID},
		{Ref: "discobox/base:latest", Image: base.ID},
		{Ref: "discobox-base:v1.0", Image: base.ID},
		{Ref: "discobox/base:latest", Image: base.ID},
	} {
		if err := d.Tag(ctx, tag.Ref, tag.Image); err != nil {
			t.Fatal(err)
		}
	}
	want := []string{
		"dvm-default--0123456789abcdef",
		"dvm-default--0123456789abcdef--t-discobox-2dbase-3av1-2e0",
		"dvm-default--0123456789abcdef--t-discobox-2fbase-3alatest",
		"dvm-default--fedcba9876543210",
	}
	if got := names(); !slices.Equal(got, want) {
		t.Fatalf("snapshots %v, want %v", got, want)
	}
	tags, err := d.Tags(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if want := []machine.Tag{{Ref: "discobox-base:v1.0", Image: base.ID}, {Ref: "discobox/base:latest", Image: base.ID}}; !slices.Equal(tags, want) {
		t.Fatalf("tags %v, want %v", tags, want)
	}
	if images, err := d.Images(ctx); err != nil || len(images) != 2 {
		t.Fatalf("images %v, %v; want the two, and no tag's", images, err)
	}
	if err := d.Untag(ctx, "discobox/base:latest"); err != nil {
		t.Fatal(err)
	}
	if got := names(); len(got) != 3 || slices.Contains(got, want[2]) {
		t.Fatalf("snapshots %v after untagging one of two", got)
	}

	// A snapshot deleted behind the driver's back is an image that is gone.
	fake.mu.Lock()
	delete(fake.snapshots, "dvm-default--fedcba9876543210")
	fake.mu.Unlock()
	if _, err := d.Image(ctx, other.ID); !errors.Is(err, machine.ErrNotFound) {
		t.Fatalf("an image whose snapshot was deleted: %v, want ErrNotFound", err)
	}
}

func TestEscapeRef(t *testing.T) {
	for _, ref := range []string{"a:latest", "org/app_x:V1.0-rc", "x-y:z"} {
		escaped := escapeRef(ref)
		for _, r := range escaped {
			if (r < 'a' || r > 'z') && (r < '0' || r > '9') && r != '-' {
				t.Fatalf("%q escapes to %q, which has %q", ref, escaped, r)
			}
		}
		if back, ok := unescapeRef(escaped); !ok || back != ref {
			t.Fatalf("%q escapes to %q, which reads back as %q, %v", ref, escaped, back, ok)
		}
	}
	for _, bad := range []string{"a-", "a-z1", "-4"} {
		if _, ok := unescapeRef(bad); ok {
			t.Errorf("%q read back", bad)
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
	base := machine.Base{Name: "ubuntu-24.04", ID: "ubuntu-24.04", GuestOS: machine.Linux, Options: map[string]string{"image": "ubuntu-24.04"}}
	spec := machine.InstanceSpec{Name: "first", GuestOS: machine.Linux, Base: &base}
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
	d := &Driver{api: &client{}, project: machine.DefaultProject}
	d.api.once.Do(func() { d.api.api = boxdapi.NewBoxdApiClient(conn) })
	return d, fake
}
