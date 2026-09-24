// Package e2e drives the real disco-vm binary through its whole lifecycle: build
// (install, layers, cache), images, run, exec, cp, stop, start, rm, rmi, warm.
// On the fake driver it is the neutral proof of everything above the driver
// seam, and it runs on every OS.
//
// $DISCO_VM_DRIVER runs it against a real driver instead. That needs a signed
// binary on macOS ($DISCO_VM_TEST_BINARY) and a persistent state root
// ($DISCO_VM_E2E_ROOT), where the base install is built once, tagged
// e2e/base, and found in the build cache by every later run. On hcs it also
// needs a Windows ISO ($DISCO_VM_E2E_ISO) and an elevated shell.
package e2e

import (
	"bytes"
	"fmt"
	"io"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"
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
		}
	default:
		panic("e2e: no guest layout for driver " + driver)
	}
}

func TestMain(m *testing.M) {
	tg = targetFromEnv()
	if tg.driver != "fake" && tg.root == "" {
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
	build := exec.Command("go", "build", "-o", binary, "../../cmd/disco-vm")
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
	cmd := exec.Command(binary, args...)
	cmd.Env = append(os.Environ(), "DISCO_VM_ROOT="+e.root, "DISCO_VM_DRIVER="+tg.driver)
	var out bytes.Buffer
	cmd.Stdout, cmd.Stderr = &out, &out
	err := cmd.Run()
	code := 0
	if exitErr, ok := err.(*exec.ExitError); ok {
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
	if tg.root == "" {
		return env{t: t, root: t.TempDir()}
	}
	e := env{t: t, root: tg.root}
	// Whatever a failed run left, so that this one builds its layers anew.
	e.run("rm", "-f", "dev", "dev2", "before", "one", "two", "after", "fw")
	e.run("warm", "--rm", "test/warm")
	for _, ref := range []string{"test/app:v1", "test/app:latest", "test/app:other", "test/warm", "test/fwd"} {
		e.run("rmi", ref)
	}
	spec := filepath.Join(t.TempDir(), "base.yaml")
	if err := os.WriteFile(spec, []byte("from:\n  install: "+tg.install+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	e.ok("build", "-f", spec, "-t", "e2e/base")
	return e
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

	// Run an instance and look inside.
	e.ok("run", "--name", "dev", "test/app:v1")
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
	if strings.Contains(e.ok("ps"), "dev ") {
		t.Fatal("dev is listed as running after stop")
	}
	mustContain(t, e.ok("ps", "-a"), "stopped")
	e.ok("start", "dev")
	mustContain(t, e.ok(append([]string{"exec", "dev"}, catArgv("drop/note.txt")...)...), "from host")

	// An image in use cannot lose its layers; after removal it can.
	e.ok("rm", "-f", "dev", "dev2")
	if strings.Contains(e.ok("ps", "-a"), "dev") {
		t.Fatal("removed instances are still listed")
	}
	out = e.ok("rmi", "test/app:v1", "test/app:latest")
	mustContain(t, out, "untagged test/app:v1", "deleted")
	mustContain(t, e.ok("images"), "test/app:other")

	// run --rm with a command is a one-shot.
	out, code := e.run(append([]string{"run", "--rm", "test/app:other"}, catArgv("greeting.txt")...)...)
	if code != 0 || !strings.Contains(out, "other") {
		t.Fatalf("run --rm: exit %d\n%s", code, out)
	}
	if ps := e.ok("ps", "-a"); strings.Count(ps, "\n") != 1 {
		t.Fatalf("run --rm left an instance:\n%s", ps)
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

	// The driver's fast mode.
	fast := "resume"
	if strings.Contains(e.ok("info"), `"fork"`) {
		fast = "fork"
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
	mustContain(t, e.ok("ps"), fast)
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

// A guest process reaches a host service through a forward: it connects out
// to the host on a port, and the instance's shim splices that into a Unix
// socket on the host, in both directions.
func TestForward(t *testing.T) {
	e := newEnv(t)
	if !strings.Contains(e.ok("info"), `"forward": true`) {
		t.Skipf("driver %s cannot forward a guest's connections to the host", tg.driver)
	}
	contextDir := t.TempDir()
	specPath := filepath.Join(contextDir, "disco-vm.yaml")
	if err := os.WriteFile(specPath, []byte("from:\n  install: "+tg.install+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	e.ok("build", "-f", specPath, "-t", "test/fwd")

	// A short directory: a Unix socket path is limited to about 100 bytes.
	dir, err := os.MkdirTemp("", "dvf")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(dir) })
	socket := filepath.Join(dir, "host.sock")
	l, err := net.Listen("unix", socket)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = l.Close() })
	fromGuest := make(chan string, 1)
	go func() {
		conn, err := l.Accept()
		if err != nil {
			return
		}
		defer conn.Close()
		fmt.Fprintln(conn, "hello from the host")
		data, _ := io.ReadAll(conn)
		fromGuest <- string(data)
	}()

	e.ok("run", "--name", "fw", "--forward", "7401:"+socket, "test/fwd")
	t.Cleanup(func() { e.run("rm", "-f", "fw") })
	// The guest's disco-vm: this build, copied in on a real guest, whose
	// baked-in agent may predate dial-host.
	// A guest is Windows on a Windows host (fake, hcs), whose shell is cmd.
	windows := runtime.GOOS == "windows"
	agent := binary
	if tg.driver != "fake" {
		if windows {
			e.ok("exec", "fw", "cmd", "/c", "mkdir", strings.ReplaceAll(tg.cmd("dv"), "/", `\`))
		} else {
			e.ok("exec", "fw", "mkdir", "-p", tg.cmd("dv"))
		}
		e.ok("cp", binary, "fw:"+tg.file("dv"))
		agent = tg.cmd("dv/" + filepath.Base(binary))
	}
	pipe := []string{"/bin/sh", "-c", `echo "hello from the guest" | "$0" dial-host 7401`, agent}
	if windows {
		// No space before the pipe: cmd would send it.
		pipe = []string{"cmd", "/c", "echo hello from the guest| " + strings.ReplaceAll(agent, "/", `\`) + " dial-host 7401"}
	}
	out := e.ok(append([]string{"exec", "fw"}, pipe...)...)
	mustContain(t, out, "hello from the host")
	select {
	case got := <-fromGuest:
		mustContain(t, got, "hello from the guest")
	case <-time.After(30 * time.Second):
		t.Fatal("the host socket never heard from the guest")
	}
	e.ok("rm", "-f", "fw")
	e.ok("rmi", "test/fwd")
}
