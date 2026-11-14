package archil

import (
	"context"
	"crypto/x509"
	"encoding/json"
	"errors"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"github.com/discobox-ai/vm/pkg/machine"
)

// TestTheKeyGoesWhereTheDefinitionSays calls Archil at a baseUrl through an
// embedder's dialer, and checks the key arrived only as machine.SecretSends
// says: at its host, below its path, in its header as its prefix and the key
// without its trimmed prefix; and that the dialer made every connection.
func TestTheKeyGoesWhereTheDefinitionSays(t *testing.T) {
	var (
		mu    sync.Mutex
		calls []string
	)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		calls = append(calls, r.Host+" "+r.URL.Path+" "+r.Header.Get("Authorization"))
		mu.Unlock()
		_, _ = w.Write([]byte(`{"sandboxes":[]}`))
	}))
	t.Cleanup(srv.Close)
	config := json.RawMessage(`{"baseUrl":"` + srv.URL + `/v2"}`)
	sends, err := machine.SecretSends("archil", config)
	if err != nil {
		t.Fatal(err)
	}
	send := sends["apiKey"][0]
	var dialed []string
	dial := func(ctx context.Context, network, addr string) (net.Conn, error) {
		mu.Lock()
		dialed = append(dialed, addr)
		mu.Unlock()
		var d net.Dialer
		return d.DialContext(ctx, network, addr)
	}
	for _, key := range []string{"key-the-real-key", "the-real-key"} {
		mu.Lock()
		calls, dialed = nil, nil
		mu.Unlock()
		d, err := New(machine.Config{DriverConfig: config, Keys: machine.StaticKey(machine.Key{Secret: key}), Dial: dial})
		if err != nil {
			t.Fatal(err)
		}
		if err := d.Check(t.Context()); err != nil {
			t.Fatal(err)
		}
		mu.Lock()
		want := send.Hosts[0] + " " + send.Path + "sandboxes " + send.ValuePrefix + strings.TrimPrefix(key, send.TrimPrefix)
		if len(calls) != 1 || calls[0] != want || !strings.HasSuffix(calls[0], "the-real-key") {
			t.Errorf("%s: calls %q, want %q", key, calls, want)
		}
		if len(dialed) != 1 || dialed[0] != send.Hosts[0] {
			t.Errorf("%s: dialed %v, want %s", key, dialed, send.Hosts[0])
		}
		mu.Unlock()
	}
}

// TestTheDialerCanRefuse: an embedder's dialer that refuses the control
// plane refuses the driver's call there.
func TestTheDialerCanRefuse(t *testing.T) {
	refused := errors.New("not a host this embedder lets the driver reach")
	d, err := New(machine.Config{Keys: machine.StaticKey(machine.Key{Secret: "k"}),
		Dial: func(context.Context, string, string) (net.Conn, error) { return nil, refused }})
	if err != nil {
		t.Fatal(err)
	}
	if err := d.Check(t.Context()); err == nil || !strings.Contains(err.Error(), refused.Error()) {
		t.Fatalf("Check through a refusing dialer: %v", err)
	}
}

// TestTheEmbeddersRootsAreTrusted serves Archil's API and process API over
// TLS with a certificate from a private CA: with the CA in
// machine.Config.RootCAs, a command runs through both; with none, the
// system's roots refuse the API.
func TestTheEmbeddersRootsAreTrusted(t *testing.T) {
	srv := fakeArchilServer(t, httptest.NewTLSServer)
	roots := x509.NewCertPool()
	roots.AddCert(srv.Certificate())
	config := json.RawMessage(`{"baseUrl":"` + srv.URL + `"}`)
	run := func(trusted *x509.CertPool) error {
		d, err := New(machine.Config{DriverConfig: config, Keys: machine.StaticKey(machine.Key{Secret: "secret"}), RootCAs: trusted})
		if err != nil {
			t.Fatal(err)
		}
		code, out, _, err := d.api.run(t.Context(), "sb1", "echo", "", strings.NewReader("hello"))
		if err == nil && (code != 3 || string(out) != "hello!") {
			t.Fatalf("run = %d %q, want 3 hello!", code, out)
		}
		return err
	}
	if err := run(roots); err != nil {
		t.Fatalf("with the CA in RootCAs: %v", err)
	}
	if err := run(nil); err == nil || !strings.Contains(err.Error(), "tls: failed to verify certificate") {
		t.Fatalf("with no RootCAs: %v, want the system's roots to refuse the private CA", err)
	}
}
