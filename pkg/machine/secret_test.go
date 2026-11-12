package machine_test

import (
	"context"
	"encoding/json"
	"errors"
	"net"
	"net/http"
	"reflect"
	"slices"
	"strings"
	"testing"

	"github.com/discobox-ai/vm/pkg/machine"
	_ "github.com/discobox-ai/vm/pkg/machine/drivers"
)

// TestSecrets holds the secret each driver takes, the variables it is read
// from, and whether it takes a base (ADR 26-11-12-461): a driver that adds a
// secret, or sends one, is in this list or argued out of it.
func TestSecrets(t *testing.T) {
	want := map[string]struct {
		secret string
		env    []string
		sent   bool
		base   bool
	}{
		"archil":  {"apiKey", []string{"ARCHIL_API_KEY"}, true, true},
		"boxd":    {"apiKey", []string{"BOXD_API_KEY", "BOXD_TOKEN"}, true, true},
		"docker":  {base: true},
		"fake":    {"key", []string{"DISCO_VM_FAKE_KEY"}, false, true},
		"hcs":     {},
		"libkrun": {base: true},
		"vz":      {base: true},
	}
	for _, name := range machine.Names() {
		if name == testDriver {
			continue
		}
		def, err := machine.DefinitionOf(name)
		if err != nil {
			t.Fatal(err)
		}
		w, ok := want[name]
		if !ok {
			t.Errorf("%s is not in the list", name)
			continue
		}
		if def.TakesBase() != w.base {
			t.Errorf("%s takes a base: %v, want %v", name, def.TakesBase(), w.base)
		}
		if w.secret == "" {
			if len(def.Secrets) != 0 {
				t.Errorf("%s takes secrets %v, want none", name, def.Secrets)
			}
			continue
		}
		s, ok := def.Secret(w.secret)
		if !ok || len(def.Secrets) != 1 || !slices.Equal(s.Env, w.env) || (len(s.Sends) > 0) != w.sent {
			t.Errorf("%s's secrets are %+v, want %s from %v, sent: %v", name, def.Secrets, w.secret, w.env, w.sent)
		}
		if s.Label == "" || s.Description == "" {
			t.Errorf("%s: secret %s has no label or description", name, s.Key)
		}
	}
}

// TestSecretSends resolves where boxd and archil send their keys under a
// configuration: its endpoint's value, else its default, else every host
// the definition lists, each path below the endpoint URL's own.
func TestSecretSends(t *testing.T) {
	grpcBearer := func(host string) machine.SecretSend {
		return machine.SecretSend{Endpoint: "addr", Hosts: []string{host}, Listed: true, Protocol: machine.SendGRPC, Method: "POST",
			Path: "/boxd.api.v1.BoxdApi/", Header: "authorization", ValuePrefix: "Bearer "}
	}
	exchange := func(host, path string) machine.SecretSend {
		return machine.SecretSend{Prefix: "bxd_", Endpoint: "tokenUrl", Hosts: []string{host}, Protocol: machine.SendHTTPS, Method: "POST",
			Path: path, JSONField: "api_key"}
	}
	archil := func(path string, hosts ...string) machine.SecretSend {
		return machine.SecretSend{Endpoint: "baseUrl", Hosts: hosts, Protocol: machine.SendHTTPS, Path: path,
			Header: "Authorization", ValuePrefix: "Bearer key-", TrimPrefix: "key-"}
	}
	for _, c := range []struct {
		driver, config string
		want           map[string][]machine.SecretSend
	}{
		{"boxd", ``, map[string][]machine.SecretSend{"apiKey": {
			exchange("app.boxd.sh:443", "/api/v1/auth/token"), grpcBearer("boxd.sh:9443")}}},
		{"boxd", `{"addr":"boxd.example:8443","tokenUrl":"http://127.0.0.1:9/t","region":"nyc"}`, map[string][]machine.SecretSend{"apiKey": {
			exchange("127.0.0.1:9", "/t"), grpcBearer("boxd.example:8443")}}},
		{"archil", ``, map[string][]machine.SecretSend{"apiKey": {archil("/api/",
			"control.green.eu-west-1.aws.prod.archil.com:443", "control.green.us-east-1.aws.prod.archil.com:443",
			"control.green.us-west-2.aws.prod.archil.com:443")}}},
		{"archil", `{"baseUrl":"https://archil.example:8443/v2/"}`, map[string][]machine.SecretSend{"apiKey": {
			archil("/v2/api/", "archil.example:8443")}}},
		{"fake", ``, map[string][]machine.SecretSend{"key": {}}},
		{"docker", ``, map[string][]machine.SecretSend{}},
	} {
		got, err := machine.SecretSends(c.driver, json.RawMessage(c.config))
		if err != nil {
			t.Errorf("%s %s: %v", c.driver, c.config, err)
			continue
		}
		if !reflect.DeepEqual(got, c.want) {
			t.Errorf("%s %s sends\n%+v\nwant\n%+v", c.driver, c.config, got, c.want)
		}
	}
	if _, err := machine.SecretSends("boxd", json.RawMessage(`{"tokenUrl":"http://example.com/t"}`)); err == nil {
		t.Error("sends resolved under a configuration New refuses")
	}
}

// TestASecretIsNeverConfiguration: a secret's key in a configuration, or
// given as a field's value, is refused by name, and no error carries it.
func TestASecretIsNeverConfiguration(t *testing.T) {
	const secret = "bxd_written-never-read" //nolint:gosec // G101: a stand-in a test sends, not a credential
	for _, c := range []struct{ driver, config, want string }{
		{"boxd", `{"apiKey":"` + secret + `"}`, "apiKey is a secret"},
		{"archil", `{"apiKey":"` + secret + `"}`, "apiKey is a secret"},
		{"fake", `{"key":"` + secret + `"}`, "key is a secret"},
	} {
		err := machine.Validate(c.driver, json.RawMessage(c.config))
		if err == nil || !strings.Contains(err.Error(), c.want) || strings.Contains(err.Error(), secret) {
			t.Errorf("%s %s: %v, want %q and not the secret", c.driver, c.config, err, c.want)
		}
	}
	if _, err := machine.ConfigFromValues("boxd", map[string]string{"apiKey": secret}); err == nil || strings.Contains(err.Error(), secret) {
		t.Errorf("a secret given as a value: %v", err)
	}
}

// TestAnEmbeddersDialerReadsNoProxy: with Config.Dial set, an HTTP service
// is dialed through it at its own address, whatever HTTPS_PROXY says; with
// none, the process's proxy variables stand.
func TestAnEmbeddersDialerReadsNoProxy(t *testing.T) {
	t.Setenv("HTTPS_PROXY", "http://proxy.example:3128")
	req, err := http.NewRequestWithContext(t.Context(), http.MethodGet, "https://control.example/api/", nil)
	if err != nil {
		t.Fatal(err)
	}
	if p := (machine.Config{}).HTTPTransport().Proxy; p == nil {
		t.Fatal("with no dialer, HTTPS_PROXY is not read")
	}
	var dialed string
	tr := machine.Config{Dial: func(_ context.Context, _, addr string) (net.Conn, error) {
		dialed = addr
		return nil, errors.New("refused")
	}}.HTTPTransport()
	if tr.Proxy != nil {
		if u, _ := tr.Proxy(req); u != nil {
			t.Fatalf("with a dialer, the request is proxied to %s", u)
		}
	}
	resp, err := (&http.Client{Transport: tr}).Do(req)
	if err == nil {
		_ = resp.Body.Close()
	}
	if err == nil || dialed != "control.example:443" {
		t.Fatalf("dialed %q (%v), want the service's own address", dialed, err)
	}
}
