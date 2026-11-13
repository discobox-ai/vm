package boxd

import (
	"context"
	"encoding/json"
	"errors"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/discobox-ai/vm/internal/fsutil"
	"github.com/discobox-ai/vm/pkg/machine"
)

// fakeAdapter is boxd's in-machine metadata adapter: each GET mints a new
// token for the machine's owner, which the fake API takes until it expires,
// as a member credential that cannot manage keys.
type fakeAdapter struct {
	*httptest.Server
	api *fakeAPI
	// lifetime is how long a token it mints lives.
	lifetime atomic.Int64
	// minted counts its tokens, and last is the newest.
	minted atomic.Int32
	mu     sync.Mutex
	last   string
}

func newFakeAdapter(t *testing.T, api *fakeAPI) *fakeAdapter {
	t.Helper()
	a := &fakeAdapter{api: api}
	a.lifetime.Store(int64(15 * time.Minute))
	a.Server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet || r.URL.Path != metadataPath {
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
			return
		}
		expires := time.Now().Add(time.Duration(a.lifetime.Load()))
		id, jwt := "vm-"+fsutil.RandomHex(4), "vmjwt_"+fsutil.RandomHex(8)
		api.mu.Lock()
		api.keys.byID[id] = &fakeKey{id: id, name: id, kind: "member", expires: expires}
		api.keys.jwts[jwt] = id
		api.mu.Unlock()
		a.minted.Add(1)
		a.mu.Lock()
		a.last = jwt
		a.mu.Unlock()
		// boxd's SDK reads expires_at as float seconds.
		_ = json.NewEncoder(w).Encode(map[string]any{
			"api_url": "http://" + api.addr, "token": jwt,
			"expires_at": float64(expires.UnixNano()) / float64(time.Second),
		})
	}))
	t.Cleanup(a.Close)
	return a
}

// fakeInMachine is a driver of the fake API with no key, on a machine whose
// metadata adapter is at url.
func fakeInMachine(t *testing.T, home *fakeAPI, url string) *Driver {
	t.Helper()
	d := fakeKeyed(t, home, machine.DefaultPool, nil)
	d.api.vm = newVMToken(machine.Config{})
	d.api.vm.locate = func() (string, error) { return url, nil }
	return d
}

// TestTheMachineTokenIsTheCredentialWithNoKey: a driver given no key on a
// boxd machine calls boxd with the token its metadata adapter mints, keeps
// it until a minute before it expires, mints another once boxd refuses it,
// and cannot manage keys with it.
func TestTheMachineTokenIsTheCredentialWithNoKey(t *testing.T) {
	_, fake := fakeDriver(t, t.TempDir())
	adapter := newFakeAdapter(t, fake)
	d := fakeInMachine(t, fake, adapter.URL+metadataPath)
	ctx := t.Context()

	if err := d.Check(ctx); err != nil {
		t.Fatalf("check on the machine's token: %v", err)
	}
	if _, err := d.Instances(ctx); err != nil {
		t.Fatal(err)
	}
	if n := adapter.minted.Load(); n != 1 {
		t.Fatalf("the adapter minted %d tokens for two calls, want 1 kept", n)
	}

	// boxd refuses the token kept: that call is ErrUnauthorized, and the
	// next mints another.
	adapter.mu.Lock()
	refused := adapter.last
	adapter.mu.Unlock()
	fake.mu.Lock()
	delete(fake.keys.jwts, refused)
	fake.mu.Unlock()
	if _, err := d.Instances(ctx); !errors.Is(err, machine.ErrUnauthorized) {
		t.Fatalf("a refused token: %v, want ErrUnauthorized", err)
	}
	if _, err := d.Instances(ctx); err != nil {
		t.Fatalf("after a refused token: %v", err)
	}
	if n := adapter.minted.Load(); n != 2 {
		t.Fatalf("the adapter minted %d tokens, want a second after the refusal", n)
	}

	// A token within a minute of its expiry is not used: every call mints.
	adapter.lifetime.Store(int64(30 * time.Second))
	d.api.vm.drop()
	before := adapter.minted.Load()
	if err := d.Check(ctx); err != nil {
		t.Fatal(err)
	}
	if err := d.Check(ctx); err != nil {
		t.Fatal(err)
	}
	if n := adapter.minted.Load() - before; n != 2 {
		t.Fatalf("the adapter minted %d tokens for two Whoamis near expiry, want 2", n)
	}

	if _, err := d.IssueKey(ctx, machine.IssueRequest{Name: "pool", TTL: time.Hour}); !errors.Is(err, machine.ErrCannotIssue) {
		t.Fatalf("issuing on the machine's token: %v, want ErrCannotIssue", err)
	}
}

// TestNoAdapterIsNoCredentials: with no key, a host with no default gateway,
// an adapter that does not answer (an isolated machine's), and one that
// answers no token or refuses are errors, the first two errNoCredentials.
func TestNoAdapterIsNoCredentials(t *testing.T) {
	_, fake := fakeDriver(t, t.TempDir())
	ctx := t.Context()

	noGateway := fakeKeyed(t, fake, machine.DefaultPool, nil)
	noGateway.api.vm = newVMToken(machine.Config{})
	noGateway.api.vm.locate = func() (string, error) { return "", errNoCredentials }
	if _, err := noGateway.Instances(ctx); !errors.Is(err, errNoCredentials) {
		t.Fatalf("no gateway: %v, want errNoCredentials", err)
	}

	listener, err := (&net.ListenConfig{}).Listen(ctx, "tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	closed := "http://" + listener.Addr().String() + metadataPath
	_ = listener.Close()
	silent := fakeInMachine(t, fake, closed)
	if _, err := silent.Instances(ctx); !errors.Is(err, errNoCredentials) || !strings.Contains(err.Error(), "did not answer") {
		t.Fatalf("an adapter that does not answer: %v, want errNoCredentials", err)
	}

	broken := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`{"token":"x"}`))
	}))
	t.Cleanup(broken.Close)
	if _, err := fakeInMachine(t, fake, broken.URL).Instances(ctx); err == nil || !strings.Contains(err.Error(), "no expiry") {
		t.Fatalf("an answer with no expiry: %v", err)
	}

	refusing := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		http.Error(w, "account disabled", http.StatusForbidden)
	}))
	t.Cleanup(refusing.Close)
	if _, err := fakeInMachine(t, fake, refusing.URL).Instances(ctx); !errors.Is(err, machine.ErrUnauthorized) {
		t.Fatalf("an adapter that refuses: %v, want ErrUnauthorized", err)
	}
}

// TestInMachineIsTheConfigurations: only a driver whose configuration sets
// inMachine, and that is given no key, reads the adapter; with it unset, no
// key is no credentials, as before.
func TestInMachineIsTheConfigurations(t *testing.T) {
	key := machine.StaticKey(machine.Key{Driver: "boxd", Secret: "bxd_x"})
	for _, c := range []struct {
		config string
		keys   machine.KeySource
		reads  bool
	}{
		{`{"inMachine":true}`, nil, true},
		{`{"inMachine":true}`, key, false},
		{`{}`, nil, false},
		{`{"inMachine":false}`, key, false},
	} {
		d, err := New(machine.Config{DriverConfig: json.RawMessage(c.config), Keys: c.keys})
		if err != nil {
			t.Fatal(err)
		}
		if got := d.api.vm != nil; got != c.reads {
			t.Errorf("%s, key %v: reads the adapter %v, want %v", c.config, c.keys != nil, got, c.reads)
		}
	}
	if err := Validate(json.RawMessage(`{"inMachine":"yes"}`)); err == nil {
		t.Error("an inMachine that is no boolean was accepted")
	}
	d, err := New(machine.Config{})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := d.api.token(t.Context()); !errors.Is(err, errNoCredentials) {
		t.Fatalf("no key and no inMachine: %v, want errNoCredentials", err)
	}
}

// TestTheAdapterIsDialedDirectly: the adapter is asked through the
// embedder's dialer, and never a proxy, with a dialer or without, since it
// knows the machine by where its request comes from.
func TestTheAdapterIsDialedDirectly(t *testing.T) {
	var d net.Dialer
	for _, cfg := range []machine.Config{{}, {Dial: d.DialContext}} {
		transport, ok := newVMToken(cfg).http.Transport.(*http.Transport)
		if !ok || transport.Proxy != nil {
			t.Fatalf("dial set %v: the adapter's transport has a proxy", cfg.Dial != nil)
		}
	}

	_, fake := fakeDriver(t, t.TempDir())
	adapter := newFakeAdapter(t, fake)
	var dialed atomic.Value
	v := newVMToken(machine.Config{Dial: func(ctx context.Context, network, addr string) (net.Conn, error) {
		dialed.Store(addr)
		var d net.Dialer
		return d.DialContext(ctx, network, addr)
	}})
	v.locate = func() (string, error) { return adapter.URL + metadataPath, nil }
	if _, err := v.get(t.Context()); err != nil {
		t.Fatal(err)
	}
	if got := dialed.Load(); got != adapter.Listener.Addr().String() {
		t.Fatalf("dialed %v, want the adapter at %s", got, adapter.Listener.Addr())
	}
}

// TestDefaultGateway: the adapter is on the default route's gateway, read
// from the route table as boxd's CLI and SDK read it.
func TestDefaultGateway(t *testing.T) {
	table := "Iface\tDestination\tGateway \tFlags\tRefCnt\tUse\tMetric\tMask\t\tMTU\tWindow\tIRTT\n" +
		"eth0\t001F0A0A\t00000000\t0001\t0\t0\t0\t00FFFFFF\t0\t0\t0\n" +
		"eth0\t00000000\t01001F0A\t0003\t0\t0\t0\t00000000\t0\t0\t0\n"
	if gw, ok := defaultGateway(table); !ok || gw != "10.31.0.1" {
		t.Fatalf("defaultGateway = %q, %v, want 10.31.0.1", gw, ok)
	}
	if _, ok := defaultGateway(table[:strings.LastIndex(table[:len(table)-1], "\n")+1]); ok {
		t.Fatal("a table with no default route has a gateway")
	}
	if _, ok := defaultGateway(""); ok {
		t.Fatal("an empty table has a gateway")
	}
	pointToPoint := "Iface\tDestination\tGateway \tFlags\n" +
		"tun0\t00000000\t00000000\t0001\n" +
		"eth0\t00000000\t01001F0A\t0003\n"
	if gw, ok := defaultGateway(pointToPoint); !ok || gw != "10.31.0.1" {
		t.Fatalf("a default route with no gateway before one with: %q, %v, want 10.31.0.1", gw, ok)
	}
}
