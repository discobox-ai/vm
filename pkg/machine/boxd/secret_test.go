package boxd

import (
	"bytes"
	"context"
	"crypto/x509"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"testing"

	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"

	"github.com/discobox-ai/vm/pkg/machine"
	"github.com/discobox-ai/vm/pkg/machine/boxd/internal/boxdapi"
)

func TestServiceName(t *testing.T) {
	if serviceName != boxdapi.BoxdApi_ServiceDesc.ServiceName {
		t.Fatalf("serviceName %q, boxd's %q", serviceName, boxdapi.BoxdApi_ServiceDesc.ServiceName)
	}
}

// exchange is one request to the token exchange, as it arrived.
type exchange struct {
	method, host, path, apiKey string
}

// TestTheKeyGoesWhereTheDefinitionSays drives boxd, in a region, with an API
// key and with a token, through an embedder's dialer, and checks the key
// reached only what machine.SecretSends says for its form: an API key the
// token exchange alone, in its JSON body, by POST at tokenUrl's path; a
// token every call's bearer, at the home endpoint and the region's it lists.
// Every connection went through the dialer.
func TestTheKeyGoesWhereTheDefinitionSays(t *testing.T) {
	const apiKey = "bxd_the-real-key" //nolint:gosec // G101: a stand-in a test sends, not a credential
	home := newFakeAPI(t.TempDir())
	home.region = "fra"
	serveFake(t, home)
	var (
		mu        sync.Mutex
		exchanges []exchange
	)
	tokens := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		var in struct {
			APIKey string `json:"api_key"`
		}
		_ = json.Unmarshal(body, &in)
		mu.Lock()
		exchanges = append(exchanges, exchange{r.Method, r.Host, r.URL.Path, in.APIKey})
		mu.Unlock()
		r.Body = io.NopCloser(bytes.NewReader(body))
		home.serveTokens(w, r)
	}))
	t.Cleanup(tokens.Close)
	home.tokenURL = tokens.URL + "/api/v1/auth/token"
	home.regions = []*boxdapi.Region{{Id: "fra", ApiEndpoint: home.addr, Home: true, OrgState: "enabled"}}
	region := fakeRegion(t, home, t.TempDir(), "nyc", "enabled")
	home.mu.Lock()
	home.keys.byID["key_ci"] = &fakeKey{id: "key_ci", name: "ci", secret: apiKey, kind: "member"}
	home.mu.Unlock()

	config := json.RawMessage(fmt.Sprintf(`{"addr":%q,"tokenUrl":%q,"region":"nyc"}`, home.addr, home.tokenURL))
	sends, err := machine.SecretSends("boxd", config)
	if err != nil {
		t.Fatal(err)
	}
	for _, key := range []string{apiKey, fakeSession} {
		t.Run(strings.SplitN(key, "-", 2)[0], func(t *testing.T) {
			mu.Lock()
			exchanges = nil
			mu.Unlock()
			var dialed []string
			dial := func(ctx context.Context, network, addr string) (net.Conn, error) {
				mu.Lock()
				dialed = append(dialed, addr)
				mu.Unlock()
				var d net.Dialer
				return d.DialContext(ctx, network, addr)
			}
			d := configured(t, config, machine.StaticKey(machine.Key{Driver: "boxd", Secret: key}), dial)
			if err := d.Check(t.Context()); err != nil {
				t.Fatal(err)
			}
			send := sendOf(sends["apiKey"], key)
			mu.Lock()
			defer mu.Unlock()
			var hosts []string
			switch {
			case send.JSONField == "api_key":
				// An API key: the exchange alone, and only its JWT is a bearer.
				want := exchange{send.Method, send.Hosts[0], send.Path, key}
				if len(exchanges) != 1 || exchanges[0] != want {
					t.Fatalf("the API key reached the exchange as %+v, want once as %+v", exchanges, want)
				}
				hosts = []string{send.Hosts[0], home.addr, region.addr}
			case send.Header == "authorization" && send.ValuePrefix == "Bearer ":
				// A token: the fake refuses any call without it as the bearer.
				if len(exchanges) != 0 {
					t.Fatalf("a token was exchanged: %+v", exchanges)
				}
				if !send.Listed || send.Hosts[0] != home.addr || send.Path != "/"+serviceName+"/" {
					t.Fatalf("a token's send is %+v", send)
				}
				hosts = []string{home.addr, region.addr}
			default:
				t.Fatalf("%s's send is %+v", key, send)
			}
			slices.Sort(dialed)
			slices.Sort(hosts)
			if !slices.Equal(slices.Compact(dialed), hosts) {
				t.Fatalf("dialed %v, want %v", dialed, hosts)
			}
		})
	}
}

// TestTheDialerCanRefuse: an embedder's dialer that refuses a host refuses
// the driver's call there, the token exchange as well as the API.
func TestTheDialerCanRefuse(t *testing.T) {
	refused := errors.New("not a host this embedder lets the driver reach")
	dial := func(context.Context, string, string) (net.Conn, error) { return nil, refused }
	for _, key := range []string{"bxd_k", fakeSession} {
		d := configured(t, json.RawMessage(`{"addr":"127.0.0.1:9","tokenUrl":"http://127.0.0.1:9/t"}`),
			machine.StaticKey(machine.Key{Driver: "boxd", Secret: key}), dial)
		if err := d.Check(t.Context()); err == nil || !strings.Contains(err.Error(), refused.Error()) {
			t.Fatalf("%s: Check through a refusing dialer: %v", key, err)
		}
	}
}

// configured is a driver of the configuration, as New makes it, dialing in
// plain text, since the fake serves no TLS.
func configured(t *testing.T, config json.RawMessage, keys machine.KeySource, dial machine.DialFunc) *Driver {
	t.Helper()
	d, err := New(machine.Config{DriverConfig: config, Keys: keys, Dial: dial})
	if err != nil {
		t.Fatal(err)
	}
	d.api.creds = insecure.NewCredentials()
	closeConns(t, d)
	return d
}

// closeConns closes the driver's gRPC connections when the test ends.
func closeConns(t *testing.T, d *Driver) {
	t.Helper()
	t.Cleanup(func() {
		d.api.dialMu.Lock()
		defer d.api.dialMu.Unlock()
		for _, conn := range d.api.conns {
			_ = conn.Close()
		}
	})
}

// TestTheEmbeddersRootsAreTrusted serves the token exchange and the API, at
// the home endpoint and a region's, over TLS with a certificate from a
// private CA, and drives boxd there with an API key and with a token: with
// the CA in machine.Config.RootCAs every call succeeds, and with none the
// system's roots refuse the exchange (an API key) or the gRPC API (a token).
func TestTheEmbeddersRootsAreTrusted(t *testing.T) {
	const apiKey = "bxd_the-real-key" //nolint:gosec // G101: a stand-in a test sends, not a credential
	home := newFakeAPI(t.TempDir())
	home.region = "fra"
	homeTLS := serveFakeTLS(t, home)
	tokens := httptest.NewTLSServer(http.HandlerFunc(home.serveTokens))
	t.Cleanup(tokens.Close)
	home.tokenURL = tokens.URL + "/api/v1/auth/token"
	region := newFakeAPI(t.TempDir())
	region.region, region.account, region.tokenURL = "nyc", home, home.tokenURL
	regionTLS := serveFakeTLS(t, region)
	home.regions = []*boxdapi.Region{
		{Id: "fra", ApiEndpoint: home.addr, Home: true, OrgState: "enabled"},
		{Id: "nyc", ApiEndpoint: "https://" + region.addr, OrgState: "enabled"},
	}
	home.mu.Lock()
	home.keys.byID["key_ci"] = &fakeKey{id: "key_ci", name: "ci", secret: apiKey, kind: "member"}
	home.mu.Unlock()
	roots := x509.NewCertPool()
	for _, srv := range []*httptest.Server{homeTLS, tokens, regionTLS} {
		roots.AddCert(srv.Certificate())
	}

	config := json.RawMessage(fmt.Sprintf(`{"addr":%q,"tokenUrl":%q,"region":"nyc"}`, home.addr, home.tokenURL))
	for _, key := range []string{apiKey, fakeSession} {
		for _, trusted := range []*x509.CertPool{roots, nil} {
			d, err := New(machine.Config{DriverConfig: config, Keys: machine.StaticKey(machine.Key{Driver: "boxd", Secret: key}), RootCAs: trusted})
			if err != nil {
				t.Fatal(err)
			}
			closeConns(t, d)
			err = d.Check(t.Context())
			switch {
			case trusted != nil && err != nil:
				t.Fatalf("%s with the CA in RootCAs: %v", key, err)
			case trusted == nil && (err == nil || !strings.Contains(err.Error(), "tls: failed to verify certificate")):
				t.Fatalf("%s with no RootCAs: %v, want the system's roots to refuse the private CA", key, err)
			}
		}
	}
	if regionTLS.Config.Handler.(*countingHandler).calls.Load() == 0 {
		t.Fatal("no call reached the region's endpoint")
	}
}

// serveFakeTLS serves fake's API over TLS on loopback, at fake.addr, with
// httptest's certificate, which the returned server's Certificate issued.
func serveFakeTLS(t *testing.T, fake *fakeAPI) *httptest.Server {
	t.Helper()
	server := grpc.NewServer(grpc.UnaryInterceptor(fake.unaryAuth), grpc.StreamInterceptor(fake.streamAuth))
	boxdapi.RegisterBoxdApiServer(server, fake)
	srv := httptest.NewUnstartedServer(&countingHandler{next: server})
	srv.EnableHTTP2 = true
	srv.StartTLS()
	t.Cleanup(func() { server.Stop(); srv.Close() })
	fake.addr = srv.Listener.Addr().String()
	return srv
}

// countingHandler counts the requests it passes on.
type countingHandler struct {
	next  http.Handler
	calls atomic.Int64
}

func (h *countingHandler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	h.calls.Add(1)
	h.next.ServeHTTP(w, r)
}

// sendOf is the send of a secret's form: the one whose Prefix it starts
// with, or else the one with none.
func sendOf(sends []machine.SecretSend, secret string) machine.SecretSend {
	for _, s := range sends {
		if s.Prefix != "" && strings.HasPrefix(secret, s.Prefix) {
			return s
		}
	}
	for _, s := range sends {
		if s.Prefix == "" {
			return s
		}
	}
	return machine.SecretSend{}
}
