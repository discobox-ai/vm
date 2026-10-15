package converge

import (
	"bytes"
	"context"
	"errors"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/discobox-ai/vm/pkg/guest"
	"github.com/discobox-ai/vm/pkg/machine"
)

// localGuest is a guest that is a directory on this machine, as the fake
// driver's is: copies land under its root, and commands run here, with this
// OS's shell, so a run's real scripts are what is tested. Packages,
// services, and owners go to the ledger a rooted guest keeps.
type localGuest struct {
	root string

	mu sync.Mutex
	// scripts is every exec resource's script, and shells the shell each
	// was given.
	scripts []string
	shells  [][]string
	// failScript fails every exec resource while it is set.
	failScript bool
	// lost fails every command, as a guest that stopped answering does.
	lost bool
}

func (g *localGuest) Info(context.Context) (machine.GuestInfo, error) {
	return machine.GuestInfo{OS: runtime.GOOS, Root: g.root}, nil
}

func (g *localGuest) CopyFrom(context.Context, string) (io.ReadCloser, error) {
	return nil, errors.New("a run does not copy out")
}

func (g *localGuest) Shutdown(context.Context, bool) error {
	return errors.New("a run does not shut its guest down")
}

// Exec runs the command once its output is waited for.
func (g *localGuest) Exec(ctx context.Context, req machine.ExecRequest) (machine.Process, error) {
	return &localProcess{run: func(stdout, stderr io.Writer) (int, error) { return g.run(ctx, req, stdout, stderr) }}, nil
}

// localProcess is a command that takes no stdin and runs in Wait.
type localProcess struct {
	run func(stdout, stderr io.Writer) (int, error)
}

func (*localProcess) Write([]byte) (int, error)                    { return 0, errors.New("no stdin") }
func (*localProcess) CloseStdin() error                            { return nil }
func (*localProcess) Resize(uint16, uint16) error                  { return nil }
func (*localProcess) Close() error                                 { return nil }
func (p *localProcess) Wait(stdout, stderr io.Writer) (int, error) { return p.run(stdout, stderr) }

func (g *localGuest) CopyTo(_ context.Context, dir string, tarStream io.Reader) error {
	if g.isLost() {
		return errors.New("the guest is not answering")
	}
	return guest.Unpack(tarStream, guest.Resolve(g.root, dir))
}

func (g *localGuest) isLost() bool {
	g.mu.Lock()
	defer g.mu.Unlock()
	return g.lost
}

// run runs a run's own scripts for real, and records an exec resource's
// script instead of running it: its text is the test's, not a program.
func (g *localGuest) run(ctx context.Context, req machine.ExecRequest, stdout, stderr io.Writer) (int, error) {
	if g.isLost() {
		return 0, errors.New("the guest is not answering")
	}
	last := req.Argv[len(req.Argv)-1]
	if !strings.HasPrefix(last, g.root) {
		g.mu.Lock()
		defer g.mu.Unlock()
		script := strings.TrimPrefix(last, dialectFor(runtime.GOOS).execPrelude())
		g.scripts = append(g.scripts, script)
		g.shells = append(g.shells, req.Argv[:len(req.Argv)-1])
		_, _ = io.WriteString(stdout, "ran "+script+"\n")
		if g.failScript {
			return 1, nil
		}
		return 0, nil
	}
	cmd := exec.CommandContext(ctx, req.Argv[0], req.Argv[1:]...) //nolint:gosec // G204: the run's own scripts
	cmd.Dir, cmd.Stdout, cmd.Stderr = g.root, stdout, stderr
	err := cmd.Run()
	if exit, ok := errors.AsType[*exec.ExitError](err); ok {
		return exit.ExitCode(), nil
	}
	return 0, err
}

// pwshOnUnix is the Windows dialect's scripts run by a PowerShell on this
// machine, with this machine's paths: what can be tested of them without a
// Windows guest. DISCO_VM_TEST_PWSH names the pwsh to run them with.
type pwshOnUnix struct {
	powershell
	pwsh string
}

func (pwshOnUnix) stateDir() string           { return posix{}.stateDir() }
func (pwshOnUnix) join(elem ...string) string { return posix{}.join(elem...) }
func (p pwshOnUnix) argv(script string) []string {
	return []string{p.pwsh, "-NoProfile", "-NonInteractive", "-File", script}
}

// modes reports whether the guest's shell gives a file its mode, which
// PowerShell does not.
func modes() bool {
	_, isPosix := dialectFor(runtime.GOOS).(posix)
	return isPosix
}

func TestMain(m *testing.M) {
	if pwsh := os.Getenv("DISCO_VM_TEST_PWSH"); pwsh != "" && runtime.GOOS != "windows" {
		dialectFor = func(string) dialect { return pwshOnUnix{pwsh: pwsh} }
	}
	os.Exit(m.Run())
}

type env struct {
	t     *testing.T
	guest *localGuest
	// dir holds the state files, on the client.
	dir string
	out bytes.Buffer
}

func newEnv(t *testing.T) *env {
	t.Helper()
	root, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	return &env{t: t, guest: &localGuest{root: root}, dir: t.TempDir()}
}

// state writes a file under the state file directory.
func (e *env) state(name, content string) string {
	e.t.Helper()
	path := filepath.Join(e.dir, filepath.FromSlash(name))
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		e.t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		e.t.Fatal(err)
	}
	return path
}

// run converges the guest to the paths, or to the whole directory.
func (e *env) run(paths ...string) (Status, error) {
	e.t.Helper()
	if len(paths) == 0 {
		paths = []string{e.dir}
	}
	e.out.Reset()
	err := Run(e.t.Context(), e.guest, Options{Paths: paths, Out: &e.out, Lease: 4 * time.Second})
	status, statusErr := ReadStatus(e.t.Context(), e.guest)
	if statusErr != nil {
		e.t.Fatal(statusErr)
	}
	return status, err
}

func (e *env) converged(paths ...string) Status {
	e.t.Helper()
	status, err := e.run(paths...)
	if err != nil || status.State != Converged {
		e.t.Fatalf("run: %v\nstatus: %+v\n%s", err, status, e.out.String())
	}
	return status
}

func (e *env) path(guestPath string) string { return guest.Resolve(e.guest.root, guestPath) }

func (e *env) read(guestPath string) string {
	e.t.Helper()
	data, err := os.ReadFile(e.path(guestPath))
	if err != nil {
		e.t.Fatal(err)
	}
	return string(data)
}

func (e *env) write(guestPath, content string) {
	e.t.Helper()
	if err := os.WriteFile(e.path(guestPath), []byte(content), 0o644); err != nil {
		e.t.Fatal(err)
	}
}

func (e *env) stateDir() string { return e.path(dialectFor(runtime.GOOS).stateDir()) }

// ledgerHas reports whether the guest wrote a line down in place of changing
// its host.
func (e *env) ledgerHas(line string) bool {
	e.t.Helper()
	data, err := os.ReadFile(filepath.Join(e.stateDir(), ledgerName))
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		e.t.Fatal(err)
	}
	for got := range strings.Lines(string(data)) {
		if strings.TrimSpace(got) == line {
			return true
		}
	}
	return false
}

// applied is the batches the last run applied, in order.
func (e *env) applied() []string {
	var out []string
	for line := range strings.Lines(e.out.String()) {
		if label, ok := strings.CutPrefix(strings.TrimSpace(line), "--> "); ok {
			out = append(out, label)
		}
	}
	return out
}

const harness = `
packages:
  npm: ["@scope/agent@1.2.3"]
  apt: [jq, "nodejs=25.*"]
files:
  - path: /etc/agent/settings.json
    source: settings.json
    mode: "0600"
    owner: root:root
  - path: /etc/profile.d/agent.sh
    content: |
      export AGENT=1
  - path: /usr/local/bin/hook
    link: /usr/local/bin/agent
  - path: /usr/local/libexec/agent
    directory: true
services:
  enabled: [agent.socket]
  masked: [apt-daily.timer]
exec:
  - name: seed
    key: "1"
    run: seed the store
`

func TestConverge(t *testing.T) {
	e := newEnv(t)
	e.state("10-harness.yaml", harness)
	e.state("settings.json", `{"theme":"dark"}`)

	status := e.converged()
	if status.Resources != 10 || status.Done != 10 || status.Files["10-harness.yaml"] == "" || status.Finished == nil {
		t.Fatalf("status after the first run: %+v", status)
	}
	if got := e.read("/etc/agent/settings.json"); got != `{"theme":"dark"}` {
		t.Fatalf("a source file holds %q", got)
	}
	if got := e.read("/etc/profile.d/agent.sh"); got != "export AGENT=1\n" {
		t.Fatalf("an inline file holds %q", got)
	}
	if modes() {
		if info, err := os.Stat(e.path("/etc/agent/settings.json")); err != nil || info.Mode().Perm() != 0o600 {
			t.Fatalf("a file's mode: %v %v", info, err)
		}
	}
	if target, err := os.Readlink(e.path("/usr/local/bin/hook")); err != nil || target != "/usr/local/bin/agent" {
		t.Fatalf("a link points at %q: %v", target, err)
	}
	if info, err := os.Stat(e.path("/usr/local/libexec/agent")); err != nil || !info.IsDir() {
		t.Fatalf("a directory: %v %v", info, err)
	}
	// Files, then each provider's packages as one install in the order the
	// state file names the providers, then the script, then the services.
	want := []string{
		"10-harness.yaml: 4 files", "package:npm/@scope/agent@1.2.3", "package:apt/jq,nodejs=25.*",
		"exec:seed", "10-harness.yaml: 2 services",
	}
	if got := e.applied(); !slices.Equal(got, want) {
		t.Fatalf("applied %v, want %v", got, want)
	}
	for _, line := range []string{
		"package npm @scope/agent@1.2.3", "package apt jq", "package apt nodejs=25.*",
		"service agent.socket enabled", "service apt-daily.timer masked",
		"owner " + e.path("/etc/agent/settings.json") + " root:root",
	} {
		if !e.ledgerHas(line) {
			t.Fatalf("the ledger has no %q", line)
		}
	}
	if !slices.Equal(e.guest.scripts, []string{"seed the store"}) {
		t.Fatalf("scripts: %v", e.guest.scripts)
	}

	// A second run applies nothing: the guest's record has every digest.
	// What a user changed by hand stays changed.
	e.write("/etc/profile.d/agent.sh", "mine")
	if e.converged(); len(e.applied()) != 0 || len(e.guest.scripts) != 1 {
		t.Fatalf("a second run applied %v, scripts %v", e.applied(), e.guest.scripts)
	}
	if got := e.read("/etc/profile.d/agent.sh"); got != "mine" {
		t.Fatalf("a recorded file was written again: %q", got)
	}

	// A changed declaration is applied, and only it. A script runs again
	// when its key changes.
	e.state("settings.json", `{"theme":"light"}`)
	e.state("10-harness.yaml", strings.Replace(harness, `key: "1"`, `key: "2"`, 1))
	e.converged()
	if got := e.applied(); !slices.Equal(got, []string{"file:/etc/agent/settings.json", "exec:seed"}) {
		t.Fatalf("a changed source and key applied %v", got)
	}
	if e.read("/etc/agent/settings.json") != `{"theme":"light"}` || e.read("/etc/profile.d/agent.sh") != "mine" {
		t.Fatal("the change was not applied as a difference")
	}
	// A run leaves nothing of its own in the guest but the record and status.
	if entries, _ := os.ReadDir(filepath.Join(e.stateDir(), "run")); len(entries) != 0 {
		t.Fatalf("a run left its scripts behind: %v", entries)
	}
	if _, err := os.Stat(filepath.Join(e.stateDir(), "lease")); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("a finished run still holds the lease: %v", err)
	}
}

// A link holds its target as written, relative or not there yet, and one to
// a directory leads into it: a file placed after it, at a path through it,
// lands in the directory.
func TestLinkTargets(t *testing.T) {
	e := newEnv(t)
	e.state("10-links.yaml", `
files:
  - {path: /opt/agent/1.0, directory: true}
  - {path: /opt/agent/current, link: "1.0"}
  - {path: /opt/agent/next, link: "2.0"}
  - {path: /opt/agent/current/extra.conf, content: x}
`)
	e.converged()
	for path, want := range map[string]string{"/opt/agent/current": "1.0", "/opt/agent/next": "2.0"} {
		if target, err := os.Readlink(e.path(path)); err != nil || target != want {
			t.Fatalf("%s points at %q: %v", path, target, err)
		}
	}
	if info, err := os.Stat(e.path("/opt/agent/current")); err != nil || !info.IsDir() {
		t.Fatalf("a link to a directory: %v %v", info, err)
	}
	if got := e.read("/opt/agent/1.0/extra.conf"); got != "x" {
		t.Fatalf("a file placed through a link holds %q", got)
	}
}

// A later state file can need what an earlier one installed: each is applied
// whole, files to services, before the next, and a provider's packages are
// one install only within a run of them.
func TestStateFilesApplyInNameOrder(t *testing.T) {
	e := newEnv(t)
	e.state("10-base.yaml", "packages:\n  apt: [curl, gnupg]\nexec:\n  - {name: key, run: fetch a key, shell: [/bin/bash, -c]}\n")
	e.state("20-repo.yaml", "files:\n  - {path: /etc/apt/sources.list.d/repo.list, content: deb}\npackages:\n  apt: [docker-ce]\n")
	e.converged()
	want := []string{"package:apt/curl,gnupg", "exec:key", "file:/etc/apt/sources.list.d/repo.list", "package:apt/docker-ce"}
	if got := e.applied(); !slices.Equal(got, want) {
		t.Fatalf("applied %v, want %v", got, want)
	}
	if !slices.Equal(e.guest.shells[0], []string{"/bin/bash", "-c"}) {
		t.Fatalf("an exec's shell: %v", e.guest.shells)
	}
	// A changed shell is a changed script, and runs again.
	e.state("10-base.yaml", "packages:\n  apt: [curl, gnupg]\nexec:\n  - {name: key, run: fetch a key}\n")
	if e.converged(); len(e.guest.scripts) != 2 || !slices.Equal(e.guest.shells[1], dialectFor(runtime.GOOS).execShell()) {
		t.Fatalf("scripts %v, shells %v", e.guest.scripts, e.guest.shells)
	}
}

// A run is given the state files to apply, and leaves the guest's others as
// they are: a layer of an image, or one changed file, is a run of its own.
func TestRunsAddUp(t *testing.T) {
	e := newEnv(t)
	base := e.state("10-base.yaml", "files:\n  - {path: /usr/bin/run, content: base}\n  - {path: /etc/base, content: base}\n")
	e.converged(base)
	harness := e.state("50-harness.yaml", "files:\n  - {path: /usr/bin/run, content: harness}\n  - {path: /etc/harness, content: h}\n")
	status := e.converged(harness)
	if status.Files["10-base.yaml"] == "" || status.Files["50-harness.yaml"] == "" {
		t.Fatalf("the status forgot a state file: %+v", status.Files)
	}
	// The later file's declaration replaced the base's, and the base's own
	// file was not taken for one that left.
	if e.read("/usr/bin/run") != "harness" || e.read("/etc/base") != "base" {
		t.Fatalf("after the harness: %q %q", e.read("/usr/bin/run"), e.read("/etc/base"))
	}
	// Given again on its own, the base does not take the path back.
	if e.converged(base); len(e.applied()) != 0 || e.read("/usr/bin/run") != "harness" {
		t.Fatalf("the base alone applied %v", e.applied())
	}
	// Given together, the later one is the resource.
	if status := e.converged(); len(e.applied()) != 0 || status.Resources != 3 {
		t.Fatalf("both applied %v: %+v", e.applied(), status)
	}
	// A state file that is emptied takes its files with it, and the base's
	// declaration is the resource again.
	e.state("50-harness.yaml", "")
	e.converged()
	if _, err := os.Stat(e.path("/etc/harness")); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("a file that left the state files is still there: %v", err)
	}
	if got := e.read("/usr/bin/run"); got != "base" {
		t.Fatalf("after the harness left, the path holds %q", got)
	}
}

// A source that is a directory is every file under it, each a resource of
// its own, and a source can be anywhere on the client.
func TestSourceDirectory(t *testing.T) {
	e := newEnv(t)
	e.state("state/a.yaml", "files:\n  - {path: /usr/share/skills, source: ../skills}\n  - {path: /opt/tools, source: tools, mode: \"0600\"}\n")
	e.state("skills/review/SKILL.md", "review")
	run := e.state("skills/run.sh", "#!/bin/sh")
	e.state("state/tools/one", "1")
	if err := os.Chmod(run, 0o755); err != nil {
		t.Fatal(err)
	}
	if status := e.converged(filepath.Join(e.dir, "state")); status.Resources != 3 {
		t.Fatalf("status: %+v", status)
	}
	if got := e.read("/usr/share/skills/review/SKILL.md"); got != "review" {
		t.Fatalf("a file in a source directory holds %q", got)
	}
	if modes() {
		for path, want := range map[string]os.FileMode{
			"/usr/share/skills/review/SKILL.md": 0o644,
			"/usr/share/skills/run.sh":          0o755, // its source can be executed
			"/opt/tools/one":                    0o600, // the declared mode
		} {
			if info, err := os.Stat(e.path(path)); err != nil || info.Mode().Perm() != want {
				t.Fatalf("%s: mode %v, want %v: %v", path, info.Mode().Perm(), want, err)
			}
		}
	}
	// A file that leaves the directory is deleted, and one that joins it is
	// the only one written.
	if err := os.Remove(run); err != nil {
		t.Fatal(err)
	}
	e.state("skills/new/SKILL.md", "new")
	e.write("/opt/tools/one", "kept")
	e.converged(filepath.Join(e.dir, "state"))
	if _, err := os.Stat(e.path("/usr/share/skills/run.sh")); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("a file that left its source directory is still there: %v", err)
	}
	if e.read("/usr/share/skills/new/SKILL.md") != "new" || e.read("/opt/tools/one") != "kept" {
		t.Fatal("a source directory's change was not applied as a difference")
	}
}

func TestRemoval(t *testing.T) {
	e := newEnv(t)
	e.state("a.yaml", "files:\n  - {path: /opt/a, content: a}\n  - {path: /opt/b, content: b}\n  - {path: /opt/d, directory: true}\npackages:\n  apt: [jq]\n")
	e.converged()
	// A directory that something else has filled stays.
	e.write("/opt/d/theirs", "x")
	e.state("a.yaml", "files:\n  - {path: /opt/a, content: a}\n")
	if status := e.converged(); status.Resources != 1 {
		t.Fatalf("status: %+v", status)
	}
	if _, err := os.Stat(e.path("/opt/b")); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("a file that left the state files is still there: %v", err)
	}
	if got := e.read("/opt/a"); got != "a" {
		t.Fatalf("a file that stayed holds %q", got)
	}
	if e.read("/opt/d/theirs") != "x" {
		t.Fatal("a directory with something in it was removed")
	}
}

func TestFailureResumes(t *testing.T) {
	e := newEnv(t)
	e.state("a.yaml", "files:\n  - {path: /opt/a, content: a}\nexec:\n  - {name: boom, run: boom}\nservices:\n  enabled: [late.service]\n")
	e.guest.failScript = true
	status, err := e.run()
	if err == nil || status.State != Failed || status.Current != "exec:boom" || status.Done != 1 || len(status.Files) != 0 {
		t.Fatalf("run: %v\nstatus: %+v", err, status)
	}
	// The error carries what the resource printed, and the status has it.
	if !strings.Contains(err.Error(), "exec:boom: exit status 1") || !strings.Contains(err.Error(), "ran boom") || status.Error != err.Error() {
		t.Fatalf("error: %q\nstatus: %q", err, status.Error)
	}
	if e.ledgerHas("service late.service enabled") {
		t.Fatal("a resource after the failure was applied")
	}
	// The next run starts at the failure: the file is not written again.
	e.write("/opt/a", "kept")
	e.guest.failScript = false
	e.converged()
	if got := e.read("/opt/a"); got != "kept" {
		t.Fatalf("a recorded resource was applied again: %q", got)
	}
	if !e.ledgerHas("service late.service enabled") {
		t.Fatal("the run after the failure did not finish")
	}
}

// A file that cannot be placed fails the run at that file, with the files
// before it in the same copy applied and recorded.
func TestFailureInABatchKeepsWhatWasApplied(t *testing.T) {
	e := newEnv(t)
	e.state("a.yaml", "files:\n  - {path: /opt/first, content: \"1\"}\n  - {path: /opt/full, content: x}\n  - {path: /opt/last, content: \"3\"}\n")
	// A directory with something in it is never removed to make way.
	if err := os.MkdirAll(e.path("/opt/full/kept"), 0o755); err != nil {
		t.Fatal(err)
	}
	status, err := e.run()
	if err == nil || status.Done != 1 || !strings.Contains(err.Error(), "file:/opt/full") {
		t.Fatalf("run: %v\nstatus: %+v", err, status)
	}
	if e.read("/opt/first") != "1" {
		t.Fatal("the file before the failure was not applied")
	}
	if err := os.RemoveAll(e.path("/opt/full")); err != nil {
		t.Fatal(err)
	}
	e.write("/opt/first", "kept")
	e.converged()
	if e.read("/opt/first") != "kept" || e.read("/opt/full") != "x" || e.read("/opt/last") != "3" {
		t.Fatal("the run after the failure did not start at it")
	}
}

func TestLoadRefuses(t *testing.T) {
	for name, files := range map[string]map[string]string{
		"an unknown key":            {"a.yaml": "pkgs: {apt: [jq]}\n"},
		"a relative path":           {"a.yaml": "files:\n  - {path: etc/x, content: x}\n"},
		"two kinds of file":         {"a.yaml": "files:\n  - {path: /x, content: x, link: /y}\n"},
		"no kind of file":           {"a.yaml": "files:\n  - {path: /x}\n"},
		"a missing source":          {"a.yaml": "files:\n  - {path: /x, source: nope}\n"},
		"a mode that is not octal":  {"a.yaml": "files:\n  - {path: /x, content: x, mode: rwx}\n"},
		"a resource declared twice": {"a.yaml": "packages: {apt: [jq, \"jq=1.7\"]}\n"},
		"a unit in two states":      {"a.yaml": "services: {enabled: [x.service], masked: [x.service]}\n"},
		"an exec with no script":    {"a.yaml": "exec:\n  - {name: x}\n"},
		"two files of one name":     {"a.yaml": "", "other/a.yaml": ""},
	} {
		t.Run(name, func(t *testing.T) {
			e := newEnv(t)
			paths := []string{e.dir}
			for file, content := range files {
				if path := e.state(file, content); filepath.Dir(path) != e.dir {
					paths = append(paths, path)
				}
			}
			if _, err := Load(paths...); err == nil {
				t.Fatal("loaded")
			}
			// And nothing reaches the guest.
			if err := Run(t.Context(), e.guest, Options{Paths: paths}); err == nil {
				t.Fatal("ran")
			}
			if _, err := os.Stat(e.stateDir()); !errors.Is(err, os.ErrNotExist) {
				t.Fatalf("state files that do not load touched the guest: %v", err)
			}
		})
	}
	// A Windows guest's paths are absolute to a client on any OS.
	e := newEnv(t)
	e.state("w.yaml", "files:\n  - {path: 'C:\\tools\\x.txt', content: x}\n")
	if _, err := Load(e.dir); err != nil {
		t.Fatal(err)
	}
}

// One client converges a guest at a time: a second is refused while the
// first holds the lease, and has the guest once the first's lease runs out.
func TestLease(t *testing.T) {
	e := newEnv(t)
	e.state("a.yaml", "files:\n  - {path: /opt/a, content: a}\n")
	ctx := t.Context()
	first, err := open(ctx, e.guest, io.Discard)
	if err != nil {
		t.Fatal(err)
	}
	take := func(holder string, ttl time.Duration) error {
		return first.script(ctx, first.shell.acquire(first.host(first.dir), holder, ttl), nil, io.Discard, io.Discard)
	}
	renew := func(holder string, ttl time.Duration) error {
		return first.script(ctx, first.shell.renew(first.host(first.dir), holder, ttl), nil, io.Discard, io.Discard)
	}
	if err := take("first", 2*time.Second); err != nil {
		t.Fatal(err)
	}
	// A heartbeat keeps the lease past the timeout it was taken with.
	if err := renew("first", time.Minute); err != nil {
		t.Fatal(err)
	}
	time.Sleep(3 * time.Second)
	if _, err := e.run(); !errors.Is(err, ErrLeaseHeld) || !strings.Contains(err.Error(), "first is converging this guest") {
		t.Fatalf("a second client's run while the first holds the lease: %v", err)
	}
	if _, err := os.Stat(e.path("/opt/a")); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("a refused run changed the guest: %v", err)
	}
	// With no more heartbeats it runs out, and the next client has the
	// guest. The first one's next heartbeat finds that out.
	if err := renew("first", time.Second); err != nil {
		t.Fatal(err)
	}
	time.Sleep(2500 * time.Millisecond)
	if err := take("second", time.Minute); err != nil {
		t.Fatal(err)
	}
	if err := renew("first", time.Minute); !errors.Is(err, ErrLeaseHeld) || !strings.Contains(err.Error(), "went to second") {
		t.Fatalf("a heartbeat for a lease another client has: %v", err)
	}
}

// A run that can no longer reach its guest gives up once its lease has run
// out, since by then another client may have the guest.
func TestRunEndsWhenItsLeaseDoes(t *testing.T) {
	e := newEnv(t)
	s, err := open(t.Context(), e.guest, io.Discard)
	if err != nil {
		t.Fatal(err)
	}
	s.lease, s.holder = 2*time.Second, "lost"
	ctx, cancel := context.WithCancelCause(t.Context())
	done := make(chan struct{})
	go func() { defer close(done); s.heartbeat(ctx, cancel) }()
	e.guest.mu.Lock()
	e.guest.lost = true
	e.guest.mu.Unlock()
	select {
	case <-ctx.Done():
	case <-time.After(15 * time.Second):
		t.Fatal("the run outlived its lease")
	}
	<-done
	if cause := context.Cause(ctx); !errors.Is(cause, ErrLeaseHeld) || !strings.Contains(cause.Error(), "no heartbeat") {
		t.Fatalf("cause: %v", cause)
	}
}
