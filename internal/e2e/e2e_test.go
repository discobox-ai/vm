// Package e2e drives the real disco-vm binary through its whole lifecycle: build
// (install, layers, cache), images, run, exec, cp, stop, start, snapshot, rm,
// rmi, warm.
// On the fake driver it is the neutral proof of everything above the driver
// seam, and it runs on every OS.
//
// $DISCO_VM_DRIVER runs it against a real driver instead. That needs a signed
// binary on macOS ($DISCO_VM_TEST_BINARY) and a persistent state root
// ($DISCO_VM_E2E_ROOT), where the base install is built once, tagged
// e2e/base, and found in the build cache by every later run. On hcs it also
// needs a Windows ISO ($DISCO_VM_E2E_ISO) and an elevated shell. The docker
// driver installs in seconds, so its state root is optional; it installs from
// debian:trixie, or the image $DISCO_VM_E2E_IMAGE names.
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
	// install is the from.install every spec uses, so every build shares one
	// cached base layer.
	install string
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

// file is a guest path for the file API and for a spec's copy destination.
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
		return target{driver: "fake", install: fmt.Sprintf("{os: %s, media: latest}", runtime.GOOS)}
	case "vz":
		return target{
			driver:  "vz",
			install: "{os: darwin, media: latest, disk: 64GiB}",
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
			driver:  "docker",
			install: fmt.Sprintf("{os: linux, media: latest, options: {image: '%s'}}", image),
			dir:     "/srv",
			root:    os.Getenv("DISCO_VM_E2E_ROOT"),
		}
	case "hcs":
		iso := os.Getenv("DISCO_VM_E2E_ISO")
		if iso == "" {
			fmt.Fprintln(os.Stderr, "e2e: driver hcs needs $DISCO_VM_E2E_ISO, a Windows ISO to install from")
			os.Exit(2)
		}
		return target{
			driver:  "hcs",
			install: fmt.Sprintf("{os: windows, media: '%s', edition: Windows 11 Pro, disk: 64GiB}", strings.ReplaceAll(iso, "'", "''")),
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

func (e env) run(args ...string) (string, int) {
	e.t.Helper()
	// Not t.Context(): cleanups call run too, after that is canceled.
	cmd := exec.CommandContext(context.Background(), binary, args...)
	cmd.Env = append(os.Environ(), "DISCO_VM_ROOT="+e.root, "DISCO_VM_DRIVER="+tg.driver)
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

// newEnv is a state root for one test: a fresh one on the fake driver, and on
// a real driver the persistent one, with its base install built and tagged.
func newEnv(t *testing.T) env {
	t.Helper()
	if tg.root == "" {
		e := env{t: t, root: t.TempDir()}
		if tg.driver != "fake" {
			// A real driver may hold layers outside the state root (docker
			// images), which a root thrown away would leave behind.
			t.Cleanup(e.release)
		}
		return e
	}
	e := env{t: t, root: tg.root}
	// Whatever a failed run left, so that this one builds its layers anew.
	e.run("rm", "-f", "dev", "dev2", "before", "one", "two", "after", "fw", "svc", "nosvc", "snap", "snap2")
	e.run("warm", "--rm", "test/warm")
	for _, ref := range []string{"test/app:v1", "test/app:latest", "test/app:other", "test/warm", "test/fwd", "test/svc", "test/nosvc", "test/snap:v2", "test/snap:v1", "test/snap:base"} {
		e.run("rmi", ref)
	}
	spec := filepath.Join(t.TempDir(), "base.yaml")
	if err := os.WriteFile(spec, []byte("from:\n  install: "+tg.install+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	e.ok("build", "-f", spec, "-t", "e2e/base")
	return e
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
		e.run("warm", "--rm", ref)
		e.run("rmi", ref)
	}
	// What is left is untagged, and named by its layer.
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
	contextDir := t.TempDir()
	if err := os.MkdirAll(filepath.Join(contextDir, "payload"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(contextDir, "payload", "tool.txt"), []byte("tool v1"), 0o644); err != nil {
		t.Fatal(err)
	}
	// The guest's OS, which is the host's except on a driver that runs another.
	guestOS := runtime.GOOS
	switch tg.driver {
	case "hcs":
		guestOS = "windows"
	case "vz":
		guestOS = "darwin"
	case "docker":
		guestOS = "linux"
	}
	shell := `["/bin/sh", "-c"]`
	other := "windows"
	if runtime.GOOS == "windows" {
		shell, other = `["cmd", "/c"]`, "darwin"
	}
	spec := fmt.Sprintf(`
args:
  MSG: hello
from:
  install: %s
shell: %s
layers:
  - name: files
    steps:
      - copy: {src: payload, dst: %s}
      - run: echo ${MSG}> %s
  - name: elsewhere
    when: {os: %s}
    steps:
      - run: exit 1
`, tg.install, shell, tg.file("opt/app"), tg.cmd("greeting.txt"), other)
	specPath := filepath.Join(contextDir, "disco-vm.yaml")
	if err := os.WriteFile(specPath, []byte(spec), 0o644); err != nil {
		t.Fatal(err)
	}

	// Build, then build again from cache. The spec names nothing; -t does.
	out := e.ok("build", "-f", specPath, "--build-arg", "MSG=hi", "-t", "test/app", "-t", "test/app:v1")
	mustContain(t, out, "==> install "+guestOS, "[1/1] files", "committed", "elsewhere: skipped", "tagged test/app:latest", "tagged test/app:v1")
	out = e.ok("build", "-f", specPath, "--build-arg", "MSG=hi")
	mustContain(t, out, "install "+guestOS+": CACHED", "files: CACHED", "untagged")
	// A different arg is a different layer.
	out = e.ok("build", "-f", specPath, "--build-arg", "MSG=other", "-t", "test/app:other")
	mustContain(t, out, "install "+guestOS+": CACHED", "committed")
	if _, code := e.run("build", "-f", specPath, "--build-arg", "NOPE=1"); code == 0 {
		t.Fatal("an undeclared build arg was accepted")
	}
	mustContain(t, e.ok("images"), "test/app:latest", "test/app:v1", "test/app:other")
	// A build without -t is still an image: listed, and removed by its ID.
	out = e.ok("build", "-f", specPath, "--build-arg", "MSG=solo")
	fields := strings.Fields(out)
	solo := fields[len(fields)-1]
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
	exitArgv := []string{"/bin/sh", "-c", "exit 7"}
	if runtime.GOOS == "windows" {
		exitArgv = []string{"cmd", "/c", "exit 7"}
	}
	if _, code := e.run(append([]string{"exec", "dev"}, exitArgv...)...); code != 7 {
		t.Fatalf("exec exit code = %d, want 7", code)
	}
	mustContain(t, e.ok("inspect", "dev"), `"state": "running"`)
	if tg.driver == "fake" {
		mustContain(t, e.ok("inspect", "dev"), `"fake": true`)
	}

	// Copy both ways.
	local := filepath.Join(t.TempDir(), "note.txt")
	if err := os.WriteFile(local, []byte("from host"), 0o644); err != nil {
		t.Fatal(err)
	}
	e.ok("cp", local, "dev:"+tg.file("drop"))
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

	// An image in use cannot lose its layers; after removal it can.
	e.ok("rm", "-f", "dev", "dev2")
	if strings.Contains(e.ok("ls", "-a"), "dev") {
		t.Fatal("removed instances are still listed")
	}
	out = e.ok("rmi", "test/app:v1", "test/app:latest")
	mustContain(t, out, "untagged test/app:v1", "deleted")
	mustContain(t, e.ok("images"), "test/app:other")

	// run takes an image and nothing else; a command goes through exec.
	if _, code := e.run(append([]string{"run", "test/app:other"}, catArgv("greeting.txt")...)...); code == 0 {
		t.Fatal("run took a command")
	}
	if _, code := e.run("run", "--rm", "test/app:other"); code == 0 {
		t.Fatal("run took --rm")
	}
}

// TestLostLayer loses a layer in the middle of a chain, as deleting a boxd
// snapshot does, and checks that the next build makes it again under a new ID
// and so makes every layer after it again too, while the layer before it and
// the images already built stay as they were.
func TestLostLayer(t *testing.T) {
	if tg.driver != "fake" {
		t.Skip("only the fake driver keeps a layer where a test can lose it")
	}
	e := newEnv(t)
	shell := `["/bin/sh", "-c"]`
	if runtime.GOOS == "windows" {
		shell = `["cmd", "/c"]`
	}
	specPath := filepath.Join(t.TempDir(), "disco-vm.yaml")
	spec := fmt.Sprintf("from:\n  install: %s\nshell: %s\nlayers:\n", tg.install, shell)
	for _, name := range []string{"one", "two", "three"} {
		spec += fmt.Sprintf("  - name: %s\n    steps:\n      - run: echo %s> %s\n", name, name, tg.cmd(name+".txt"))
	}
	if err := os.WriteFile(specPath, []byte(spec), 0o644); err != nil {
		t.Fatal(err)
	}
	// ids are the layers a build's log names, in order: install, one, two, three.
	ids := func(out string) []string {
		var ids []string
		for line := range strings.SplitSeq(out, "\n") {
			for _, mark := range []string{": CACHED ", ": committed "} {
				if _, after, ok := strings.Cut(line, mark); ok && strings.HasPrefix(line, "==> [") {
					ids = append(ids, strings.Fields(after)[0])
				}
			}
		}
		if len(ids) != 3 {
			t.Fatalf("a build of three layers named %v:\n%s", ids, out)
		}
		return ids
	}
	first := ids(e.ok("build", "-f", specPath, "-t", "test/lost"))

	dirs, err := filepath.Glob(filepath.Join(e.root, "images", "layers", first[1]+"*", "payload", "*"))
	if err != nil || len(dirs) == 0 {
		t.Fatalf("layer %s holds %v, %v", first[1], dirs, err)
	}
	for _, dir := range dirs {
		if err := os.RemoveAll(dir); err != nil {
			t.Fatal(err)
		}
	}

	out := e.ok("build", "-f", specPath, "-t", "test/lost")
	mustContain(t, out, "[1/3] one: CACHED "+first[0], "[2/3] two: fake no longer has "+first[1], "[3/3] three: committed")
	second := ids(out)
	if second[0] != first[0] || second[1] == first[1] || second[2] == first[2] {
		t.Fatalf("layers %v became %v: want the first kept and the two after it new", first, second)
	}
	// The replacements are the cache from here on.
	out = e.ok("build", "-f", specPath, "-t", "test/lost")
	mustContain(t, out, "two: CACHED "+second[1], "three: CACHED "+second[2])
	// The image built on the lost layer is still there, untagged, to remove.
	mustContain(t, e.ok("images"), "test/lost:latest", "<none>", first[2])
	e.ok("rmi", first[2])
	e.ok("rmi", "test/lost")
}

// TestSnapshot commits a stopped instance as an image, which consumes the
// instance, then snapshots an instance of that image in turn: each snapshot
// keeps what the instance wrote, on top of the image it came from.
func TestSnapshot(t *testing.T) {
	e := newEnv(t)
	base := filepath.Join(t.TempDir(), "base.yaml")
	if err := os.WriteFile(base, []byte("from:\n  install: "+tg.install+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	e.ok("build", "-f", base, "-t", "test/snap:base")
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

	e.ok("run", "--name", "snap", "test/snap:base")
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
		t.Fatal("two snapshots share a layer")
	}
	mustContain(t, e.ok("images"), "<none>", second[:12])
	e.ok("tag", second, "test/snap:v2")
	e.ok("run", "--name", "snap", "test/snap:v2")
	read("snap", "first")
	read("snap", "second")

	// The snapshots are layers like any other: kept while in use, then removed.
	e.ok("rm", "-f", "snap")
	e.ok("rmi", "test/snap:v2", "test/snap:v1", "test/snap:base")
	if out := e.ok("images", "-a"); strings.Contains(out, first[:12]) || strings.Contains(out, second[:12]) {
		t.Fatalf("a removed snapshot is still listed:\n%s", out)
	}
}

func TestInfo(t *testing.T) {
	e := env{t: t, root: t.TempDir()}
	mustContain(t, e.ok("info"), "driver:   "+tg.driver, "check:    ok", "fake")
	if tg.driver == "fake" {
		// A driver with no display refuses a window rather than ignoring it.
		out, code := e.run("build", "--gui", "-f", "nonexistent.yaml")
		if code == 0 || !strings.Contains(out, "no display") {
			t.Fatalf("build --gui on the fake driver: exit %d\n%s", code, out)
		}
	}
}

// A warm image serves clones from its stage. A template (hcs fork, vz resume)
// serves any number; a pool (fake) serves one per staged clone until it is
// used up, then auto falls back to cold and an explicit resume says to warm
// again.
func TestWarm(t *testing.T) {
	e := newEnv(t)
	// The driver's fast mode.
	info := e.ok("info")
	fast := "resume"
	if strings.Contains(info, `"fork"`) {
		fast = "fork"
	} else if !strings.Contains(info, `"resume"`) {
		t.Skipf("driver %s clones cold only", tg.driver)
	}
	contextDir := t.TempDir()
	if err := os.WriteFile(filepath.Join(contextDir, "greeting.txt"), []byte("warm hello"), 0o644); err != nil {
		t.Fatal(err)
	}
	spec := fmt.Sprintf(`
from:
  install: %s
layers:
  - name: files
    steps:
      - copy: {src: greeting.txt, dst: %s}
`, tg.install, tg.file(""))
	specPath := filepath.Join(contextDir, "disco-vm.yaml")
	if err := os.WriteFile(specPath, []byte(spec), 0o644); err != nil {
		t.Fatal(err)
	}
	e.ok("build", "-f", specPath, "-t", "test/warm")
	mode := func(name string) string {
		t.Helper()
		out := e.ok("inspect", name)
		for _, m := range []string{"cold", "resume", "fork"} {
			if strings.Contains(out, `"mode": "`+m+`"`) {
				return m
			}
		}
		t.Fatalf("inspect %s shows no mode:\n%s", name, out)
		return ""
	}

	// Cold until warmed.
	e.ok("run", "--name", "before", "test/warm")
	if got := mode("before"); got != "cold" {
		t.Fatalf("an instance of a cold image cloned %s", got)
	}
	if _, code := e.run("run", "--mode", fast, "test/warm"); code == 0 {
		t.Fatalf("a %s clone of an image never warmed was allowed", fast)
	}
	// No more than two run at once anywhere in this test: vz allows two.
	e.ok("rm", "-f", "before")

	// A template says "<mode> clones", any number; a pool counts them.
	warmed := e.ok("warm", "--count", "2", "test/warm")
	unlimited := strings.Contains(warmed, "warm, "+fast+" clones")
	if unlimited {
		mustContain(t, e.ok("images"), fast+" clones")
	} else {
		mustContain(t, warmed, "warm, 2 "+fast+" clones")
		mustContain(t, e.ok("images"), "2 "+fast+" clones")
	}
	// Auto takes the fast mode, and naming it does too.
	e.ok("run", "--name", "one", "test/warm")
	e.ok("run", "--name", "two", "--mode", fast, "test/warm")
	for _, name := range []string{"one", "two"} {
		if got := mode(name); got != fast {
			t.Fatalf("instance %s of a warm image cloned %s", name, got)
		}
		mustContain(t, e.ok(append([]string{"exec", name}, catArgv("greeting.txt")...)...), "warm hello")
	}
	mustContain(t, e.ok("ls"), fast)
	e.ok("rm", "-f", "one", "two")

	if unlimited {
		// A template is never used up.
		e.ok("run", "--name", "after", "test/warm")
		if got := mode("after"); got != fast {
			t.Fatalf("an instance of a warm template cloned %s", got)
		}
		mustContain(t, e.ok("warm", "test/warm"), "warm, "+fast+" clones")
	} else {
		// Used up: auto boots cold, and resume says to warm again.
		e.ok("run", "--name", "after", "test/warm")
		if got := mode("after"); got != "cold" {
			t.Fatalf("an instance of a used-up stage cloned %s", got)
		}
		out, code := e.run("run", "--mode", "resume", "test/warm")
		if code == 0 {
			t.Fatal("a resume clone of a used-up stage was allowed")
		}
		mustContain(t, out, "disco-vm warm test/warm")
		mustContain(t, e.ok("images"), "used up (0 of 2)")

		// Warming again tops the stage up.
		mustContain(t, e.ok("warm", "test/warm"), "1 resume clone")
	}

	// A warm image is not deleted out from under its stage.
	e.ok("rm", "-f", "after")
	if _, code := e.run("rmi", "test/warm"); code == 0 {
		t.Fatal("rmi deleted a warm image")
	}
	mustContain(t, e.ok("warm", "--rm", "test/warm"), "cooled")
	if strings.Contains(e.ok("images"), "clone") {
		t.Fatal("a cooled image still shows a stage")
	}
	if entries, _ := os.ReadDir(filepath.Join(e.root, "warm")); len(entries) != 0 {
		t.Fatalf("warm --rm left %d stages on disk", len(entries))
	}
	e.ok("rmi", "test/warm")
}

// Every instance has one service. Its URL is served while it runs, and the
// guest finds where to listen in its service file. On the fake driver the
// guest is a host process, so the service is this test's server.
func TestURL(t *testing.T) {
	e := newEnv(t)
	specPath := filepath.Join(t.TempDir(), "svc.yaml")
	if err := os.WriteFile(specPath, []byte("from:\n  install: "+tg.install+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	e.ok("build", "-f", specPath, "-t", "test/svc")
	e.ok("create", "--name", "svc", "test/svc")
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
	e.ok("rmi", "test/svc")
}
