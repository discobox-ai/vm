package served_test

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/discobox-ai/vm/pkg/engine"
	"github.com/discobox-ai/vm/pkg/guest"
	"github.com/discobox-ai/vm/pkg/machine"
	"github.com/discobox-ai/vm/pkg/machine/fake"
	"github.com/discobox-ai/vm/pkg/machine/local"
	"github.com/discobox-ai/vm/pkg/machine/machinetest"
	"github.com/discobox-ai/vm/pkg/machine/served"
)

// agent is the disco-vm binary a fake guest runs, built once.
func agent(t *testing.T) string {
	t.Helper()
	if bin := os.Getenv("DISCO_VM_TEST_BINARY"); bin != "" {
		return bin
	}
	bin := filepath.Join(t.TempDir(), "disco-vm")
	if runtime.GOOS == "windows" {
		bin += ".exe"
	}
	build := exec.CommandContext(t.Context(), "go", "build", "-o", bin, "../../../cmd/disco-vm")
	build.Stdout, build.Stderr = os.Stdout, os.Stderr
	if err := build.Run(); err != nil {
		t.Fatal(err)
	}
	return bin
}

// fakeDriver is a fake local driver of pool in dir, whose shims are bin's.
func fakeDriver(t *testing.T, bin, dir, pool string) machine.Driver {
	t.Helper()
	driver, err := local.New(machine.Config{
		Pool:         pool,
		Dir:          dir,
		ShimCommand:  []string{bin, "--root", dir, "--pool", pool, "--driver", "fake", "shim"},
		DriverConfig: json.RawMessage("{}"),
	}, &fake.Hypervisor{Agent: bin, BaseName: "stock"})
	if err != nil {
		t.Fatal(err)
	}
	return driver
}

// serve serves driver under opts on a loopback listener, and returns a
// dialer of a new stream to it.
func serve(t *testing.T, driver machine.Driver, opts served.Options) func(context.Context) (net.Conn, error) {
	t.Helper()
	h, err := served.Handler(driver, opts)
	if err != nil {
		t.Fatal(err)
	}
	var lc net.ListenConfig
	l, err := lc.Listen(t.Context(), "tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	srv := &http.Server{Handler: h, ReadHeaderTimeout: 10 * time.Second}
	go func() { _ = srv.Serve(l) }()
	t.Cleanup(func() { _ = srv.Close() })
	return func(ctx context.Context) (net.Conn, error) {
		var d net.Dialer
		return d.DialContext(ctx, "tcp", l.Addr().String())
	}
}

func client(t *testing.T, dial func(context.Context) (net.Conn, error), version string) *served.Driver {
	t.Helper()
	d, err := served.New(t.Context(), served.Config{Dial: dial, Version: version})
	if err != nil {
		t.Fatal(err)
	}
	return d
}

// Everything allowed, the client passes the conformance suite against a
// fake served to it: every call it makes crosses the stream, and every
// machine it boots is the server's.
func TestConformance(t *testing.T) {
	bin := agent(t)
	dir := t.TempDir()
	dial := serve(t, fakeDriver(t, bin, dir, "served"), served.Options{
		Version: "v1", Allow: served.Ops, Fields: served.Fields,
	})
	machinetest.Run(t, client(t, dial, "v1"), machinetest.Config{
		Timeout:    30 * time.Second,
		ScratchDir: "/scratch",
		GuestAgent: bin,
	})
}

// A client of another version is refused, naming both, when it is made and
// on any call after.
func TestVersionMismatchIsRefused(t *testing.T) {
	dial := serve(t, fakeDriver(t, "/nonexistent", t.TempDir(), "p"), served.Options{
		Version: "v2", Allow: served.Ops,
	})
	_, err := served.New(t.Context(), served.Config{Dial: dial, Version: "v1"})
	ve, ok := errors.AsType[*served.VersionError](err)
	if !ok || !errors.Is(err, served.ErrVersion) || ve.Server != "v2" || ve.Client != "v1" {
		t.Fatalf("a v1 client of a v2 server: %v, want a version error naming both", err)
	}
	if !strings.Contains(err.Error(), "v1") || !strings.Contains(err.Error(), "v2") {
		t.Fatalf("%q names neither version", err)
	}
	// Any route, with no version or another.
	for _, version := range []string{"", "v1"} {
		if status := raw(t, dial, http.MethodGet, "/v1/vms", version, nil); status != http.StatusPreconditionFailed {
			t.Fatalf("list with version %q: %d, want refused", version, status)
		}
	}
}

// Every call the allow-list does not list is refused, and so is a create
// field it does not list, or that the protocol does not know; capabilities
// are narrowed to what is allowed.
func TestAllowListRefuses(t *testing.T) {
	ctx := t.Context()
	bin := agent(t)
	driver := fakeDriver(t, bin, t.TempDir(), "p")
	dial := serve(t, driver, served.Options{
		Version: "v1",
		Allow:   []served.Op{served.OpCapabilities, served.OpCreate, served.OpGet, served.OpList, served.OpStart, served.OpState, served.OpStop, served.OpRemove, served.OpInfo},
		Fields:  []served.Field{served.FieldCPUs},
	})
	c := client(t, dial, "v1")

	caps := c.Capabilities()
	if caps.Commit || caps.Ports || len(caps.Volumes) > 0 || len(caps.Install) > 0 || caps.Keys.Mode != machine.KeysNone {
		t.Fatalf("capabilities %+v, want commit, ports, volumes, install, and keys narrowed away", caps)
	}

	inst, err := c.Create(ctx, machine.InstanceSpec{Name: "allowed", CPUs: 1})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = driver.Destroy(context.Background(), inst) })

	for name, spec := range map[string]machine.InstanceSpec{
		"memory":  {Name: "refused", Memory: 1 << 30},
		"from":    {Name: "refused", From: &inst},
		"data":    {Name: "refused", Data: &machine.DataVolume{}},
		"forward": {Name: "refused", Forwards: []machine.Forward{{Port: 9, Instance: "allowed", InstancePort: guest.AgentPort}}},
	} {
		if _, err := c.Create(ctx, spec); !errors.Is(err, served.ErrRefused) {
			t.Fatalf("create with %s: %v, want ErrRefused", name, err)
		}
	}
	if status := raw(t, dial, http.MethodPost, "/v1/vms", "v1", []byte(`{"name":"refused","cache":{"key":"k"}}`)); status != http.StatusForbidden {
		t.Fatalf("create with a field the protocol does not know: %d, want refused", status)
	}
	if _, err := c.Instance(ctx, "refused"); !errors.Is(err, machine.ErrNotFound) {
		t.Fatalf("a refused create made %v", err)
	}

	if _, err := c.Commit(ctx, inst); !errors.Is(err, served.ErrRefused) {
		t.Fatalf("commit: %v, want ErrRefused", err)
	}
	if _, err := c.Upgrade(ctx, inst, nil); !errors.Is(err, served.ErrRefused) {
		t.Fatalf("upgrade: %v, want ErrRefused", err)
	}
	if _, err := c.Service(ctx, inst); !errors.Is(err, served.ErrRefused) {
		t.Fatalf("service: %v, want ErrRefused", err)
	}

	m, err := c.Start(ctx, inst, machine.BootOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := m.Info(ctx); err != nil {
		t.Fatalf("info, allowed: %v", err)
	}
	if _, err := m.Exec(ctx, machine.ExecRequest{Argv: []string{"true"}}); !errors.Is(err, served.ErrRefused) {
		t.Fatalf("exec: %v, want ErrRefused", err)
	}
	if err := m.CopyTo(ctx, "/scratch", bytes.NewReader(nil)); !errors.Is(err, served.ErrRefused) {
		t.Fatalf("copy: %v, want ErrRefused", err)
	}
	// The server refuses them itself, to a client that asks anyway.
	for _, req := range []string{
		"POST /v1/exec HTTP/1.1\r\nHost: guest\r\nContent-Length: 17\r\n\r\n{\"argv\":[\"true\"]}",
		"GET /v1/files?path=/etc HTTP/1.1\r\nHost: guest\r\n\r\n",
		"GET /v1/anything HTTP/1.1\r\nHost: guest\r\n\r\n",
	} {
		if status := rawGuest(t, dial, "allowed", req); status != http.StatusForbidden {
			t.Fatalf("%q through the guest's upgrade: %d, want refused", req, status)
		}
	}
	if _, err := m.Dial(ctx, guest.AgentPort); !errors.Is(err, served.ErrRefused) {
		t.Fatalf("dial: %v, want ErrRefused", err)
	}
	if _, err := c.Install(ctx, machine.InstallSpec{}, "x"); !errors.Is(err, machine.ErrUnsupported) {
		t.Fatalf("install: %v, want it never served", err)
	}
	if _, err := c.IssueKey(ctx, machine.IssueRequest{Name: "k"}); !errors.Is(err, machine.ErrNoKey) {
		t.Fatalf("issue a key: %v, want it never served", err)
	}
	if err := (&engine.Booted{Machine: m}).Shutdown(ctx, 30*time.Second); err != nil {
		t.Fatal(err)
	}
	if err := c.Destroy(ctx, inst); err != nil {
		t.Fatal(err)
	}
}

// The embedder's hook sees every create's spec, its From found in the
// pool, and changes it or refuses it.
func TestCreateHook(t *testing.T) {
	ctx := t.Context()
	driver := fakeDriver(t, "/nonexistent", t.TempDir(), "p")
	dial := serve(t, driver, served.Options{
		Version: "v1",
		Allow:   []served.Op{served.OpCapabilities, served.OpCreate, served.OpRemove},
		Fields:  []served.Field{served.FieldMemory},
		Create: func(_ context.Context, spec *machine.InstanceSpec) error {
			if spec.Memory > 1<<30 {
				return errors.New("over the provider's bounds")
			}
			spec.CPUs = 3
			return nil
		},
	})
	c := client(t, dial, "v1")
	if _, err := c.Create(ctx, machine.InstanceSpec{Name: "big", Memory: 2 << 30}); err == nil || !strings.Contains(err.Error(), "bounds") {
		t.Fatalf("a create the hook refuses: %v", err)
	}
	inst, err := c.Create(ctx, machine.InstanceSpec{Name: "small", Memory: 1 << 20})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = c.Destroy(context.Background(), inst) })
	if inst.CPUs != 3 {
		t.Fatalf("created %+v, want the hook's 3 CPUs", inst)
	}
}

// A client reaches only its pool's VMs: another pool's, in the same
// directory, are neither listed, found, nor removed, and an ID is never
// taken for a name.
func TestScope(t *testing.T) {
	ctx := t.Context()
	dir := t.TempDir()
	other := fakeDriver(t, "/nonexistent", dir, "other")
	theirs, err := other.Create(ctx, machine.InstanceSpec{Name: "theirs"})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = other.Destroy(context.Background(), theirs) })
	dial := serve(t, fakeDriver(t, "/nonexistent", dir, "mine"), served.Options{
		Version: "v1", Allow: served.Ops, Fields: served.Fields,
	})
	c := client(t, dial, "v1")
	if all, err := c.Instances(ctx); err != nil || len(all) != 0 {
		t.Fatalf("list: %v, %v, want none of another pool's", all, err)
	}
	for _, name := range []string{"theirs", theirs.ID, "../" + theirs.ID} {
		if _, err := c.Instance(ctx, name); !errors.Is(err, machine.ErrNotFound) {
			t.Fatalf("find %q: %v, want ErrNotFound", name, err)
		}
	}
	if err := c.Destroy(ctx, theirs); err != nil {
		t.Fatal(err)
	}
	if _, err := other.Instance(ctx, "theirs"); err != nil {
		t.Fatalf("another pool's VM, after the client removed it by its instance: %v", err)
	}
	if _, err := c.Create(ctx, machine.InstanceSpec{Name: "mine", From: &theirs}); !errors.Is(err, machine.ErrNotFound) {
		t.Fatalf("create from another pool's VM: %v, want ErrNotFound", err)
	}
}

// An embedder lists only operations and fields there are.
func TestHandlerRefusesWhatIsNot(t *testing.T) {
	driver := fakeDriver(t, "/nonexistent", t.TempDir(), "p")
	for _, opts := range []served.Options{
		{Allow: served.Ops},
		{Version: "v1", Allow: []served.Op{"install"}},
		{Version: "v1", Fields: []served.Field{"cache"}},
	} {
		if _, err := served.Handler(driver, opts); err == nil {
			t.Fatalf("handler of %+v made", opts)
		}
	}
}

// raw makes one request on a stream of its own, as a client of any
// version, and returns its status.
func raw(t *testing.T, dial func(context.Context) (net.Conn, error), method, path, version string, body []byte) int {
	t.Helper()
	hc := &http.Client{Transport: &http.Transport{
		DialContext:       func(ctx context.Context, _, _ string) (net.Conn, error) { return dial(ctx) },
		DisableKeepAlives: true,
	}}
	req, err := http.NewRequestWithContext(t.Context(), method, "http://served"+path, bytes.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	if version != "" {
		req.Header.Set(served.VersionHeader, version)
	}
	resp, err := hc.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	_, _ = io.Copy(io.Discard, resp.Body)
	_ = resp.Body.Close()
	return resp.StatusCode
}

// rawGuest upgrades a stream to the VM's guest operations, sends req in the
// agent's protocol, and returns its status.
func rawGuest(t *testing.T, dial func(context.Context) (net.Conn, error), name, req string) int {
	t.Helper()
	conn, err := dial(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	_ = conn.SetDeadline(time.Now().Add(30 * time.Second))
	if _, err := io.WriteString(conn, "GET /v1/vms/"+name+"/guest HTTP/1.1\r\nHost: served\r\n"+served.VersionHeader+": v1\r\nConnection: Upgrade\r\nUpgrade: disco-vm-guest\r\n\r\n"); err != nil {
		t.Fatal(err)
	}
	r := bufio.NewReader(conn)
	resp, err := http.ReadResponse(r, nil)
	if err != nil {
		t.Fatal(err)
	}
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusSwitchingProtocols {
		t.Fatalf("upgrade: %s", resp.Status)
	}
	if _, err := io.WriteString(conn, req); err != nil {
		t.Fatal(err)
	}
	inner, err := http.ReadResponse(r, nil)
	if err != nil {
		t.Fatal(err)
	}
	_ = inner.Body.Close()
	return inner.StatusCode
}

// A client allowed forwards is checked on the server as its engine would
// check it, and a forward to a host socket or one that names a pool is
// never served; the embedder's hook may set either, as discobox's server
// sets a sandbox's one forward to its pool machine.
func TestForwardsAreCheckedOnTheServer(t *testing.T) {
	ctx := t.Context()
	driver := fakeDriver(t, "/nonexistent", t.TempDir(), "p")
	hostSocket := filepath.Join(t.TempDir(), "host.sock")
	dial := serve(t, driver, served.Options{
		Version: "v1",
		Allow:   []served.Op{served.OpCapabilities, served.OpCreate, served.OpRemove},
		Fields:  []served.Field{served.FieldForwards},
		Create: func(_ context.Context, spec *machine.InstanceSpec) error {
			spec.Forwards = append(spec.Forwards,
				machine.Forward{Port: 9001, Socket: hostSocket},
				machine.Forward{Port: 9002, Pool: "provider", Instance: "pool-a", InstancePort: guest.ServicePort})
			return nil
		},
	})
	c := client(t, dial, "v1")
	for name, c2 := range map[string]struct {
		fwd  machine.Forward
		want error
	}{
		"a host socket":           {machine.Forward{Port: 9000, Socket: "/var/run/docker.sock"}, served.ErrRefused},
		"another pool's instance": {machine.Forward{Port: 9000, Pool: "provider", Instance: "pool-a", InstancePort: guest.ServicePort}, machine.ErrUnsupported},
		"its own pool, named":     {machine.Forward{Port: 9000, Pool: "p", Instance: "peer", InstancePort: guest.AgentPort}, machine.ErrUnsupported},
		"no target":               {machine.Forward{Port: 9000}, served.ErrRefused},
		"a peer's other port":     {machine.Forward{Port: 9000, Instance: "peer", InstancePort: 22}, machine.ErrUnsupported},
	} {
		if _, err := c.Create(ctx, machine.InstanceSpec{Name: "fwd", Forwards: []machine.Forward{c2.fwd}}); !errors.Is(err, c2.want) {
			t.Fatalf("a forward to %s: %v, want %v", name, err, c2.want)
		}
	}
	if _, err := c.Create(ctx, machine.InstanceSpec{Name: "fwd", Forwards: []machine.Forward{{Port: 9000, Instance: "fwd", InstancePort: guest.AgentPort}}}); err == nil || !strings.Contains(err.Error(), "itself") {
		t.Fatalf("a forward to itself: %v, want it refused", err)
	}
	inst, err := c.Create(ctx, machine.InstanceSpec{Name: "fwd", Forwards: []machine.Forward{{Port: 9000, Instance: "peer", InstancePort: guest.AgentPort}}})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = c.Destroy(context.Background(), inst) })
	if len(inst.Forwards) != 3 || inst.Forwards[1].Socket != hostSocket || inst.Forwards[2].Pool != "provider" {
		t.Fatalf("created %+v, want the peer forward, and the hook's socket and its forward to another pool", inst.Forwards)
	}
}

// A client's From of another pool is refused, even where the served pool has
// a VM of its name; the embedder's hook may set one, as discobox's server
// makes a darwin pool's sandboxes from its own base VM, and the client sees
// where it came from.
func TestOnlyTheHookReachesAnotherPool(t *testing.T) {
	ctx := t.Context()
	dir := t.TempDir()
	server := fakeDriver(t, "/nonexistent", dir, "server")
	b, err := server.Create(ctx, machine.InstanceSpec{Name: "base"})
	if err != nil {
		t.Fatal(err)
	}
	base, err := server.Commit(ctx, b)
	if err != nil {
		t.Fatal(err)
	}
	mine := fakeDriver(t, "/nonexistent", dir, "mine")
	if _, err := mine.Create(ctx, machine.InstanceSpec{Name: "base"}); err != nil {
		t.Fatal(err)
	}
	dial := serve(t, mine, served.Options{
		Version: "v1", Allow: served.Ops, Fields: served.Fields,
		Create: func(_ context.Context, spec *machine.InstanceSpec) error {
			if spec.From == nil {
				spec.From = &base
			}
			return nil
		},
	})
	c := client(t, dial, "v1")
	if caps, err := c.Capabilities(), c.Check(ctx); err != nil || caps.AcrossPools {
		t.Fatalf("a served driver reports AcrossPools %t: %v", caps.AcrossPools, err)
	}
	if _, err := c.Create(ctx, machine.InstanceSpec{Name: "copy", From: &base}); !errors.Is(err, machine.ErrUnsupported) {
		t.Fatalf("a client's From of another pool, its name the served pool's too: %v, want ErrUnsupported", err)
	}
	inst, err := c.Create(ctx, machine.InstanceSpec{Name: "sandbox"})
	if err != nil {
		t.Fatal(err)
	}
	if inst.Pool != "mine" || inst.From != "base" || inst.FromPool != "server" {
		t.Fatalf("a VM the hook made from another pool's: %+v", inst)
	}
	if all, err := c.Instances(ctx); err != nil || len(all) != 2 {
		t.Fatalf("the pool's VMs: %+v, %v; want its base and the sandbox alone", all, err)
	}
}
