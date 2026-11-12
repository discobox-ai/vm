// Package e2e drives the real disco-vm binary through its whole lifecycle:
// install, run, exec, cp, stop, start, commit, create --from, rm.
// On the fake driver it is the neutral proof of everything above the driver
// seam, and it runs on every OS.
//
// $DISCO_VM_DRIVER runs it against a real driver instead. That needs a signed
// binary on macOS ($DISCO_VM_TEST_BINARY) and a persistent state root
// ($DISCO_VM_E2E_ROOT), where the base install is made once, as the
// committed VM e2e-base, and found by every later run. On hcs it also needs a
// Windows ISO ($DISCO_VM_E2E_ISO) and an elevated shell. The docker driver
// installs nothing: e2e-base is a fresh VM of its base, debian:trixie or the
// image $DISCO_VM_E2E_IMAGE names, committed, so its state root is optional.
// Nor does libkrun: its base is $DISCO_VM_LIBKRUN_BASE, or its default.
//
// A driver that does not report Capabilities.Commit (ADR 26-11-03-868) has
// no e2e-base: every VM is fresh, the cases that commit are skipped, and
// TestNoCommit checks that commit and --from refuse. The fake driver runs
// that way with $DISCO_VM_FAKE_NO_COMMIT set, and TestNoCommit runs it so on
// every fake run.
package e2e

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/discobox-ai/vm/pkg/guest"
	"github.com/discobox-ai/vm/pkg/machine"
)

var (
	binary string
	tg     target
)

// target is the driver the suite drives, and what its guest looks like.
type target struct {
	driver string
	// install is the arguments of the `disco-vm install` that makes
	// e2e-base, on a driver that installs; nil makes it a fresh VM.
	install []string
	// base is the driver's base ($DISCO_VM_BASE), which a fresh VM starts
	// from; empty is the driver's default.
	base string
	// dir is where the suite writes in the guest. The fake guest's is its
	// root, and a command there runs in it.
	dir string
	// root is a state root that outlives the run, for a driver whose install
	// is too slow to repeat.
	root string
	// slow says the install is too slow to repeat in every test, so root is
	// required.
	slow bool
}

// file is a guest path for the file API.
func (t target) file(p string) string {
	if t.dir == "" {
		return "/" + p
	}
	return t.dir + "/" + p
}

// cmd is the same path as a command in the guest sees it.
func (t target) cmd(p string) string {
	if t.dir == "" {
		return p
	}
	return t.dir + "/" + p
}

func targetFromEnv() target {
	switch driver := os.Getenv("DISCO_VM_DRIVER"); driver {
	case "", "fake":
		return target{driver: "fake", install: []string{}}
	case "vz":
		return target{
			driver:  "vz",
			install: []string{"--disk", "64GiB"},
			dir:     "/Users/Shared",
			root:    os.Getenv("DISCO_VM_E2E_ROOT"),
			slow:    true,
		}
	case "docker":
		image := os.Getenv("DISCO_VM_E2E_IMAGE")
		if image == "" {
			image = "debian:trixie"
		}
		return target{
			driver: "docker",
			base:   image,
			dir:    "/srv",
			root:   os.Getenv("DISCO_VM_E2E_ROOT"),
		}
	case "libkrun":
		// A fresh VM of $DISCO_VM_LIBKRUN_BASE (a guest image's reference, a
		// directory of its artifacts, or "default", the driver's default);
		// the runtime is $DISCO_VM_KRUN_RUNTIME's, or the pinned one, pulled.
		base := os.Getenv("DISCO_VM_LIBKRUN_BASE")
		if base == "default" {
			base = ""
		}
		return target{
			driver: "libkrun",
			base:   base,
			dir:    "/var/tmp",
			root:   os.Getenv("DISCO_VM_E2E_ROOT"),
		}
	case "hcs":
		iso := os.Getenv("DISCO_VM_E2E_ISO")
		if iso == "" {
			fmt.Fprintln(os.Stderr, "e2e: driver hcs needs $DISCO_VM_E2E_ISO, a Windows ISO to install from")
			os.Exit(2)
		}
		return target{
			driver:  "hcs",
			install: []string{"--media", iso, "--edition", "Windows 11 Pro", "--disk", "64GiB"},
			dir:     "C:/Users/Public",
			root:    os.Getenv("DISCO_VM_E2E_ROOT"),
			slow:    true,
		}
	default:
		panic("e2e: no guest layout for driver " + driver)
	}
}

func TestMain(m *testing.M) {
	tg = targetFromEnv()
	if tg.slow && tg.root == "" {
		fmt.Fprintf(os.Stderr, "e2e: driver %s needs $DISCO_VM_E2E_ROOT, a state root that keeps its base install between runs\n", tg.driver)
		os.Exit(2)
	}
	// A prebuilt binary lets the suite run where there is no Go toolchain,
	// such as a cross-compiled test binary copied into a VM.
	if binary = os.Getenv("DISCO_VM_TEST_BINARY"); binary != "" {
		os.Exit(m.Run())
	}
	dir, err := os.MkdirTemp("", "disco-vm-e2e")
	if err != nil {
		panic(err)
	}
	binary = filepath.Join(dir, "disco-vm")
	if runtime.GOOS == "windows" {
		binary += ".exe"
	}
	build := exec.CommandContext(context.Background(), "go", "build", "-o", binary, "../../cmd/disco-vm")
	build.Stdout, build.Stderr = os.Stdout, os.Stderr
	if err := build.Run(); err != nil {
		panic(err)
	}
	code := m.Run()
	_ = os.RemoveAll(dir)
	os.Exit(code)
}

type env struct {
	t    *testing.T
	root string
	// commits is whether the driver reports Capabilities.Commit; where it
	// does not, there is no e2e-base, and every VM is fresh.
	commits bool
	// extra is added to every command's environment.
	extra []string
	// stdin is every command's standard input; empty is none.
	stdin string
}

// poolEnv names the pool every command runs in, "e2e" by default. A
// driver whose provider is a service (docker) keeps everything there, not
// in the root, and the suite removes all of a pool's VMs,
// so it never runs in the pool of someone's own.
const poolEnv = "DISCO_VM_E2E_POOL"

func (e env) run(args ...string) (string, int) {
	e.t.Helper()
	// Not t.Context(): cleanups call run too, after that is canceled.
	cmd := exec.CommandContext(context.Background(), binary, args...)
	pool := os.Getenv(poolEnv)
	if pool == "" {
		pool = "e2e"
	}
	cmd.Env = append(os.Environ(), "DISCO_VM_ROOT="+e.root, "DISCO_VM_DRIVER="+tg.driver, "DISCO_VM_POOL="+pool)
	if tg.base != "" {
		cmd.Env = append(cmd.Env, "DISCO_VM_BASE="+tg.base)
	}
	cmd.Env = append(cmd.Env, e.extra...)
	var out bytes.Buffer
	cmd.Stdout, cmd.Stderr = &out, &out
	if e.stdin != "" {
		cmd.Stdin = strings.NewReader(e.stdin)
	}
	err := cmd.Run()
	code := 0
	if exitErr, ok := errors.AsType[*exec.ExitError](err); ok {
		code = exitErr.ExitCode()
	} else if err != nil {
		e.t.Fatalf("disco-vm %v: %v", args, err)
	}
	return out.String(), code
}

func (e env) ok(args ...string) string {
	e.t.Helper()
	out, code := e.run(args...)
	if code != 0 {
		e.t.Fatalf("disco-vm %s: exit %d\n%s", strings.Join(args, " "), code, out)
	}
	return out
}

func mustContain(t *testing.T, out string, wants ...string) {
	t.Helper()
	for _, want := range wants {
		if !strings.Contains(out, want) {
			t.Fatalf("output lacks %q:\n%s", want, out)
		}
	}
}

// newEnv is a state root for one test, with extra in every command's
// environment, holding the committed VM e2e-base: a fresh one on the fake
// driver, and on a real driver the persistent one, where e2e-base is made
// once. On a driver that does not commit there is none.
func newEnv(t *testing.T, extra ...string) env {
	t.Helper()
	e := env{t: t, root: tg.root, extra: extra}
	if tg.root == "" {
		e.root = t.TempDir()
		if tg.driver != "fake" {
			// A real driver may hold VMs outside the state root (docker
			// images), which a root thrown away would leave behind.
			t.Cleanup(e.release)
		}
	} else {
		// Whatever a failed run left, newest first, since a VM created
		// from another may need its disk.
		e.run("rm", "-f", "dev", "dev2", "svc", "snap3", "snap2", "snap", "app-v1", "solo", "fenced", "vol-s", "vol-b", "vol-a", "up", "up-next")
	}
	e.commits = strings.Contains(e.ok("info"), `"commit": true`)
	if !e.commits {
		return e
	}
	if _, code := e.run("inspect", "e2e-base"); code != 0 {
		if tg.install != nil {
			e.ok(append([]string{"install", "e2e-base"}, tg.install...)...)
		} else {
			e.ok("run", "e2e-base")
			e.ok("stop", "e2e-base")
			e.ok("commit", "e2e-base")
		}
	}
	return e
}

// base is the arguments that create a VM of the suite's start: --from
// e2e-base, or none, a fresh VM, on a driver that does not commit.
func (e env) base() []string {
	if !e.commits {
		return nil
	}
	return []string{"--from", "e2e-base"}
}

// release removes every VM in the state root, a VM created from another
// before that one.
func (e env) release() {
	for range 3 {
		var names []string
		for _, line := range strings.Split(strings.TrimSpace(e.ok("ls", "-a")), "\n")[1:] {
			if f := strings.Fields(line); len(f) > 1 {
				names = append(names, f[1])
			}
		}
		if len(names) == 0 {
			return
		}
		e.run(append([]string{"rm", "-f"}, names...)...)
	}
}

// catArgv reads a guest file under the target's directory.
func catArgv(path string) []string {
	if runtime.GOOS == "windows" {
		return []string{"cmd", "/c", "type", strings.ReplaceAll(tg.cmd(path), "/", `\`)}
	}
	return []string{"cat", tg.cmd(path)}
}

// TestLifecycle shapes a VM and commits it as the app, then runs VMs of it,
// execs, copies both ways, stops, starts, and removes them. On a driver
// that does not commit, the VMs are fresh and nothing is shaped.
func TestLifecycle(t *testing.T) {
	e := newEnv(t)
	if e.commits {
		mustContain(t, e.ok("ls", "-a"), "e2e-base")
	}
	exitArgv := []string{"/bin/sh", "-c", "exit 7"}
	writeArgv := []string{"/bin/sh", "-c", "echo hi > " + tg.cmd("greeting.txt")}
	if runtime.GOOS == "windows" {
		exitArgv = []string{"cmd", "/c", "exit 7"}
		writeArgv = []string{"cmd", "/c", "echo hi> " + strings.ReplaceAll(tg.cmd("greeting.txt"), "/", `\`)}
	}

	// Shape a VM of the base, and commit it as the app.
	local := t.TempDir()
	if err := os.WriteFile(filepath.Join(local, "tool.txt"), []byte("tool v1"), 0o644); err != nil {
		t.Fatal(err)
	}
	var app []string
	if e.commits {
		e.ok(append([]string{"run", "app-v1"}, e.base()...)...)
		e.ok("cp", filepath.Join(local, "tool.txt"), "app-v1:"+tg.file("opt/app"))
		e.ok(append([]string{"exec", "app-v1"}, writeArgv...)...)
		e.ok("stop", "app-v1")
		e.ok("commit", "app-v1")
		mustContain(t, e.ok("ls", "-a"), "app-v1", "e2e-base")
		app = []string{"--from", "app-v1"}
	}

	// Run a VM of it and look inside.
	e.ok(append([]string{"run", "dev"}, app...)...)
	mustContain(t, e.ok("ls"), "dev", "running")
	// ps is ls by its older name.
	mustContain(t, e.ok("ps"), "dev", "running")
	if e.commits {
		mustContain(t, e.ok("ls"), "app-v1")
		mustContain(t, e.ok(append([]string{"exec", "dev"}, catArgv("greeting.txt")...)...), "hi")
		mustContain(t, e.ok(append([]string{"exec", "dev"}, catArgv("opt/app/tool.txt")...)...), "tool v1")
	} else {
		e.ok("cp", filepath.Join(local, "tool.txt"), "dev:"+tg.file("opt/app"))
	}
	if _, code := e.run(append([]string{"exec", "dev"}, exitArgv...)...); code != 7 {
		t.Fatalf("exec exit code = %d, want 7", code)
	}
	mustContain(t, e.ok("inspect", "dev"), `"state": "running"`)
	if tg.driver == "fake" {
		mustContain(t, e.ok("inspect", "dev"), `"fake": true`)
		if e.commits {
			mustContain(t, e.ok("inspect", "dev"), `"from": "app-v1"`)
		}
	}
	if out, code := e.run("create", "dev3", "--from", ""); code == 0 || !strings.Contains(out, "leave it out for a fresh VM") {
		t.Fatalf("an empty --from (exit %d):\n%s", code, out)
	}
	if out, code := e.run("create", "dev3", "--from", "dev"); code == 0 || !strings.Contains(out, map[bool]string{true: "stop and commit it", false: "cannot create a VM from"}[e.commits]) {
		t.Fatalf("a VM created from a running one (exit %d):\n%s", code, out)
	}

	// Copy both ways.
	note := filepath.Join(t.TempDir(), "note.txt")
	if err := os.WriteFile(note, []byte("from host"), 0o644); err != nil {
		t.Fatal(err)
	}
	e.ok("cp", note, "dev:"+tg.file("drop"))
	mustContain(t, e.ok(append([]string{"exec", "dev"}, catArgv("drop/note.txt")...)...), "from host")
	outDir := t.TempDir()
	e.ok("cp", "dev:"+tg.file("opt/app"), outDir)
	if data, err := os.ReadFile(filepath.Join(outDir, "app", "tool.txt")); err != nil || string(data) != "tool v1" {
		t.Fatalf("cp out: %q %v", data, err)
	}

	// A VM's writes are its own: a second VM of the same committed one does
	// not see them, and they survive the first's restart.
	e.ok(append([]string{"run", "dev2"}, app...)...)
	if _, code := e.run(append([]string{"exec", "dev2"}, catArgv("drop/note.txt")...)...); code == 0 {
		t.Fatal("a second VM saw the first one's write")
	}
	e.ok("stop", "dev")
	if strings.Contains(e.ok("ls"), "dev ") {
		t.Fatal("dev is listed as running after stop")
	}
	mustContain(t, e.ok("ls", "-a"), "stopped")
	e.ok("start", "dev")
	mustContain(t, e.ok(append([]string{"exec", "dev"}, catArgv("drop/note.txt")...)...), "from host")

	e.ok("rm", "-f", "dev", "dev2")
	if e.commits {
		e.ok("rm", "app-v1")
	}
	if out := e.ok("ls", "-a"); strings.Contains(out, "dev") || strings.Contains(out, "app-v1") {
		t.Fatalf("removed VMs are still listed:\n%s", out)
	}

	// run takes a name and nothing else; a command goes through exec.
	if _, code := e.run(append(append(append([]string{"run"}, e.base()...), "cmd"), catArgv("greeting.txt")...)...); code == 0 {
		t.Fatal("run took a command")
	}
	if _, code := e.run(append([]string{"run", "--rm"}, e.base()...)...); code == 0 {
		t.Fatal("run took --rm")
	}
}

// TestCommit freezes a stopped VM under its name and creates a VM from it,
// which is committed in turn: each keeps what was written before it, and
// the second outlives the first's removal. A driver whose second still
// needs the first's disk (hcs) refuses the removal until the second is gone.
func TestCommit(t *testing.T) {
	e := newEnv(t)
	if !e.commits {
		t.Skipf("%s does not commit (TestNoCommit)", tg.driver)
	}
	dir := t.TempDir()
	// write copies a file named name into the VM's snap directory.
	write := func(inst, name string) {
		t.Helper()
		local := filepath.Join(dir, name)
		if err := os.WriteFile(local, []byte(name), 0o644); err != nil {
			t.Fatal(err)
		}
		e.ok("cp", local, inst+":"+tg.file("snap"))
	}
	read := func(inst, name string) {
		t.Helper()
		mustContain(t, e.ok(append([]string{"exec", inst}, catArgv("snap/"+name)...)...), name)
	}

	e.ok("run", "snap", "--from", "e2e-base")
	write("snap", "first")
	if out, code := e.run("commit", "snap"); code == 0 || !strings.Contains(out, "stop it first") {
		t.Fatalf("a running VM was committed (exit %d):\n%s", code, out)
	}
	e.ok("stop", "snap")
	e.ok("commit", "snap")
	mustContain(t, e.ok("ls", "-a"), "snap")
	if out, code := e.run("create", "snap", "--from", "e2e-base"); code == 0 || !strings.Contains(out, "already exists") {
		t.Fatalf("a VM named as a committed one (exit %d):\n%s", code, out)
	}

	e.ok("run", "snap2", "--from", "snap")
	read("snap2", "first")
	write("snap2", "second")
	e.ok("stop", "snap2")
	e.ok("commit", "snap2")

	// The first goes, or is kept for the second; either way the second's
	// VMs run with both writes.
	out, code := e.run("rm", "snap")
	kept := code != 0
	if kept && !strings.Contains(out, "still needs its disk") {
		t.Fatalf("rm snap: exit %d\n%s", code, out)
	}
	e.ok("run", "snap3", "--from", "snap2")
	read("snap3", "first")
	read("snap3", "second")

	e.ok("rm", "-f", "snap3")
	e.ok("rm", "snap2")
	if kept {
		e.ok("rm", "snap")
	}
	if out := e.ok("ls", "-a"); strings.Contains(out, "snap") {
		t.Fatalf("a removed VM is still listed:\n%s", out)
	}
}

func TestInfo(t *testing.T) {
	e := env{t: t, root: t.TempDir()}
	info := e.ok("info")
	mustContain(t, info, "driver:   "+tg.driver, "check:    ok", "fake")
	if tg.driver == "fake" && strings.Contains(info, `"install": [`) {
		// A driver with no display refuses a window rather than ignoring
		// it, where it installs at all.
		out, code := e.run("install", "gui", "--gui")
		if code == 0 || !strings.Contains(out, "no display") {
			t.Fatalf("install --gui on the fake driver: exit %d\n%s", code, out)
		}
	}
}

// A key the driver issues runs the pool's machines from the command line,
// cannot issue another, and is refused once it is revoked. The fake driver is
// the one that issues keys and reads them from the environment.
func TestKeys(t *testing.T) {
	if tg.driver != "fake" {
		t.Skipf("%s issues no keys the suite can use", tg.driver)
	}
	e := newEnv(t)
	if out, code := e.run("key", "issue"); code == 0 || !strings.Contains(out, "ttl") {
		t.Fatalf("key issue with no --ttl: exit %d\n%s", code, out)
	}
	var key struct {
		Secret, ID, Name, Scope string
		Issued                  bool
		Expires                 time.Time
	}
	if err := json.Unmarshal([]byte(e.ok("key", "issue", "--name", "pool", "--ttl", "1h")), &key); err != nil {
		t.Fatal(err)
	}
	if !key.Issued || key.Secret == "" || key.Name != "pool" || key.Scope != "pool" || time.Until(key.Expires) > time.Hour {
		t.Fatalf("issued %+v", key)
	}
	listed := e.ok("key", "ls", "pool")
	mustContain(t, listed, key.ID, "pool", "pool")
	if strings.Contains(listed, key.Secret) {
		t.Fatal("key ls shows the secret")
	}

	keyed := env{t: t, root: e.root, commits: e.commits, extra: []string{"DISCO_VM_FAKE_KEY=" + key.Secret}}
	if e.commits {
		mustContain(t, keyed.ok("ls", "-a"), "e2e-base")
	}
	keyed.ok(append([]string{"run", "solo"}, e.base()...)...)
	t.Cleanup(func() { e.run("rm", "-f", "solo") })
	mustContain(t, keyed.ok("ls", "-a"), "solo")
	note := filepath.Join(t.TempDir(), "keyed.txt")
	if err := os.WriteFile(note, []byte("with a key"), 0o644); err != nil {
		t.Fatal(err)
	}
	keyed.ok("cp", note, "solo:"+tg.file("drop"))
	mustContain(t, keyed.ok(append([]string{"exec", "solo"}, catArgv("drop/keyed.txt")...)...), "with a key")
	if runtime.GOOS != "windows" {
		// Neither the shim nor the guest is handed the key.
		if out := keyed.ok("exec", "solo", "env"); strings.Contains(out, key.Secret) {
			t.Fatalf("the guest's environment holds the key:\n%s", out)
		}
	}
	keyed.ok("stop", "solo")
	if out, code := keyed.run("key", "issue", "--ttl", "1h"); code == 0 || !strings.Contains(out, "cannot issue") {
		t.Fatalf("an issued key issuing: exit %d\n%s", code, out)
	}
	keyed.ok("rm", "-f", "solo")

	e.ok("key", "revoke", key.ID)
	if out := e.ok("key", "ls", "--json"); strings.TrimSpace(out) != "[]" {
		t.Fatalf("key ls after revoking: %s", out)
	}
	if out, code := keyed.run("ps"); code == 0 || !strings.Contains(out, "refused") {
		t.Fatalf("a revoked key: exit %d\n%s", code, out)
	}
}

func TestDrivers(t *testing.T) {
	e := env{t: t, root: t.TempDir()}
	out := e.ok("drivers")
	mustContain(t, out, "DRIVER", "SELECTED", "boxd", "docker", "fake")
	if !driverSelected(out, tg.driver) {
		t.Fatalf("selected driver is not %s:\n%s", tg.driver, out)
	}

	// The command-line flag takes precedence over DISCO_VM_DRIVER, which the
	// harness sets to tg.driver, so the flag names a different driver.
	other := "docker"
	if tg.driver == other {
		other = "fake"
	}
	out = e.ok("--driver", other, "drivers")
	if !driverSelected(out, other) {
		t.Fatalf("--driver %s did not select %s:\n%s", other, other, out)
	}

	// An unknown driver is refused.
	if out, code := e.run("--driver", "nope", "drivers"); code == 0 || !strings.Contains(out, `no driver "nope"`) {
		t.Fatalf("--driver nope: exit %d\n%s", code, out)
	}
}

// TestDriverConfig configures the fake driver, on every target: --help=DRIVER
// lists its fields; a field's flag, its environment variable, and
// --driver-config each reach the driver; a flag beats the variable; a
// configuration the driver does not take, another driver's flag, and a flag
// beside --driver-config are refused before anything is made; and a VM's
// shim is given the configuration of the command that started it.
func TestDriverConfig(t *testing.T) {
	// Empty is unset: the suite may run with the fake driver's variables set
	// (task test's second run sets DISCO_VM_FAKE_NO_COMMIT).
	unset := []string{"DISCO_VM_FAKE_NO_COMMIT=", "DISCO_VM_AGENT=", "DISCO_VM_FAKE_MAX_RUNNING="}
	e := env{t: t, root: t.TempDir(), extra: unset}
	fake := func(args ...string) []string { return append([]string{"--driver", "fake"}, args...) }

	help := e.ok("--help=fake")
	mustContain(t, help, "The fake driver", "--fake-no-commit", "key: noCommit", "env: $DISCO_VM_FAKE_NO_COMMIT", "default: false",
		"--fake-agent string", "privileged", "Base ($DISCO_VM_BASE", "default: scratch", "--driver-config")
	mustContain(t, e.ok("run", "--help=fake"), "--fake-no-commit")
	mustContain(t, e.ok("--help"), "--help=DRIVER")
	if strings.Contains(e.ok("--help"), "--fake-no-commit") {
		t.Fatal("a driver's flags are in every command's help")
	}
	mustContain(t, e.ok("drivers"), "Guests as disco-vm agent processes")
	if out, _ := e.run("--help=nope"); !strings.Contains(out, `no driver "nope"`) {
		t.Fatalf("--help=nope:\n%s", out)
	}

	config := filepath.Join(t.TempDir(), "fake.json")
	if err := os.WriteFile(config, []byte(`{"noCommit": true}`), 0o600); err != nil {
		t.Fatal(err)
	}
	for _, args := range [][]string{
		fake("--fake-no-commit", "info"),
		fake("--driver-config", `{"noCommit":true}`, "info"),
		fake("--driver-config", "@"+config, "info"),
	} {
		out := e.ok(args...)
		mustContain(t, out, `"commit": false`, `config:   {"noCommit":true}`)
		if strings.Contains(out, "warning") {
			t.Fatalf("disco-vm %s warned of nothing set:\n%s", strings.Join(args, " "), out)
		}
	}
	withEnv := e
	withEnv.extra = append(slices.Clone(unset), "DISCO_VM_FAKE_NO_COMMIT=1")
	mustContain(t, withEnv.ok(fake("info")...), `"commit": false`)
	mustContain(t, withEnv.ok(fake("--fake-no-commit=false", "info")...), `"commit": true`)
	mustContain(t, e.ok(fake("info")...), `"commit": true`, `config:   {"noCommit":false}`)
	// A whole configuration reads no variable, and names one it ignores
	// that says otherwise, never with its value.
	mustContain(t, withEnv.ok(fake("--driver-config", "{}", "info")...), `"commit": true`,
		"warning: $DISCO_VM_FAKE_NO_COMMIT is set and not read")

	// @FILE is read at most 1 MiB, and no error quotes it.
	const secret = "s3cr3t-in-a-file"
	notJSON := filepath.Join(t.TempDir(), "id_ed25519")
	if err := os.WriteFile(notJSON, []byte("-----BEGIN "+secret+"-----\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	big := filepath.Join(t.TempDir(), "big.json")
	if err := os.WriteFile(big, []byte(`{"agent":"/`+strings.Repeat("a", 1<<20)+`"}`), 0o600); err != nil {
		t.Fatal(err)
	}
	for file, want := range map[string]string{notJSON: "not JSON", big: "is over 1048576 bytes"} {
		out, code := e.run(fake("--driver-config", "@"+file, "ls")...)
		if code == 0 || !strings.Contains(out, want) || strings.Contains(out, secret) {
			t.Errorf("--driver-config @%s: exit %d, want %q and nothing of the file:\n%s", filepath.Base(file), code, want, out)
		}
	}

	for _, c := range []struct {
		args []string
		want string
	}{
		{fake("--driver-config", `{"noComit":true}`, "ls"), `unknown field "noComit" (the driver takes agent, noCommit, maxRunning)`},
		{fake("--driver-config", `{"noCommit":"yes"}`, "ls"), "noCommit: not a bool"},
		{fake("--fake-agent", "disco-vm", "ls"), "agent: not an absolute path"},
		{fake("--driver-config", `not json`, "ls"), "not JSON"},
		{fake("--fake-no-commit=maybe", "ls"), "noCommit"},
		{fake("--docker-host", "unix:///x", "ls"), "--docker-host configures the docker driver, and the driver is fake"},
		{fake("--driver-config", "{}", "--fake-no-commit", "ls"), "--driver-config cannot be combined with --fake-no-commit"},
		{[]string{"--driver", "docker", "--docker-host", "ssh://h", "ls"}, "host: only unix:// and tcp://"},
		{[]string{"--driver", "boxd", "--boxd-token-url", "http://example.com/t", "ls"}, "tokenUrl: a URL the key is sent to is https"},
	} {
		if out, code := e.run(c.args...); code == 0 || !strings.Contains(out, c.want) {
			t.Errorf("disco-vm %s: exit %d, want %q:\n%s", strings.Join(c.args, " "), code, c.want, out)
		}
	}

	// A secret is written, never read back: its variables are named, and
	// its value is nowhere a command prints, nor taken in a configuration.
	const key = "bxd_written-never-read"
	keyed := e
	keyed.extra = append(slices.Clone(unset), "BOXD_API_KEY="+key)
	for _, args := range [][]string{
		{"--help=boxd"},
		{"drivers"},
		{"--driver", "boxd", "--driver-config", `{"apiKey":"` + key + `"}`, "ls"},
		{"--driver", "boxd", "--driver-config", `{"region":"New York"}`, "ls"},
	} {
		out, _ := keyed.run(args...)
		if strings.Contains(out, key) {
			t.Errorf("disco-vm %s printed the key:\n%s", strings.Join(args, " "), out)
		}
	}
	mustContain(t, keyed.ok("--help=boxd"), "Key (secret; $BOXD_API_KEY or $BOXD_TOKEN)")
	if out, code := keyed.run("--driver", "boxd", "--driver-config", `{"apiKey":"`+key+`"}`, "ls"); code == 0 || !strings.Contains(out, "apiKey is a secret") {
		t.Errorf("a key in the configuration: exit %d\n%s", code, out)
	}

	// Only a boot runs the agent, and a started VM boots in its shim: a
	// missing one fails the start only if the shim was given it.
	missing := filepath.Join(t.TempDir(), "no-agent")
	t.Cleanup(func() { e.run(fake("rm", "-f", "vm")...) })
	if out, code := e.run(fake("--fake-agent", missing, "run", "vm")...); code == 0 || !strings.Contains(out, missing) {
		t.Fatalf("a VM started with a missing agent: exit %d\n%s", code, out)
	}
	e.run(fake("rm", "-f", "vm")...)
	e.ok(fake("--fake-no-commit", "run", "vm")...)

	// The shim was given the configuration in a file of the VM's, readable
	// by its owner alone, and nothing of it on its command line.
	files, err := filepath.Glob(filepath.Join(e.root, "instances", "*", "driver-config.json"))
	if err != nil || len(files) != 1 {
		t.Fatalf("the shim's configuration files: %v, %v", files, err)
	}
	if data, err := os.ReadFile(files[0]); err != nil || string(data) != `{"noCommit":true}` {
		t.Fatalf("the shim's configuration: %q, %v", data, err)
	}
	if runtime.GOOS == "linux" {
		var state struct {
			PID int `json:"pid"`
		}
		data, err := os.ReadFile(filepath.Join(filepath.Dir(files[0]), "shim.json"))
		if err == nil {
			err = json.Unmarshal(data, &state)
		}
		if err != nil {
			t.Fatal(err)
		}
		cmdline, err := os.ReadFile(fmt.Sprintf("/proc/%d/cmdline", state.PID))
		if err != nil {
			t.Fatal(err)
		}
		args := strings.ReplaceAll(string(cmdline), "\x00", " ")
		if !strings.Contains(args, "--driver-config @"+files[0]) || strings.Contains(args, "noCommit") {
			t.Fatalf("the shim's command line: %s", args)
		}
	}
}

func driverSelected(out, name string) bool {
	for line := range strings.SplitSeq(out, "\n") {
		fields := strings.Fields(line)
		if len(fields) >= 2 && fields[0] == name && fields[1] == "*" {
			return true
		}
	}
	return false
}

// Every instance has one service. Its URL is served while it runs, and the
// guest finds where to listen in its service file. On the fake driver the
// guest is a host process, so the service is this test's server.
func TestURL(t *testing.T) {
	e := newEnv(t)
	e.ok(append([]string{"create", "svc"}, e.base()...)...)
	t.Cleanup(func() { e.run("rm", "-f", "svc") })
	if out, code := e.run("url", "svc"); code == 0 || !strings.Contains(out, "not running") {
		t.Fatalf("url of a stopped instance: exit %d\n%s", code, out)
	}
	if out, code := e.run("url", "--private", "svc"); code == 0 || !strings.Contains(out, "not running") {
		t.Fatalf("url --private of a stopped instance: exit %d\n%s", code, out)
	}
	e.ok("start", "svc")
	// A guest port is reached through the instance's shim: the agent's,
	// whose health an HTTP request reads. docker has no socket into its
	// guest.
	asking := e
	asking.stdin = "GET /v1/health HTTP/1.0\r\nHost: guest\r\n\r\n"
	answer, code := asking.run("dial", "svc", strconv.FormatUint(uint64(guest.AgentPort), 10))
	switch tg.driver {
	case "docker":
		if code == 0 || !strings.Contains(answer, "not supported") {
			t.Fatalf("dial svc on docker: exit %d\n%s", code, answer)
		}
	default:
		if code != 0 || !strings.Contains(answer, " 200 ") {
			t.Fatalf("dial svc %d: exit %d\n%s", guest.AgentPort, code, answer)
		}
	}
	url := strings.TrimSpace(e.ok("url", "svc"))
	if !strings.HasPrefix(url, "http://127.0.0.1:") {
		t.Fatalf("url svc = %q", url)
	}
	// A fake guest's peers share its loopback, a container's dial its
	// address on its network, and a vz or hcs guest has no peers.
	private, code := e.run("url", "--private", "svc")
	private = strings.TrimSpace(private)
	switch tg.driver {
	case "fake":
		if code != 0 || private != url {
			t.Fatalf("url --private svc = %q, exit %d; want the URL, %s", private, code, url)
		}
	case "docker":
		if code != 0 || !strings.HasPrefix(private, "http://") || !strings.HasSuffix(private, ":7302") || private == url {
			t.Fatalf("url --private svc = %q, exit %d; want the container's address at 7302", private, code)
		}
	default:
		if code == 0 {
			t.Fatalf("url --private svc on %s = %q, want none", tg.driver, private)
		}
	}

	var guestOS struct {
		OS string `json:"guestOS"`
	}
	if err := json.Unmarshal([]byte(e.ok("inspect", "svc")), &guestOS); err != nil {
		t.Fatal(err)
	}
	outDir := t.TempDir()
	e.ok("cp", "svc:"+guest.ServiceFile(guestOS.OS), outDir)
	data, err := os.ReadFile(filepath.Join(outDir, "service"))
	if err != nil {
		t.Fatal(err)
	}
	address := strings.TrimSpace(string(data))
	want := map[string]string{"vz": "vsock:7302", "hcs": "vsock:7302", "docker": "tcp:0.0.0.0:7302"}[tg.driver]
	if want != "" && address != want {
		t.Fatalf("service file says %q, want %q", address, want)
	}
	if tg.driver == "fake" {
		l, err := guest.Listen(address)
		if err != nil {
			t.Fatalf("listen on the service file's %q: %v", address, err)
		}
		server := &http.Server{
			Handler:           http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { fmt.Fprint(w, "hello from the service") }),
			ReadHeaderTimeout: 10 * time.Second,
		}
		go func() { _ = server.Serve(l) }()
		t.Cleanup(func() { _ = server.Close() })
		req, err := http.NewRequestWithContext(t.Context(), http.MethodGet, url, nil)
		if err != nil {
			t.Fatal(err)
		}
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		body, _ := io.ReadAll(resp.Body)
		_ = resp.Body.Close()
		if string(body) != "hello from the service" {
			t.Fatalf("GET %s: %s %q", url, resp.Status, body)
		}
	}
	e.ok("rm", "-f", "svc")
}

// TestEgress: none takes no other entry beside it, and a fence the driver
// does not hold is refused and makes nothing: vz holds none alone, as an
// instance with no network device, which inspect reports; the other drivers
// e2e runs hold no fence.
func TestEgress(t *testing.T) {
	e := newEnv(t)
	t.Cleanup(func() { e.run("rm", "-f", "fenced") })
	fenced := append([]string{"create", "fenced"}, e.base()...)
	if out, code := e.run(append(fenced, "--egress", "none,example.com")...); code == 0 || !strings.Contains(out, "takes no other entry") {
		t.Fatalf("create --egress none,example.com: exit %d\n%s", code, out)
	}
	for _, empty := range [][]string{{"--egress", ""}, {"--egress", "", "--egress", "example.com"}, {"--egress", "example.com,"}} {
		args := append(slices.Clone(fenced), empty...)
		if out, code := e.run(args...); code == 0 || !strings.Contains(out, "no empty entry") {
			t.Fatalf("create %q: exit %d\n%s", empty, code, out)
		}
	}
	if out, code := e.run(append(slices.Clone(fenced), "--egress", "example.com")...); code == 0 || !strings.Contains(out, "cannot fence") {
		t.Fatalf("create --egress example.com on %s: exit %d\n%s", tg.driver, code, out)
	}
	if tg.driver == "vz" || tg.driver == "libkrun" {
		e.ok(append(slices.Clone(fenced), "--egress", "none")...)
		var fenced struct {
			Egress *struct{} `json:"egress"`
		}
		if err := json.Unmarshal([]byte(e.ok("inspect", "fenced")), &fenced); err != nil || fenced.Egress == nil {
			t.Fatalf("an instance with no network device reports no fence: %v", err)
		}
		e.ok("rm", "fenced")
		return
	}
	if out, code := e.run(append(slices.Clone(fenced), "--egress", "none")...); code == 0 || !strings.Contains(out, "cannot fence") {
		t.Fatalf("create --egress none on %s: exit %d\n%s", tg.driver, code, out)
	}
	if out, code := e.run("inspect", "fenced"); code == 0 {
		t.Fatalf("a refused create made an instance:\n%s", out)
	}
}

// TestVolumes gives instances a data volume and a cache volume: the data is
// kept across a stop and never in a committed VM; a cache saved from a stopped
// instance, and not a running one, is what the next instance of its key
// starts from; a second save from it is refused once the key has moved,
// unless forced; and a key removed lists no more. A driver that attaches no
// volume refuses both.
func TestVolumes(t *testing.T) {
	e := newEnv(t)
	t.Cleanup(func() {
		e.run("rm", "-f", "vol-s", "vol-b", "vol-a")
		e.run("cache", "rm", "e2e-cache")
	})
	guestOS := map[string]machine.OS{"fake": machine.OS(runtime.GOOS), "docker": machine.Linux, "libkrun": machine.Linux, "vz": machine.Darwin, "hcs": machine.Windows}[tg.driver]
	data, cache := machine.VolumePath(guestOS, machine.VolumeData), machine.VolumePath(guestOS, machine.VolumeCache)
	if tg.driver != "fake" && tg.driver != "docker" {
		for _, flags := range [][]string{{"--data", "1GiB"}, {"--cache", "e2e-cache"}} {
			args := append(append([]string{"create", "vol-a"}, e.base()...), flags...)
			if out, code := e.run(args...); code == 0 || !strings.Contains(out, "cannot attach") {
				t.Fatalf("create %q on %s: exit %d\n%s", flags, tg.driver, code, out)
			}
		}
		return
	}
	if out, code := e.run(append(append([]string{"create", "vol-a"}, e.base()...), "--cache-size", "1GiB")...); code == 0 || !strings.Contains(out, "needs --cache") {
		t.Fatalf("create --cache-size with no --cache: exit %d\n%s", code, out)
	}
	local := t.TempDir()
	note := filepath.Join(local, "note.txt")
	write := func(content string) {
		t.Helper()
		if err := os.WriteFile(note, []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	read := func(inst, path string) string {
		t.Helper()
		out := t.TempDir()
		e.ok("cp", inst+":"+path+"/note.txt", out)
		got, err := os.ReadFile(filepath.Join(out, "note.txt"))
		if err != nil {
			t.Fatal(err)
		}
		return string(got)
	}

	e.ok(append(append([]string{"run", "vol-a"}, e.base()...), "--data", "1GiB", "--cache", "e2e-cache")...)
	write("data")
	e.ok("cp", note, "vol-a:"+data)
	write("cache a")
	e.ok("cp", note, "vol-a:"+cache)
	if out, code := e.run("cache", "save", "vol-a"); code == 0 || !strings.Contains(out, "stop it first") {
		t.Fatalf("cache save of a running instance: exit %d\n%s", code, out)
	}
	e.ok("stop", "vol-a")
	e.ok("start", "vol-a")
	if got := read("vol-a", data); got != "data" {
		t.Fatalf("the data volume reads %q after a stop and a start", got)
	}
	e.ok("stop", "vol-a")
	gen := strings.TrimSpace(e.ok("cache", "save", "vol-a"))
	mustContain(t, e.ok("cache", "ls"), "e2e-cache", gen)
	if out, code := e.run("cache", "save", "vol-a"); code == 0 || !strings.Contains(out, "--force") {
		t.Fatalf("a second guarded save: exit %d\n%s", code, out)
	}

	e.ok(append(append([]string{"run", "vol-b"}, e.base()...), "--cache", "e2e-cache")...)
	if got := read("vol-b", cache); got != "cache a" {
		t.Fatalf("an instance of the saved key reads %q", got)
	}
	var inspected struct {
		Cache *machine.CacheVolume `json:"cache"`
	}
	if err := json.Unmarshal([]byte(e.ok("inspect", "vol-b")), &inspected); err != nil || inspected.Cache == nil || inspected.Cache.Generation != gen {
		t.Fatalf("inspect vol-b: cache %+v, %v; want generation %s", inspected.Cache, err, gen)
	}
	write("cache b")
	e.ok("cp", note, "vol-b:"+cache)
	e.ok("stop", "vol-b")
	forced := strings.TrimSpace(e.ok("cache", "save", "--force", "vol-a"))
	mustContain(t, e.ok("cache", "ls"), forced)

	if e.commits {
		e.ok("commit", "vol-a")
		e.ok("run", "vol-s", "--from", "vol-a", "--data", "0")
		if out, code := e.run("cp", "vol-s:"+data+"/note.txt", t.TempDir()); code == 0 {
			t.Fatalf("a committed VM carried its data volume:\n%s", out)
		}
	}
	e.ok("cache", "rm", "e2e-cache")
	if out := e.ok("cache", "ls"); strings.Contains(out, "e2e-cache") {
		t.Fatalf("a removed key is listed:\n%s", out)
	}
}

// TestUpgrade gives a stopped VM a new root, the root of another committed
// VM, or a fresh one on a driver that does not commit: it keeps its name,
// its data volume, and its cache clone, and loses what it wrote to its old
// root. It is refused while the VM runs; from a VM there is not, or from
// any where the driver does not commit; and on a driver that attaches no
// data volume.
func TestUpgrade(t *testing.T) {
	e := newEnv(t)
	t.Cleanup(func() {
		e.run("rm", "-f", "up", "up-next")
		e.run("cache", "rm", "e2e-up")
	})
	if tg.driver != "fake" && tg.driver != "docker" {
		e.ok(append([]string{"create", "up"}, e.base()...)...)
		if out, code := e.run("upgrade", "up"); code == 0 || !strings.Contains(out, "attaches no data volume") {
			t.Fatalf("upgrade on %s: exit %d\n%s", tg.driver, code, out)
		}
		return
	}
	guestOS := map[string]machine.OS{"fake": machine.OS(runtime.GOOS), "docker": machine.Linux}[tg.driver]
	data, cache := machine.VolumePath(guestOS, machine.VolumeData), machine.VolumePath(guestOS, machine.VolumeCache)
	local := t.TempDir()
	put := func(inst, dir, name, content string) {
		t.Helper()
		file := filepath.Join(local, name)
		if err := os.WriteFile(file, []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
		e.ok("cp", file, inst+":"+dir)
	}
	read := func(inst, path string) (string, bool) {
		t.Helper()
		out := t.TempDir()
		if _, code := e.run("cp", inst+":"+path, out); code != 0 {
			return "", false
		}
		got, err := os.ReadFile(filepath.Join(out, filepath.Base(path)))
		if err != nil {
			t.Fatal(err)
		}
		return string(got), true
	}
	root := strings.TrimSuffix(tg.file("x"), "/x")

	// to is the upgrade's --from: a committed VM with a file of its own in
	// its root, or none, a fresh root, on a driver that does not commit.
	var to []string
	from := ""
	if e.commits {
		e.ok(append([]string{"run", "up-next"}, e.base()...)...)
		put("up-next", root, "next.txt", "next")
		e.ok("stop", "up-next")
		e.ok("commit", "up-next")
		to, from = []string{"--from", "up-next"}, "up-next"
	}

	e.ok(append([]string{"run", "up", "--data", "1GiB", "--cache", "e2e-up"}, e.base()...)...)
	put("up", data, "data.txt", "data")
	put("up", cache, "cache.txt", "cache")
	put("up", root, "old.txt", "old")
	if out, code := e.run(append([]string{"upgrade", "up"}, to...)...); code == 0 || !strings.Contains(out, "stop it") {
		t.Fatalf("upgrade of a running VM: exit %d\n%s", code, out)
	}
	e.ok("stop", "up")
	refused := map[bool]string{true: "not found", false: "cannot give a VM the root of a committed one"}[e.commits]
	if out, code := e.run("upgrade", "up", "--from", "nothing"); code == 0 || !strings.Contains(out, refused) {
		t.Fatalf("upgrade from no VM: exit %d\n%s", code, out)
	}
	if out := strings.TrimSpace(e.ok(append([]string{"upgrade", "up"}, to...)...)); out != "up" {
		t.Fatalf("upgrade printed %q", out)
	}
	var inspected struct {
		From  string               `json:"from"`
		Data  *machine.DataVolume  `json:"data"`
		Cache *machine.CacheVolume `json:"cache"`
	}
	if err := json.Unmarshal([]byte(e.ok("inspect", "up")), &inspected); err != nil || inspected.From != from || inspected.Data == nil || inspected.Cache == nil || inspected.Cache.Key != "e2e-up" {
		t.Fatalf("inspect up: %+v, %v", inspected, err)
	}
	e.ok("start", "up")
	wants := map[string]string{data + "/data.txt": "data", cache + "/cache.txt": "cache"}
	if e.commits {
		wants[root+"/next.txt"] = "next"
	}
	for path, want := range wants {
		if got, ok := read("up", path); got != want {
			t.Errorf("the upgraded VM reads %q (found %v) from %s, want %q", got, ok, path, want)
		}
	}
	if _, ok := read("up", root+"/old.txt"); ok {
		t.Error("the upgraded VM kept what it wrote to its old root")
	}
	if n := strings.Count(e.ok("ls", "-a"), " up "); n != 1 {
		t.Errorf("the upgraded VM is listed %d times:\n%s", n, e.ok("ls", "-a"))
	}
	e.ok("rm", "-f", "up")
}

// TestFresh makes a fresh VM from the driver's base, which is the driver's
// configuration ($DISCO_VM_BASE), and commits it; a VM created from it
// starts from the same base.
func TestFresh(t *testing.T) {
	if tg.driver != "fake" {
		t.Skip("only the fake driver's base writes a marker the test reads")
	}
	e := newEnv(t)
	e.extra = []string{"DISCO_VM_BASE=stock"}
	e.ok("run", "b")
	mustContain(t, e.ok(append([]string{"exec", "b"}, catArgv(".disco-vm-install")...)...), "base=stock")
	e.ok("stop", "b")
	if !e.commits {
		e.ok("rm", "b")
		return
	}
	e.ok("commit", "b")
	e.ok("run", "b2", "--from", "b")
	mustContain(t, e.ok(append([]string{"exec", "b2"}, catArgv(".disco-vm-install")...)...), "base=stock")
	e.ok("rm", "-f", "b2", "b")
}

// TestNoCommit runs a driver that does not report Capabilities.Commit (ADR
// 26-11-03-868): the fake driver with $DISCO_VM_FAKE_NO_COMMIT, or the
// target where it does not commit. Its info says so; commit refuses a
// running VM and a stopped one, and leaves it to start again; create and run
// refuse --from, and make nothing.
func TestNoCommit(t *testing.T) {
	e := newEnv(t)
	if e.commits {
		if tg.driver != "fake" {
			t.Skipf("%s commits", tg.driver)
		}
		e = newEnv(t, "DISCO_VM_FAKE_NO_COMMIT=1")
	}
	if e.commits {
		t.Fatal("the fake driver with $DISCO_VM_FAKE_NO_COMMIT reports commit")
	}
	mustContain(t, e.ok("info"), `"commit": false`)
	t.Cleanup(func() { e.run("rm", "-f", "vm", "copy") })
	e.ok("run", "vm")
	refused := func(what string, args ...string) {
		t.Helper()
		if out, code := e.run(args...); code == 0 || !strings.Contains(out, "not supported by this driver") {
			t.Fatalf("%s on a driver that does not commit (exit %d):\n%s", what, code, out)
		}
	}
	refused("commit of a running VM", "commit", "vm")
	e.ok("stop", "vm")
	refused("commit of a stopped VM", "commit", "vm")
	refused("create --from", "create", "copy", "--from", "vm")
	refused("run --from", "run", "copy", "--from", "vm")
	if out := e.ok("ls", "-a"); strings.Contains(out, "copy") {
		t.Fatalf("a refused copy is listed:\n%s", out)
	}
	e.ok("start", "vm")
	e.ok("rm", "-f", "vm")
}

// TestPoolScope exercises the breaking CLI scope rename, including flag
// precedence over the environment and the removal of the old flag.
func TestPoolScope(t *testing.T) {
	if tg.driver != "fake" {
		t.Skip("uses a scratch local store with the fake driver")
	}
	e := env{t: t, root: t.TempDir(), extra: []string{"DISCO_VM_POOL=env-pool"}}
	e.ok("create", "env-vm")
	e.ok("--pool", "flag-pool", "create", "flag-vm")
	mustContain(t, e.ok("ls", "-a"), "env-vm")
	if out := e.ok("ls", "-a"); strings.Contains(out, "flag-vm") {
		t.Fatalf("environment pool saw flag pool VM: %s", out)
	}
	mustContain(t, e.ok("--pool", "flag-pool", "ls", "-a"), "flag-vm")
	if out, code := e.run("--project", "old", "ls"); code == 0 || !strings.Contains(out, "unknown flag: --project") {
		t.Fatalf("old flag: exit %d, %s", code, out)
	}
	if out, code := e.run("--pool", "bad--pool", "ls"); code == 0 || !strings.Contains(out, "pool") {
		t.Fatalf("invalid pool: exit %d, %s", code, out)
	}
	e.ok("rm", "env-vm")
	e.ok("--pool", "flag-pool", "rm", "flag-vm")
}

// TestCapacityAcrossPools caps the fake driver at one running guest
// ($DISCO_VM_FAKE_MAX_RUNNING): a VM running in one pool keeps another pool's
// from starting, until it stops.
func TestCapacityAcrossPools(t *testing.T) {
	if tg.driver != "fake" {
		t.Skip("uses a scratch local store with the fake driver")
	}
	e := env{t: t, root: t.TempDir(), extra: []string{"DISCO_VM_FAKE_MAX_RUNNING=1"}}
	t.Cleanup(func() {
		e.run("--pool", "a", "rm", "-f", "vm")
		e.run("--pool", "b", "rm", "-f", "vm")
	})
	e.ok("--pool", "a", "run", "vm")
	e.ok("--pool", "b", "create", "vm")
	if out, code := e.run("--pool", "b", "start", "vm"); code == 0 || !strings.Contains(out, "runs at most 1") {
		t.Fatalf("a start in another pool past the cap (exit %d):\n%s", code, out)
	}
	e.ok("--pool", "a", "stop", "vm")
	e.ok("--pool", "b", "start", "vm")
}
