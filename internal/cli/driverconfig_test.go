package cli

import (
	"bytes"
	"crypto/sha256"
	"debug/elf"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"io/fs"
	"os"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"testing"

	"github.com/spf13/pflag"

	"github.com/discobox-ai/vm/pkg/machine"
	"github.com/discobox-ai/vm/pkg/machine/agent"
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
			{Key: "agent", Label: "Agent", Type: machine.ConfigString, Env: "CLITEST_EXE_AGENT", Privileged: true, AgentOS: machine.Linux},
			{Key: "other", Label: "Other", Type: machine.ConfigString},
		}},
		Validate: func(json.RawMessage) error { return nil },
	})
}

// elfHeader is the smallest file debug/elf opens: a linux executable for
// arch, which agent.Check takes on every OS.
func elfHeader(t *testing.T, arch string) []byte {
	t.Helper()
	h := elf.Header64{
		Type:    uint16(elf.ET_EXEC),
		Machine: uint16(map[string]elf.Machine{"amd64": elf.EM_X86_64, "arm64": elf.EM_AARCH64}[arch]),
		Version: uint32(elf.EV_CURRENT),
		Ehsize:  64,
	}
	copy(h.Ident[:], elf.ELFMAG)
	h.Ident[elf.EI_CLASS] = byte(elf.ELFCLASS64)
	h.Ident[elf.EI_DATA] = byte(elf.ELFDATA2LSB)
	h.Ident[elf.EI_VERSION] = byte(elf.EV_CURRENT)
	var buf bytes.Buffer
	if err := binary.Write(&buf, binary.LittleEndian, h); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}

// carry is what a disco-vm carrying data as its linux/arch agent carries.
func carry(t *testing.T, data []byte, arch string) fs.FS {
	t.Helper()
	src := filepath.Join(t.TempDir(), "disco-vm")
	if err := os.WriteFile(src, data, 0o755); err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	if err := agent.Carry(dir, src, "linux", arch); err != nil {
		t.Fatal(err)
	}
	return os.DirFS(dir)
}

// otherArch is an architecture this host is not.
func otherArch() string {
	if runtime.GOARCH == "arm64" {
		return "amd64"
	}
	return "arm64"
}

// A field that names a Linux agent is, wherever the configuration leaves it
// out, this disco-vm if it is a linux one for this host's architecture, else
// the one it carries, written out once under the root, else left out; and it
// is what it is given otherwise (ADR 26-11-14-352 §2).
func TestAgentFieldDefaults(t *testing.T) {
	dir := t.TempDir()
	linuxExe := filepath.Join(dir, "linux-disco-vm")
	if err := os.WriteFile(linuxExe, elfHeader(t, runtime.GOARCH), 0o755); err != nil {
		t.Fatal(err)
	}
	otherExe := filepath.Join(dir, "other-disco-vm")
	if err := os.WriteFile(otherExe, []byte("not an executable for linux"), 0o755); err != nil {
		t.Fatal(err)
	}
	carriedAgent := elfHeader(t, runtime.GOARCH)
	sum := sha256.Sum256(carriedAgent)
	carriesIt := carry(t, carriedAgent, runtime.GOARCH)
	carriesOther := carry(t, elfHeader(t, otherArch()), otherArch())
	quote := strconv.Quote

	for _, c := range []struct {
		name, exe, config, env string
		carried                fs.FS
		// want is the configuration, with ROOT for where the carried
		// agent is written out.
		want string
	}{
		{name: "this disco-vm, a linux one", exe: linuxExe, want: `{"agent":` + quote(linuxExe) + `}`},
		{name: "this disco-vm, beside what it carries", exe: linuxExe, carried: carriesIt, want: `{"agent":` + quote(linuxExe) + `}`},
		{name: "the carried agent", exe: otherExe, carried: carriesIt,
			want: `{"agent":ROOT}`},
		{name: "a whole configuration without it", exe: otherExe, config: `{"other":"o"}`, carried: carriesIt,
			want: `{"agent":ROOT,"other":"o"}`},
		{name: "a whole configuration of null", exe: linuxExe, config: `null`, want: `{"agent":` + quote(linuxExe) + `}`},
		{name: "only another architecture's carried", exe: otherExe, carried: carriesOther, want: `{}`},
		{name: "nothing carried", exe: otherExe, want: `{}`},
		{name: "its variable", exe: linuxExe, env: "/a", want: `{"agent":"/a"}`},
		{name: "a whole configuration with it", exe: otherExe, config: `{"agent":"/b"}`, env: "/a", carried: carriesIt, want: `{"agent":"/b"}`},
	} {
		t.Run(c.name, func(t *testing.T) {
			exe := c.exe
			executable = func() (string, error) { return exe, nil }
			t.Cleanup(func() { executable = os.Executable })
			t.Setenv("CLITEST_EXE_AGENT", c.env)
			root := t.TempDir()
			g := &globals{config: c.config, root: root, carried: c.carried}
			g.addDriverFlags(pflag.NewFlagSet("t", pflag.ContinueOnError))
			config, err := g.driverConfig("clitest-exe")
			if err != nil {
				t.Fatal(err)
			}
			got, _ := machine.CanonicalConfig(config)
			unpacked := filepath.Join(root, "cache", "cli", "agent", "sha256-"+hex.EncodeToString(sum[:])+"-linux-"+runtime.GOARCH, "disco-vm")
			if want := strings.ReplaceAll(c.want, "ROOT", quote(unpacked)); got != want {
				t.Fatalf("%s, want %s", got, want)
			}
			if !strings.Contains(c.want, "ROOT") {
				return
			}
			data, err := os.ReadFile(unpacked)
			if err != nil || !bytes.Equal(data, carriedAgent) {
				t.Fatalf("the carried agent written out: %v", err)
			}
			if runtime.GOOS != "windows" {
				for _, p := range []string{filepath.Join(root, "cache", "cli", "agent"), unpacked} {
					if info, err := os.Stat(p); err != nil || info.Mode().Perm()&0o077 != 0 {
						t.Fatalf("%s: %v, want it owner-only", p, err)
					}
				}
			}
			// Asked again, by another command, it is the same file.
			again := &globals{config: c.config, root: root, carried: c.carried}
			again.addDriverFlags(pflag.NewFlagSet("t", pflag.ContinueOnError))
			if config, err := again.driverConfig("clitest-exe"); err != nil {
				t.Fatal(err)
			} else if g2, _ := machine.CanonicalConfig(config); g2 != got {
				t.Fatalf("again: %s, want %s", g2, got)
			}
		})
	}
}

// A carried agent that cannot be written out leaves the field absent, with a
// warning, and fails no command: most need no agent, and a boot that does is
// refused by its driver, naming the field (ADR 26-11-14-352 §2, step 3).
func TestAgentFieldUnwritable(t *testing.T) {
	dir := t.TempDir()
	otherExe := filepath.Join(dir, "other-disco-vm")
	if err := os.WriteFile(otherExe, []byte("no"), 0o755); err != nil {
		t.Fatal(err)
	}
	executable = func() (string, error) { return otherExe, nil }
	var warned strings.Builder
	stderr = &warned
	t.Cleanup(func() { executable, stderr = os.Executable, os.Stderr })
	t.Setenv("CLITEST_EXE_AGENT", "")
	// The root is a file, so no cache can be made under it.
	root := filepath.Join(dir, "root")
	if err := os.WriteFile(root, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	g := &globals{root: root, carried: carry(t, elfHeader(t, runtime.GOARCH), runtime.GOARCH)}
	g.addDriverFlags(pflag.NewFlagSet("t", pflag.ContinueOnError))
	for range 2 {
		config, err := g.driverConfig("clitest-exe")
		if err != nil {
			t.Fatalf("a configuration read failed: %v", err)
		}
		if got, _ := machine.CanonicalConfig(config); got != `{}` {
			t.Fatalf("%s, want the agent left out", got)
		}
	}
	if n := strings.Count(warned.String(), "disco-vm: warning: no linux agent:"); n != 1 {
		t.Fatalf("warned %d times, want once:\n%s", n, warned.String())
	}
}

// --help=DRIVER says what an agent field defaults to: this disco-vm, the one
// it carries, or, with neither, nothing.
func TestAgentFieldHelp(t *testing.T) {
	dir := t.TempDir()
	linuxExe := filepath.Join(dir, "linux-disco-vm")
	if err := os.WriteFile(linuxExe, elfHeader(t, runtime.GOARCH), 0o755); err != nil {
		t.Fatal(err)
	}
	otherExe := filepath.Join(dir, "other-disco-vm")
	if err := os.WriteFile(otherExe, []byte("no"), 0o755); err != nil {
		t.Fatal(err)
	}
	for _, c := range []struct {
		name, exe string
		carried   fs.FS
		want      string
	}{
		{"this disco-vm", linuxExe, nil, "default: this disco-vm"},
		{"carried", otherExe, carry(t, elfHeader(t, runtime.GOARCH), runtime.GOARCH), "default: the linux/" + runtime.GOARCH + " disco-vm this one carries"},
		{"neither", otherExe, nil, ""},
	} {
		t.Run(c.name, func(t *testing.T) {
			exe := c.exe
			executable = func() (string, error) { return exe, nil }
			t.Cleanup(func() { executable = os.Executable })
			g := &globals{carried: c.carried}
			var out strings.Builder
			if err := writeDriverHelp(&out, "clitest-exe", g.agentDefault); err != nil {
				t.Fatal(err)
			}
			if c.want == "" {
				if strings.Contains(out.String(), "default:") {
					t.Fatalf("help says a default:\n%s", out.String())
				}
				return
			}
			if !strings.Contains(out.String(), c.want) {
				t.Fatalf("help:\n%s\nwant %q", out.String(), c.want)
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
