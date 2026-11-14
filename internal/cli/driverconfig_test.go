package cli

import (
	"encoding/json"
	"os"
	"testing"

	"github.com/spf13/pflag"

	"github.com/discobox-ai/vm/pkg/machine"
)

// A driver of this test's alone, with a field retired for another.
func init() {
	machine.Register("clitest", machine.Registration{
		New: func(machine.Config) (machine.Driver, error) { return nil, nil },
		Definition: machine.Definition{ConfigFields: []machine.ConfigField{
			{Key: "endpoint", Label: "Endpoint", Type: machine.ConfigString, Env: "CLITEST_ENDPOINT", Description: "now"},
			{Key: "oldEndpoint", Label: "Old", Type: machine.ConfigString, Env: "CLITEST_OLD_ENDPOINT", Description: "before", Retired: true, ReplacedBy: "endpoint"},
		}},
		Validate: func(json.RawMessage) error { return nil },
	})
	machine.Register("clitest-exe", machine.Registration{
		New: func(machine.Config) (machine.Driver, error) { return nil, nil },
		Definition: machine.Definition{ConfigFields: []machine.ConfigField{
			{Key: "agent", Label: "Agent", Type: machine.ConfigString, Env: "CLITEST_EXE_AGENT", Privileged: true, DefaultExecutable: true},
			{Key: "other", Label: "Other", Type: machine.ConfigString},
		}},
		Validate: func(json.RawMessage) error { return nil },
	})
}

// A field that defaults to the executable is this disco-vm wherever the
// configuration leaves it out, whole or from flags and variables, and what
// it is given otherwise.
func TestFieldThatDefaultsToTheExecutable(t *testing.T) {
	exe, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	self, err := json.Marshal(exe)
	if err != nil {
		t.Fatal(err)
	}
	for _, c := range []struct {
		name, config, env, want string
	}{
		{"nothing given", "", "", `{"agent":` + string(self) + `}`},
		{"its variable", "", "/a", `{"agent":"/a"}`},
		{"a whole configuration without it", `{"other":"o"}`, "", `{"agent":` + string(self) + `,"other":"o"}`},
		{"a whole configuration with it", `{"agent":"/b"}`, "/a", `{"agent":"/b"}`},
		{"a whole configuration of null", `null`, "", `{"agent":` + string(self) + `}`},
	} {
		t.Run(c.name, func(t *testing.T) {
			t.Setenv("CLITEST_EXE_AGENT", c.env)
			g := &globals{config: c.config}
			g.addDriverFlags(pflag.NewFlagSet("t", pflag.ContinueOnError))
			config, err := g.driverConfig("clitest-exe")
			if err != nil {
				t.Fatal(err)
			}
			got, _ := machine.CanonicalConfig(config)
			if got != c.want {
				t.Fatalf("%s, want %s", got, c.want)
			}
		})
	}
}

// A retired field has no flag and keeps its variable, which gives way to its
// replacement's flag or variable rather than being refused beside it.
func TestRetiredFieldOnTheCommandLine(t *testing.T) {
	for _, c := range []struct {
		name     string
		args     []string
		old, cur string
		want     string
	}{
		{"the retired variable alone", nil, "a", "", `{"oldEndpoint":"a"}`},
		{"beside its replacement's flag", []string{"--clitest-endpoint", "b"}, "a", "", `{"endpoint":"b"}`},
		{"beside its replacement's variable", nil, "a", "c", `{"endpoint":"c"}`},
	} {
		t.Run(c.name, func(t *testing.T) {
			t.Setenv("CLITEST_OLD_ENDPOINT", c.old)
			t.Setenv("CLITEST_ENDPOINT", c.cur)
			g := &globals{}
			flags := pflag.NewFlagSet("t", pflag.ContinueOnError)
			g.addDriverFlags(flags)
			if flags.Lookup("clitest-old-endpoint") != nil {
				t.Fatal("a retired field has a flag")
			}
			if err := flags.Parse(c.args); err != nil {
				t.Fatal(err)
			}
			config, err := g.driverConfig("clitest")
			if err != nil || string(config) != c.want {
				t.Fatalf("%s, %v, want %s", config, err, c.want)
			}
			if _, err := machine.Normalize("clitest", config); err != nil {
				t.Fatal(err)
			}
		})
	}
}

// --url reads SCOPE=on|off[,strict], once a scope, and refuses anything else.
func TestParseURLRequests(t *testing.T) {
	got, err := parseURLRequests([]string{"public=off,strict", "private=on"})
	want := machine.URLRequests{Public: machine.URLRequest{Mode: machine.URLOff, Strict: true}, Private: machine.URLRequest{Mode: machine.URLOn}}
	if err != nil || got != want {
		t.Fatalf("parse = %+v, %v; want %+v", got, err, want)
	}
	for _, bad := range [][]string{{"public"}, {"public=yes"}, {"public=off,lax"}, {"edge=on"}, {"public=on", "public=off"}} {
		if _, err := parseURLRequests(bad); err == nil {
			t.Errorf("parse %q succeeded", bad)
		}
	}
}
