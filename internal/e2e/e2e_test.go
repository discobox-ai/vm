// Package e2e drives the real disco-vm binary through its whole lifecycle:
// install, images, run, exec, cp, stop, start, snapshot, tag, rm, rmi.
// On the fake driver it is the neutral proof of everything above the driver
// seam, and it runs on every OS.
//
// $DISCO_VM_DRIVER runs it against a real driver instead. That needs a signed
// binary on macOS ($DISCO_VM_TEST_BINARY) and a persistent state root
// ($DISCO_VM_E2E_ROOT), where the base install is made once, tagged
// e2e/base, and found by every later run. On hcs it also needs a Windows ISO
// ($DISCO_VM_E2E_ISO) and an elevated shell. The docker driver installs
// nothing: e2e/base is a snapshot of an instance of debian:trixie, or of the
// image $DISCO_VM_E2E_IMAGE names, so its state root is optional.
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
	"strings"
	"testing"
	"time"

	"github.com/discobox-ai/vm/pkg/guest"
)

var (
	binary string
	tg     target
)

// target is the driver the suite drives, and what its guest looks like.
type target struct {
	driver string
	// install is the arguments of the `disco-vm install` that makes
	// e2e/base, on a driver that installs.
	install []string
	// base is the driver's base that e2e/base is a snapshot of, on a driver
	// that does not.
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
}

// projectEnv names the project every command runs in, "e2e" by default. A
// driver whose provider is a service (docker) keeps everything there, not
// in the root, and the suite removes all of a project's instances and images,
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
	var out bytes.Buffer
	cmd.Stdout, cmd.Stderr = &out, &out
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

// newEnv is a state root for one test, holding e2e/base: a fresh one on the
// fake driver, and on a real driver the persistent one, where e2e/base is
// made once.
func newEnv(t *testing.T) env {
	t.Helper()
	e := env{t: t, root: tg.root}
	if tg.root == "" {
		e.root = t.TempDir()
		if tg.driver != "fake" {
			// A real driver may hold images outside the state root (docker
			// images), which a root thrown away would leave behind.
			t.Cleanup(e.release)
		}
	} else {
		// Whatever a failed run left, so that this one makes its images anew.
		e.run("rm", "-f", "dev", "dev2", "svc", "snap", "snap2", "for-base", "solo")
		for _, ref := range []string{"test/app:v1", "test/app:latest", "test/snap:v2", "test/snap:v1"} {
			e.run("rmi", ref)
		}
	}
	if !strings.Contains(e.ok("images"), "e2e/base:latest") {
		if tg.base != "" {
			e.snapshotOf(tg.base, "for-base", "e2e/base")
		} else {
			e.ok(append([]string{"install", "-t", "e2e/base"}, tg.install...)...)
		}
	}
	return e
}

// snapshotOf runs an instance of ref named name, stops it, and snapshots it
// as tag, or untagged when tag is empty, and returns the image's ID.
func (e env) snapshotOf(ref, name, tag string) string {
	e.t.Helper()
	e.ok("run", "--name", name, ref)
	e.ok("stop", name)
	args := []string{"snapshot", name}
	if tag != "" {
		args = append(args, tag)
	}
	return strings.TrimSpace(e.ok(args...))
}

// release removes every instance and image in the state root.
func (e env) release() {
	rows := func(out string) []string {
		var first []string
		for _, line := range strings.Split(strings.TrimSpace(out), "\n")[1:] {
			if f := strings.Fields(line); len(f) > 0 {
				first = append(first, f[0])
			}
		}
		return first
	}
	if ids := rows(e.ok("ls", "-a")); len(ids) > 0 {
		e.run(append([]string{"rm", "-f"}, ids...)...)
	}
	for _, ref := range rows(e.ok("images")) {
		e.run("rmi", ref)
	}
	// What is left is untagged, and named by its ID.
	for _, line := range strings.Split(strings.TrimSpace(e.ok("images")), "\n")[1:] {
		if f := strings.Fields(line); len(f) > 1 {
			e.run("rmi", f[1])
		}
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
	mustContain(t, e.ok("images"), "e2e/base:latest")
	exitArgv := []string{"/bin/sh", "-c", "exit 7"}
	writeArgv := []string{"/bin/sh", "-c", "echo hi > " + tg.cmd("greeting.txt")}
	if runtime.GOOS == "windows" {
		exitArgv = []string{"cmd", "/c", "exit 7"}
		writeArgv = []string{"cmd", "/c", "echo hi> " + strings.ReplaceAll(tg.cmd("greeting.txt"), "/", `\`)}
	}

	// Shape an instance of the base, and snapshot it as the app's image.
	local := t.TempDir()
	if err := os.WriteFile(filepath.Join(local, "tool.txt"), []byte("tool v1"), 0o644); err != nil {
		t.Fatal(err)
	}
	e.ok("run", "--name", "dev", "e2e/base")
	e.ok("cp", filepath.Join(local, "tool.txt"), "dev:"+tg.file("opt/app"))
	e.ok(append([]string{"exec", "dev"}, writeArgv...)...)
	e.ok("stop", "dev")
	app := strings.TrimSpace(e.ok("snapshot", "dev", "test/app:v1"))
	e.ok("tag", "test/app:v1", "test/app")
	mustContain(t, e.ok("images"), "test/app:latest", "test/app:v1", app[:12], "e2e/base:latest")

	// A snapshot without a tag is still an image: listed, and removed by its
	// ID.
	solo := e.snapshotOf("test/app:v1", "solo", "")
	mustContain(t, e.ok("images"), "<none>", solo[:12])
	mustContain(t, e.ok("rmi", solo), "deleted "+solo[:12])
	if out := e.ok("images"); strings.Contains(out, "<none>") {
		t.Fatalf("an untagged image survived rmi:\n%s", out)
	}
	if _, code := e.run("rmi", solo); code == 0 {
		t.Fatal("rmi removed an image twice")
	}

	// Run an instance and look inside.
	e.ok("run", "--name", "dev", "test/app:v1")
	mustContain(t, e.ok("ls"), "dev", "running")
	// ps is ls by its older name.
	mustContain(t, e.ok("ps"), "dev", "running")
	mustContain(t, e.ok(append([]string{"exec", "dev"}, catArgv("greeting.txt")...)...), "hi")
	mustContain(t, e.ok(append([]string{"exec", "dev"}, catArgv("opt/app/tool.txt")...)...), "tool v1")
	if _, code := e.run(append([]string{"exec", "dev"}, exitArgv...)...); code != 7 {
		t.Fatalf("exec exit code = %d, want 7", code)
	}
	mustContain(t, e.ok("inspect", "dev"), `"state": "running"`, `"imageID": "`+app)
	if tg.driver == "fake" {
		mustContain(t, e.ok("inspect", "dev"), `"fake": true`)
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

	// An instance's writes are its own: a second instance of the image does
	// not see them, and they survive the first instance's restart.
	e.ok("run", "--name", "dev2", "test/app:v1")
	if _, code := e.run(append([]string{"exec", "dev2"}, catArgv("drop/note.txt")...)...); code == 0 {
		t.Fatal("a second instance saw the first one's write")
	}
	e.ok("stop", "dev")
	if strings.Contains(e.ok("ls"), "dev ") {
		t.Fatal("dev is listed as running after stop")
	}
	mustContain(t, e.ok("ls", "-a"), "stopped")
	e.ok("start", "dev")
	mustContain(t, e.ok(append([]string{"exec", "dev"}, catArgv("drop/note.txt")...)...), "from host")

	// An image in use is untagged but kept; once nothing runs it, its last
	// tag deletes it.
	if out := e.ok("rmi", "test/app:latest"); !strings.Contains(out, "untagged test/app:latest") || strings.Contains(out, "deleted") {
		t.Fatalf("rmi of one of two tags:\n%s", out)
	}
	if _, code := e.run("rmi", app); code == 0 {
		t.Fatal("rmi deleted a tagged image by its ID")
	}
	e.ok("rm", "-f", "dev", "dev2")
	if strings.Contains(e.ok("ls", "-a"), "dev") {
		t.Fatal("removed instances are still listed")
	}
	mustContain(t, e.ok("rmi", "test/app:v1"), "untagged test/app:v1", "deleted "+app[:12])
	if strings.Contains(e.ok("images"), app[:12]) {
		t.Fatal("a deleted image is still listed")
	}

	// run takes an image and nothing else; a command goes through exec.
	if _, code := e.run(append([]string{"run", "e2e/base"}, catArgv("greeting.txt")...)...); code == 0 {
		t.Fatal("run took a command")
	}
	if _, code := e.run("run", "--rm", "e2e/base"); code == 0 {
		t.Fatal("run took --rm")
	}
}

// TestSnapshot commits a stopped instance as an image, which consumes the
// instance, then snapshots an instance of that image in turn: each snapshot
// keeps what the instance wrote, and the second stands alone once the first
// is removed. A driver whose second snapshot still needs the first's disk
// (hcs) keeps the first, untagged, and says so.
func TestSnapshot(t *testing.T) {
	e := newEnv(t)
	dir := t.TempDir()
	// write copies a file named name into the instance's snap directory.
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

	e.ok("run", "--name", "snap", "e2e/base")
	write("snap", "first")
	if out, code := e.run("snapshot", "snap", "test/snap:v1"); code == 0 || !strings.Contains(out, "stop it first") {
		t.Fatalf("a running instance was snapshotted (exit %d):\n%s", code, out)
	}
	e.ok("stop", "snap")
	if out, code := e.run("snapshot", "snap", "Not A Tag"); code == 0 {
		t.Fatalf("an invalid tag was accepted:\n%s", out)
	}
	mustContain(t, e.ok("ls", "-a"), "snap")
	first := strings.TrimSpace(e.ok("snapshot", "snap", "test/snap:v1"))
	if strings.Contains(e.ok("ls", "-a"), "snap") {
		t.Fatal("a snapshotted instance is still listed")
	}
	mustContain(t, e.ok("images"), "test/snap:v1", first[:12])
	if _, code := e.run("snapshot", "snap"); code == 0 {
		t.Fatal("a consumed instance was snapshotted again")
	}

	e.ok("run", "--name", "snap2", "test/snap:v1")
	read("snap2", "first")
	write("snap2", "second")
	e.ok("stop", "snap2")
	// Untagged, it is known by the ID it prints.
	second := strings.TrimSpace(e.ok("snapshot", "snap2"))
	if second == first {
		t.Fatal("two snapshots share an ID")
	}
	mustContain(t, e.ok("images"), "<none>", second[:12])
	e.ok("tag", second, "test/snap:v2")

	// The first goes, or is kept untagged for the second; either way the
	// second runs with both writes.
	out := e.ok("rmi", "test/snap:v1")
	mustContain(t, out, "untagged test/snap:v1")
	kept := strings.Contains(out, "kept it, untagged")
	if !kept {
		mustContain(t, out, "deleted "+first[:12])
	}
	e.ok("run", "--name", "snap", "test/snap:v2")
	read("snap", "first")
	read("snap", "second")

	e.ok("rm", "-f", "snap")
	e.ok("rmi", "test/snap:v2")
	if kept {
		e.ok("rmi", first)
	}
	if out := e.ok("images"); strings.Contains(out, first[:12]) || strings.Contains(out, second[:12]) {
		t.Fatalf("a removed snapshot is still listed:\n%s", out)
	}
}

func TestInfo(t *testing.T) {
	e := env{t: t, root: t.TempDir()}
	mustContain(t, e.ok("info"), "driver:   "+tg.driver, "check:    ok", "fake")
	if tg.driver == "fake" {
		// A driver with no display refuses a window rather than ignoring it.
		out, code := e.run("install", "--gui")
		if code == 0 || !strings.Contains(out, "no display") {
			t.Fatalf("install --gui on the fake driver: exit %d\n%s", code, out)
		}
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
	e.ok("create", "--name", "svc", "e2e/base")
	t.Cleanup(func() { e.run("rm", "-f", "svc") })
	if out, code := e.run("url", "svc"); code == 0 || !strings.Contains(out, "not running") {
		t.Fatalf("url of a stopped instance: exit %d\n%s", code, out)
	}
	e.ok("start", "svc")
	url := strings.TrimSpace(e.ok("url", "svc"))
	if !strings.HasPrefix(url, "http://127.0.0.1:") {
		t.Fatalf("url svc = %q", url)
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

// TestBases lists a driver's base among the images, runs it with no image,
// and snapshots an instance of it; a base is not an image, so tag and rmi
// refuse it.
func TestBases(t *testing.T) {
	if tg.driver != "fake" {
		t.Skip("only the fake driver is given its bases by the test")
	}
	t.Setenv("DISCO_VM_FAKE_BASES", fmt.Sprintf(`[{"name":"stock","id":"stock-1","guestOS":%q,"options":{"image":"stock"}}]`, runtime.GOOS))
	e := newEnv(t)
	row := func() []string {
		t.Helper()
		out := e.ok("images")
		for line := range strings.SplitSeq(out, "\n") {
			if f := strings.Fields(line); len(f) > 1 && f[0] == "stock" {
				return f
			}
		}
		t.Fatalf("images lists no base stock:\n%s", out)
		return nil
	}
	if r := row(); r[1] != "-" {
		t.Fatalf("a base has an image ID: %v", r)
	}
	// A base runs with no image made of it.
	e.ok("run", "--name", "b", "stock")
	mustContain(t, e.ok(append([]string{"exec", "b"}, catArgv(".disco-vm-install")...)...), "base=stock-1", "option.image=stock")
	if images := e.ok("images"); strings.Contains(images, "<none>") {
		t.Fatalf("running a base made an image:\n%s", images)
	}
	for _, args := range [][]string{{"tag", "stock", "test/stock"}, {"rmi", "stock"}} {
		if out, code := e.run(args...); code == 0 || !strings.Contains(out, "bases, not an image") {
			t.Fatalf("disco-vm %s on a base (exit %d):\n%s", strings.Join(args, " "), code, out)
		}
	}

	// An instance of a base snapshots to an image like any other.
	e.ok("stop", "b")
	e.ok("snapshot", "b", "test/from-b")
	e.ok("run", "--name", "b2", "test/from-b")
	mustContain(t, e.ok(append([]string{"exec", "b2"}, catArgv(".disco-vm-install")...)...), "base=stock-1")
	e.ok("rm", "-f", "b2")
	e.ok("rmi", "test/from-b")
	if r := row(); r[1] != "-" {
		t.Fatalf("the base is not listed as it was: %v", r)
	}
}
