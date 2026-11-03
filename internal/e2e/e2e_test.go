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
	// extra is added to every command's environment.
	extra []string
	// stdin is every command's standard input; empty is none.
	stdin string
}

// projectEnv names the project every command runs in, "e2e" by default. A
// driver whose provider is a service (docker) keeps everything there, not
// in the root, and the suite removes all of a project's VMs,
// so it never runs in the project of someone's own.
const projectEnv = "DISCO_VM_E2E_PROJECT"

func (e env) run(args ...string) (string, int) {
	e.t.Helper()
	// Not t.Context(): cleanups call run too, after that is canceled.
	cmd := exec.CommandContext(context.Background(), binary, args...)
	project := os.Getenv(projectEnv)
	if project == "" {
		project = "e2e"
	}
	cmd.Env = append(os.Environ(), "DISCO_VM_ROOT="+e.root, "DISCO_VM_DRIVER="+tg.driver, "DISCO_VM_PROJECT="+project)
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

// newEnv is a state root for one test, holding the committed VM e2e-base: a
// fresh one on the fake driver, and on a real driver the persistent one,
// where e2e-base is made once.
func newEnv(t *testing.T) env {
	t.Helper()
	e := env{t: t, root: tg.root}
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
		e.run("rm", "-f", "dev", "dev2", "svc", "snap3", "snap2", "snap", "app-v1", "solo", "fenced", "vol-s", "vol-b", "vol-a")
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

func TestLifecycle(t *testing.T) {
	e := newEnv(t)
	mustContain(t, e.ok("ls", "-a"), "e2e-base")
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
	e.ok("run", "app-v1", "--from", "e2e-base")
	e.ok("cp", filepath.Join(local, "tool.txt"), "app-v1:"+tg.file("opt/app"))
	e.ok(append([]string{"exec", "app-v1"}, writeArgv...)...)
	e.ok("stop", "app-v1")
	e.ok("commit", "app-v1")
	mustContain(t, e.ok("ls", "-a"), "app-v1", "e2e-base")

	// Run a VM of it and look inside.
	e.ok("run", "dev", "--from", "app-v1")
	mustContain(t, e.ok("ls"), "dev", "running", "app-v1")
	// ps is ls by its older name.
	mustContain(t, e.ok("ps"), "dev", "running")
	mustContain(t, e.ok(append([]string{"exec", "dev"}, catArgv("greeting.txt")...)...), "hi")
	mustContain(t, e.ok(append([]string{"exec", "dev"}, catArgv("opt/app/tool.txt")...)...), "tool v1")
	if _, code := e.run(append([]string{"exec", "dev"}, exitArgv...)...); code != 7 {
		t.Fatalf("exec exit code = %d, want 7", code)
	}
	mustContain(t, e.ok("inspect", "dev"), `"state": "running"`)
	if tg.driver == "fake" {
		mustContain(t, e.ok("inspect", "dev"), `"fake": true`, `"from": "app-v1"`)
	}
	if out, code := e.run("create", "dev3", "--from", ""); code == 0 || !strings.Contains(out, "leave it out for a fresh VM") {
		t.Fatalf("an empty --from (exit %d):\n%s", code, out)
	}
	if out, code := e.run("create", "dev3", "--from", "dev"); code == 0 || !strings.Contains(out, "stop and commit it") {
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
	e.ok("run", "dev2", "--from", "app-v1")
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
	e.ok("rm", "app-v1")
	if out := e.ok("ls", "-a"); strings.Contains(out, "dev") || strings.Contains(out, "app-v1") {
		t.Fatalf("removed VMs are still listed:\n%s", out)
	}

	// run takes a name and nothing else; a command goes through exec.
	if _, code := e.run(append([]string{"run", "--from", "e2e-base", "cmd"}, catArgv("greeting.txt")...)...); code == 0 {
		t.Fatal("run took a command")
	}
	if _, code := e.run("run", "--rm", "--from", "e2e-base"); code == 0 {
		t.Fatal("run took --rm")
	}
}

// TestCommit freezes a stopped VM under its name and creates a VM from it,
// which is committed in turn: each keeps what was written before it, and
// the second outlives the first's removal. A driver whose second still
// needs the first's disk (hcs) refuses the removal until the second is gone.
func TestCommit(t *testing.T) {
	e := newEnv(t)
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
	mustContain(t, e.ok("info"), "driver:   "+tg.driver, "check:    ok", "fake")
	if tg.driver == "fake" {
		// A driver with no display refuses a window rather than ignoring it.
		out, code := e.run("install", "gui", "--gui")
		if code == 0 || !strings.Contains(out, "no display") {
			t.Fatalf("install --gui on the fake driver: exit %d\n%s", code, out)
		}
	}
}

// A key the driver issues runs the project's machines from the command line,
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
	if !key.Issued || key.Secret == "" || key.Name != "pool" || key.Scope != "project" || time.Until(key.Expires) > time.Hour {
		t.Fatalf("issued %+v", key)
	}
	listed := e.ok("key", "ls", "pool")
	mustContain(t, listed, key.ID, "pool", "project")
	if strings.Contains(listed, key.Secret) {
		t.Fatal("key ls shows the secret")
	}

	keyed := env{t: t, root: e.root, extra: []string{"DISCO_VM_FAKE_KEY=" + key.Secret}}
	mustContain(t, keyed.ok("ls", "-a"), "e2e-base")
	keyed.ok("run", "solo", "--from", "e2e-base")
	t.Cleanup(func() { e.run("rm", "-f", "solo") })
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

func driverSelected(out, name string) bool {
	for line := range strings.SplitSeq(out, "\n") {
		fields := strings.Fields(line)
		if len(fields) == 2 && fields[0] == name && fields[1] == "*" {
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
	e.ok("create", "svc", "--from", "e2e-base")
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
	if out, code := e.run("create", "fenced", "--from", "e2e-base", "--egress", "none,example.com"); code == 0 || !strings.Contains(out, "takes no other entry") {
		t.Fatalf("create --egress none,example.com: exit %d\n%s", code, out)
	}
	for _, empty := range [][]string{{"--egress", ""}, {"--egress", "", "--egress", "example.com"}, {"--egress", "example.com,"}} {
		args := append([]string{"create", "fenced", "--from", "e2e-base"}, empty...)
		if out, code := e.run(args...); code == 0 || !strings.Contains(out, "no empty entry") {
			t.Fatalf("create %q: exit %d\n%s", empty, code, out)
		}
	}
	if out, code := e.run("create", "fenced", "--from", "e2e-base", "--egress", "example.com"); code == 0 || !strings.Contains(out, "cannot fence") {
		t.Fatalf("create --egress example.com on %s: exit %d\n%s", tg.driver, code, out)
	}
	if tg.driver == "vz" {
		e.ok("create", "fenced", "--from", "e2e-base", "--egress", "none")
		var fenced struct {
			Egress *struct{} `json:"egress"`
		}
		if err := json.Unmarshal([]byte(e.ok("inspect", "fenced")), &fenced); err != nil || fenced.Egress == nil {
			t.Fatalf("an instance with no network device reports no fence: %v", err)
		}
		e.ok("rm", "fenced")
		return
	}
	if out, code := e.run("create", "fenced", "--from", "e2e-base", "--egress", "none"); code == 0 || !strings.Contains(out, "cannot fence") {
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
	guestOS := map[string]machine.OS{"fake": machine.OS(runtime.GOOS), "docker": machine.Linux, "vz": machine.Darwin, "hcs": machine.Windows}[tg.driver]
	data, cache := machine.VolumePath(guestOS, machine.VolumeData), machine.VolumePath(guestOS, machine.VolumeCache)
	if tg.driver != "fake" && tg.driver != "docker" {
		for _, flags := range [][]string{{"--data", "1GiB"}, {"--cache", "e2e-cache"}} {
			args := append([]string{"create", "vol-a", "--from", "e2e-base"}, flags...)
			if out, code := e.run(args...); code == 0 || !strings.Contains(out, "cannot attach") {
				t.Fatalf("create %q on %s: exit %d\n%s", flags, tg.driver, code, out)
			}
		}
		return
	}
	if out, code := e.run("create", "vol-a", "--from", "e2e-base", "--cache-size", "1GiB"); code == 0 || !strings.Contains(out, "needs --cache") {
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

	e.ok("run", "vol-a", "--from", "e2e-base", "--data", "1GiB", "--cache", "e2e-cache")
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

	e.ok("run", "vol-b", "--from", "e2e-base", "--cache", "e2e-cache")
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

	e.ok("commit", "vol-a")
	e.ok("run", "vol-s", "--from", "vol-a", "--data", "0")
	if out, code := e.run("cp", "vol-s:"+data+"/note.txt", t.TempDir()); code == 0 {
		t.Fatalf("a committed VM carried its data volume:\n%s", out)
	}
	e.ok("cache", "rm", "e2e-cache")
	if out := e.ok("cache", "ls"); strings.Contains(out, "e2e-cache") {
		t.Fatalf("a removed key is listed:\n%s", out)
	}
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
	e.ok("commit", "b")
	e.ok("run", "b2", "--from", "b")
	mustContain(t, e.ok(append([]string{"exec", "b2"}, catArgv(".disco-vm-install")...)...), "base=stock")
	e.ok("rm", "-f", "b2", "b")
}
