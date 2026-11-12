package machine_test

import (
	"encoding/json"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"

	"github.com/discobox-ai/vm/pkg/machine"
	_ "github.com/discobox-ai/vm/pkg/machine/drivers"
)

// TestEveryDriverDecodesItsDefinition sets every field each driver in this
// build declares, as its type's zero value, which every driver reads as its
// default, and then as its Default: a field the definition lists and the
// driver's own Config does not decode, of another type, or whose default
// the driver refuses, fails here.
func TestEveryDriverDecodesItsDefinition(t *testing.T) {
	for _, name := range machine.Names() {
		if name == testDriver {
			continue
		}
		def, err := machine.DefinitionOf(name)
		if err != nil {
			t.Fatal(err)
		}
		if def.Description == "" || def.TakesBase() && (def.Base.Description == "" || def.Base.Env != machine.BaseEnv) {
			t.Errorf("%s has no description, or its base none", name)
		}
		zero, defaults := map[string]any{}, map[string]any{}
		for _, f := range def.ConfigFields {
			if f.Label == "" || f.Description == "" {
				t.Errorf("%s: %s has no label or description", name, f.Key)
			}
			zero[f.Key] = map[string]any{machine.ConfigString: "", machine.ConfigBoolean: false, machine.ConfigNumber: 0}[f.Type]
			if f.Default != "" {
				if defaults[f.Key], err = f.Parse(f.Default); err != nil {
					t.Errorf("%s: %s's default is not a %s", name, f.Key, f.Type)
				}
			}
		}
		for what, config := range map[string]map[string]any{"its zero value": zero, "its default": defaults} {
			raw, err := json.Marshal(config)
			if err != nil {
				t.Fatal(err)
			}
			if err := machine.Validate(name, raw); err != nil {
				t.Errorf("%s: every field at %s: %v", name, what, err)
			}
		}
		if err := machine.Validate(name, nil); err != nil {
			t.Errorf("%s: no configuration: %v", name, err)
		}
	}
}

// TestPrivileged holds the fields, and the bases, that name a host resource
// or where a key or a guest's data is sent (ADR 26-11-11-215 §7): a field
// added to a driver is in this list or argued out of it.
func TestPrivileged(t *testing.T) {
	want := map[string][]string{
		"archil":  {"baseUrl"},
		"boxd":    {"addr", "tokenUrl"},
		"docker":  {"agent", "host", "root"},
		"fake":    {"agent"},
		"hcs":     nil,
		"libkrun": {"agent", "base", "runtime"},
		"vz":      {"agent", "base"},
	}
	for _, name := range machine.Names() {
		if name == testDriver {
			continue
		}
		def, err := machine.DefinitionOf(name)
		if err != nil {
			t.Fatal(err)
		}
		var got []string
		if def.TakesBase() && def.Base.Privileged {
			got = append(got, "base")
		}
		for _, f := range def.ConfigFields {
			if f.Privileged {
				got = append(got, f.Key)
			}
		}
		slices.Sort(got)
		if w, ok := want[name]; !ok || !slices.Equal(got, w) {
			t.Errorf("%s's privileged fields are %v, want %v", name, got, w)
		}
	}
}

func TestValidateRefusesWhatTheDriverDoesNotTake(t *testing.T) {
	for _, c := range []struct {
		driver, config, want string
	}{
		{"fake", `{"nope":1}`, `unknown field "nope" (the driver takes agent, noCommit, maxRunning)`},
		{"fake", `{"noCommit":"yes"}`, "noCommit: not a bool"},
		{"fake", `{"maxRunning":-1}`, "maxRunning: not a count"},
		{"fake", `{"agent":"disco-vm"}`, "agent: not an absolute path"},
		{"fake", `[]`, "configuration"},
		{"fake", `{} {}`, "not one JSON object"},
		{"docker", `{"host":"ssh://h"}`, "host: only unix:// and tcp://"},
		{"docker", `{"root":"relative"}`, "root: not an absolute path"},
		{"boxd", `{"addr":"no-port"}`, "addr: not HOST:PORT"},
		{"boxd", `{"tokenUrl":"http://example.com/t"}`, "tokenUrl: a URL the key is sent to is https"},
		{"boxd", `{"region":"New York"}`, "region: not a boxd region ID"},
		{"archil", `{"region":"mars"}`, "region: not one that runs sandboxes"},
		{"archil", `{"baseUrl":"https://h/?k=v"}`, "baseUrl: a URL the key is sent to has no userinfo, query"},
		{"nope", `{}`, `no driver "nope"`},
	} {
		err := machine.Validate(c.driver, json.RawMessage(c.config))
		if err == nil || !strings.Contains(err.Error(), c.want) {
			t.Errorf("%s %s: %v, want %q", c.driver, c.config, err, c.want)
		}
	}
	// New validates before it constructs.
	if _, err := machine.New("docker", machine.Config{DriverConfig: json.RawMessage(`{"host":"ssh://h"}`)}); err == nil {
		t.Error("New made a docker driver of ssh://")
	}
	// A base URL stands in for a region's control plane.
	if err := machine.Validate("archil", json.RawMessage(`{"region":"anywhere","baseUrl":"http://127.0.0.1:1"}`)); err != nil {
		t.Error(err)
	}
}

// TestNoErrorQuotesAValue gives each driver values it refuses, and finds
// none of them in the error (ADR 26-11-11-215 §4): a value may be a file a
// person pointed at by mistake, or a URL with a password in it.
func TestNoErrorQuotesAValue(t *testing.T) {
	const secret = "s3cr3tvalue"
	for _, c := range []struct{ driver, config string }{
		{"docker", `{"host":"s3cr3tvalue"}`},
		{"docker", `{"root":"s3cr3tvalue"}`},
		{"docker", `{"agent":"s3cr3tvalue"}`},
		{"archil", `{"region":"s3cr3tvalue"}`},
		{"archil", `{"baseUrl":"https://u:s3cr3tvalue@h"}`},
		{"archil", `{"baseUrl":"http://s3cr3tvalue.example"}`},
		{"boxd", `{"addr":"s3cr3tvalue"}`},
		{"boxd", `{"tokenUrl":"ftp://s3cr3tvalue"}`},
		{"fake", `{"noCommit":"s3cr3tvalue"}`},
		{"fake", `{"agent": s3cr3tvalue}`},
	} {
		err := machine.Validate(c.driver, json.RawMessage(c.config))
		if err == nil || strings.Contains(err.Error(), secret) {
			t.Errorf("%s %s: %v", c.driver, c.config, err)
		}
	}
	if _, err := machine.ConfigFromValues("fake", map[string]string{"noCommit": secret}); err == nil || strings.Contains(err.Error(), secret) {
		t.Errorf("a flag's value: %v", err)
	}
}

func TestCheckKeyURL(t *testing.T) {
	for raw, ok := range map[string]bool{
		"https://app.boxd.sh/api/v1/auth/token": true,
		"http://127.0.0.1:8080/t":               true,
		"http://[::1]:8080/t":                   true,
		"http://localhost/t":                    true,
		"http://example.com/t":                  false,
		"https://user:pass@example.com/t":       false,
		"https://example.com/t?token=x":         false,
		"https://example.com/t#x":               false,
		"ftp://example.com/t":                   false,
		"example.com":                           false,
	} {
		if err := machine.CheckKeyURL("d", "f", raw); (err == nil) != ok {
			t.Errorf("%s: %v, want ok %v", raw, err, ok)
		}
	}
}

func TestCheckFile(t *testing.T) {
	dir := t.TempDir()
	file := filepath.Join(dir, "agent")
	if err := os.WriteFile(file, []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := machine.CheckFile(file); err != nil {
		t.Errorf("a regular file: %v", err)
	}
	for what, path := range map[string]string{"a directory": dir, "a relative path": "agent", "a missing file": filepath.Join(dir, "none")} {
		if err := machine.CheckFile(path); err == nil {
			t.Errorf("%s: accepted", what)
		}
	}
}

func TestConfigFromEnvAndValues(t *testing.T) {
	t.Setenv("DOCKER_HOST", "tcp://h:2375")
	t.Setenv("DISCO_VM_DOCKER_ROOT", "")
	t.Setenv("DISCO_VM_AGENT", "")
	values := machine.ConfigFromEnv("docker")
	if len(values) != 1 || values["host"] != "tcp://h:2375" {
		t.Fatalf("docker's configuration in the environment: %v", values)
	}
	values["root"] = "/srv"
	raw, err := machine.ConfigFromValues("docker", values)
	if err != nil || string(raw) != `{"host":"tcp://h:2375","root":"/srv"}` {
		t.Fatalf("%s, %v", raw, err)
	}
	if raw, err := machine.ConfigFromValues("fake", map[string]string{"noCommit": "1", "maxRunning": "2"}); err != nil || string(raw) != `{"maxRunning":2,"noCommit":true}` {
		t.Fatalf("a boolean and a number as text: %s, %v", raw, err)
	}
	if _, err := machine.ConfigFromValues("fake", map[string]string{"noCommit": "maybe"}); err == nil {
		t.Fatal("noCommit=maybe")
	}
	if _, err := machine.ConfigFromValues("fake", map[string]string{"host": "x"}); err == nil || !strings.Contains(err.Error(), "unknown field") {
		t.Fatalf("another driver's field: %v", err)
	}
	if raw, err := machine.ConfigFromValues("fake", nil); raw != nil || err != nil {
		t.Fatalf("no values: %s, %v", raw, err)
	}
}

// TestConfigEnvDisagrees names a variable that says otherwise than a whole
// configuration, never one that agrees with it or with a default it leaves.
func TestConfigEnvDisagrees(t *testing.T) {
	t.Setenv("DOCKER_HOST", "tcp://h:2375")
	t.Setenv("DISCO_VM_DOCKER_ROOT", "/var/lib/discobox")
	t.Setenv("DISCO_VM_AGENT", "")
	for config, want := range map[string][]string{
		`{"host":"tcp://h:2375"}`: nil,
		`{}`:                      {"DOCKER_HOST"},
		`{"host":"tcp://h:2375","root":"/srv/docker"}`: {"DISCO_VM_DOCKER_ROOT"},
	} {
		if got := machine.ConfigEnvDisagrees("docker", json.RawMessage(config)); !slices.Equal(got, want) {
			t.Errorf("%s: %v, want %v", config, got, want)
		}
	}
}

func TestCanonicalConfig(t *testing.T) {
	for _, config := range []string{"", "{}", " { } "} {
		if got, err := machine.CanonicalConfig(json.RawMessage(config)); err != nil || got != "{}" {
			t.Errorf("%q: %q, %v", config, got, err)
		}
	}
	a, _ := machine.CanonicalConfig(json.RawMessage(`{"b": 1, "a": true}`))
	b, _ := machine.CanonicalConfig(json.RawMessage(`{"a":true,"b":1}`))
	if a != b {
		t.Errorf("%s and %s", a, b)
	}
}

// testDriver is TestRegistration's, which no other test reads, registered
// once however many times the test runs.
const testDriver = "configtest"

var registerTestDriver sync.Once

// TestRegistration covers what only a registration can say: a required
// field, a number, retired fields, and a malformed definition.
func TestRegistration(t *testing.T) {
	registerTestDriver.Do(func() {
		machine.Register(testDriver, machine.Registration{
			New: func(machine.Config) (machine.Driver, error) { return nil, nil },
			Definition: machine.Definition{ConfigFields: []machine.ConfigField{
				{Key: "need", Type: machine.ConfigString, Required: true},
				{Key: "count", Type: machine.ConfigNumber},
				{Key: "endpoint", Type: machine.ConfigString, Privileged: true},
				{Key: "oldEndpoint", Type: machine.ConfigString, Privileged: true, Retired: true, ReplacedBy: "endpoint"},
				{Key: "gone", Type: machine.ConfigBoolean, Retired: true},
			}},
			Validate: func(json.RawMessage) error { return nil },
		})
	})
	for _, config := range []string{``, `{}`, `{"count":1}`} {
		if err := machine.Validate(testDriver, json.RawMessage(config)); err == nil || !strings.Contains(err.Error(), "need is required") {
			t.Errorf("%q without a required field: %v", config, err)
		}
	}
	if raw, err := machine.ConfigFromValues(testDriver, map[string]string{"need": "x", "count": "2.5"}); err != nil || string(raw) != `{"count":2.5,"need":"x"}` {
		t.Errorf("%s, %v", raw, err)
	}

	// A retired key is decoded into its replacement, or dropped, for good,
	// and refused beside its replacement; it is listed nowhere a person
	// reads.
	for config, want := range map[string]string{
		`{"need":"x","oldEndpoint":"e"}`: `{"endpoint":"e","need":"x"}`,
		`{"need":"x","gone":true}`:       `{"need":"x"}`,
	} {
		if got, err := machine.Normalize(testDriver, json.RawMessage(config)); err != nil || string(got) != want {
			t.Errorf("%s: %s, %v, want %s", config, got, err, want)
		}
	}
	if _, err := machine.Normalize(testDriver, json.RawMessage(`{"need":"x","endpoint":"a","oldEndpoint":"b"}`)); err == nil || !strings.Contains(err.Error(), "both are given") {
		t.Errorf("a retired key beside its replacement: %v", err)
	}
	if def, _ := machine.DefinitionOf(testDriver); !slices.Equal(def.Keys(), []string{"need", "count", "endpoint"}) {
		t.Errorf("current keys: %v", def.Keys())
	}

	newFn := func(machine.Config) (machine.Driver, error) { return nil, nil }
	validate := func(json.RawMessage) error { return nil }
	for name, r := range map[string]machine.Registration{
		"no validator": {New: newFn},
		"twice": {New: newFn, Validate: validate,
			Definition: machine.Definition{ConfigFields: []machine.ConfigField{{Key: "a", Type: machine.ConfigString}, {Key: "a", Type: machine.ConfigString}}}},
		"a bad type": {New: newFn, Validate: validate,
			Definition: machine.Definition{ConfigFields: []machine.ConfigField{{Key: "a", Type: "list"}}}},
		"a secret type": {New: newFn, Validate: validate,
			Definition: machine.Definition{ConfigFields: []machine.ConfigField{{Key: "a", Type: "secret"}}}},
		"a retired alias less privileged": {New: newFn, Validate: validate,
			Definition: machine.Definition{ConfigFields: []machine.ConfigField{
				{Key: "a", Type: machine.ConfigString, Privileged: true},
				{Key: "old", Type: machine.ConfigString, Retired: true, ReplacedBy: "a"}}}},
		"a retired alias of nothing": {New: newFn, Validate: validate,
			Definition: machine.Definition{ConfigFields: []machine.ConfigField{
				{Key: "old", Type: machine.ConfigString, Retired: true, ReplacedBy: "a"}}}},
		"a base with a key": {New: newFn, Validate: validate,
			Definition: machine.Definition{Base: &machine.ConfigField{Key: "base"}}},
		"a secret of another type": {New: newFn, Validate: validate,
			Definition: machine.Definition{Secrets: []machine.SecretField{{Key: "k", Type: machine.ConfigString, Env: []string{"K"}}}}},
		"a secret from no variable": {New: newFn, Validate: validate,
			Definition: machine.Definition{Secrets: []machine.SecretField{{Key: "k", Type: machine.ConfigSecret}}}},
		"two secrets": {New: newFn, Validate: validate,
			Definition: machine.Definition{Secrets: []machine.SecretField{
				{Key: "k", Type: machine.ConfigSecret, Env: []string{"K"}}, {Key: "t", Type: machine.ConfigSecret, Env: []string{"T"}}}}},
		"a secret with a field's key": {New: newFn, Validate: validate,
			Definition: machine.Definition{ConfigFields: []machine.ConfigField{{Key: "k", Type: machine.ConfigString}},
				Secrets: []machine.SecretField{{Key: "k", Type: machine.ConfigSecret, Env: []string{"K"}}}}},
		"a send in no place": {New: newFn, Validate: validate,
			Definition: secretSentTo(machine.SecretSend{Protocol: machine.SendHTTPS, Hosts: []string{"h:443"}})},
		"a send in two places": {New: newFn, Validate: validate,
			Definition: secretSentTo(machine.SecretSend{Protocol: machine.SendHTTPS, Hosts: []string{"h:443"}, Header: "A", JSONField: "k"})},
		"a send over another protocol": {New: newFn, Validate: validate,
			Definition: secretSentTo(machine.SecretSend{Protocol: "smtp", Hosts: []string{"h:25"}, Header: "A"})},
		"a send to no host": {New: newFn, Validate: validate,
			Definition: secretSentTo(machine.SecretSend{Protocol: machine.SendHTTPS, Header: "A"})},
		"a send to a host with no port": {New: newFn, Validate: validate,
			Definition: secretSentTo(machine.SecretSend{Protocol: machine.SendHTTPS, Hosts: []string{"h"}, Header: "A"})},
		"a send to an unknown field": {New: newFn, Validate: validate,
			Definition: secretSentTo(machine.SecretSend{Protocol: machine.SendHTTPS, Endpoint: "nope", Header: "A"})},
		"a send to a field not privileged": {New: newFn, Validate: validate,
			Definition: secretSentTo(machine.SecretSend{Protocol: machine.SendHTTPS, Endpoint: "plain", Header: "A"})},
		"a send to a field with no default and no hosts": {New: newFn, Validate: validate,
			Definition: secretSentTo(machine.SecretSend{Protocol: machine.SendHTTPS, Endpoint: "bare", Header: "A"})},
		"a send to a relative path": {New: newFn, Validate: validate,
			Definition: secretSentTo(machine.SecretSend{Protocol: machine.SendHTTPS, Endpoint: "url", Path: "api/", Header: "A"})},
	} {
		func() {
			defer func() {
				if recover() == nil {
					t.Errorf("registered a driver with %s", name)
				}
			}()
			// Each panics before it registers, so a rerun registers none.
			machine.Register("bad-"+strings.ReplaceAll(name, " ", "-"), r)
		}()
	}
}

// secretSentTo is a definition whose one secret is sent as send, beside
// fields send can name: url (privileged, with a default), plain (not
// privileged), and bare (privileged, with no default).
func secretSentTo(send machine.SecretSend) machine.Definition {
	return machine.Definition{
		ConfigFields: []machine.ConfigField{
			{Key: "url", Type: machine.ConfigString, Privileged: true, Default: "https://h/"},
			{Key: "plain", Type: machine.ConfigString, Default: "https://h/"},
			{Key: "bare", Type: machine.ConfigString, Privileged: true},
		},
		Secrets: []machine.SecretField{{Key: "k", Type: machine.ConfigSecret, Env: []string{"K"}, Sends: []machine.SecretSend{send}}},
	}
}
