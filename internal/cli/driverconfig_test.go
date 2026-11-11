package cli

import (
	"encoding/json"
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
