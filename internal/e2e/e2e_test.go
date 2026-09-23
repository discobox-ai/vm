// Package e2e drives the real disco-vm binary through its whole lifecycle on
// the fake driver: build (install, layers, cache), images, run, exec, cp, stop,
// start, rm, rmi. It is the neutral proof of everything above the driver seam,
// and it runs on every OS.
package e2e

import (
	"bytes"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

var binary string

func TestMain(m *testing.M) {
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
	cmd.Env = append(os.Environ(), "DISCO_VM_ROOT="+e.root, "DISCO_VM_DRIVER=fake")
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

// catArgv reads a guest file relative to the fake guest's root.
func catArgv(path string) []string {
	if runtime.GOOS == "windows" {
		return []string{"cmd", "/c", "type", strings.ReplaceAll(path, "/", `\`)}
	}
	return []string{"cat", path}
}

func TestLifecycle(t *testing.T) {
	e := env{t: t, root: t.TempDir()}
	contextDir := t.TempDir()
	if err := os.MkdirAll(filepath.Join(contextDir, "payload"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(contextDir, "payload", "tool.txt"), []byte("tool v1"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(contextDir, "fake.iso"), []byte("not really"), 0o644); err != nil {
		t.Fatal(err)
	}
	shell := `["/bin/sh", "-c"]`
	other := "windows"
	if runtime.GOOS == "windows" {
		shell, other = `["cmd", "/c"]`, "darwin"
	}
	spec := fmt.Sprintf(`
name: test/app
args:
  MSG: hello
from:
  install:
    os: %s
    media: fake.iso
    disk: 1GiB
shell: %s
layers:
  - name: files
    steps:
      - copy: {src: payload, dst: /opt/app}
      - run: echo ${MSG}> greeting.txt
  - name: elsewhere
    when: {os: %s}
    steps:
      - run: exit 1
`, runtime.GOOS, shell, other)
	specPath := filepath.Join(contextDir, "disco-vm.yaml")
	if err := os.WriteFile(specPath, []byte(spec), 0o644); err != nil {
		t.Fatal(err)
	}

	// Build, then build again from cache.
	out := e.ok("build", "-f", specPath, "--build-arg", "MSG=hi", "-t", "test/app:v1")
	mustContain(t, out, "==> install "+runtime.GOOS, "[1/1] files", "committed", "elsewhere: skipped", "tagged test/app:latest", "tagged test/app:v1")
	out = e.ok("build", "-f", specPath, "--build-arg", "MSG=hi")
	mustContain(t, out, "install "+runtime.GOOS+": CACHED", "files: CACHED")
	// A different arg is a different layer.
	out = e.ok("build", "-f", specPath, "--build-arg", "MSG=other", "-t", "test/app:other")
	mustContain(t, out, "install "+runtime.GOOS+": CACHED", "committed")
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
	mustContain(t, e.ok("inspect", "dev"), `"state": "running"`, `"fake": true`)

	// Copy both ways.
	local := filepath.Join(t.TempDir(), "note.txt")
	if err := os.WriteFile(local, []byte("from host"), 0o644); err != nil {
		t.Fatal(err)
	}
	e.ok("cp", local, "dev:/drop")
	mustContain(t, e.ok(append([]string{"exec", "dev"}, catArgv("drop/note.txt")...)...), "from host")
	outDir := t.TempDir()
	e.ok("cp", "dev:/opt/app", outDir)
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
	mustContain(t, e.ok("info"), "driver:   fake", "check:    ok", "fake")
}

// A warm image serves clones from its stage until it is used up, then auto
// falls back to cold and an explicit fast mode says to warm again.
func TestWarm(t *testing.T) {
	e := env{t: t, root: t.TempDir()}
	contextDir := t.TempDir()
	if err := os.WriteFile(filepath.Join(contextDir, "greeting.txt"), []byte("warm hello"), 0o644); err != nil {
		t.Fatal(err)
	}
	spec := fmt.Sprintf(`
name: test/warm
from:
  install: {os: %s, media: latest}
layers:
  - name: files
    steps:
      - copy: {src: greeting.txt, dst: /}
`, runtime.GOOS)
	specPath := filepath.Join(contextDir, "disco-vm.yaml")
	if err := os.WriteFile(specPath, []byte(spec), 0o644); err != nil {
		t.Fatal(err)
	}
	e.ok("build", "-f", specPath)
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
	if _, code := e.run("run", "--mode", "resume", "test/warm"); code == 0 {
		t.Fatal("a resume clone of an image never warmed was allowed")
	}

	mustContain(t, e.ok("warm", "--count", "2", "test/warm"), "warm, 2 resume clones")
	mustContain(t, e.ok("images"), "2 resume clones")
	e.ok("run", "--name", "one", "test/warm")
	e.ok("run", "--name", "two", "--mode", "resume", "test/warm")
	for _, name := range []string{"one", "two"} {
		if got := mode(name); got != "resume" {
			t.Fatalf("instance %s of a warm image cloned %s", name, got)
		}
		mustContain(t, e.ok(append([]string{"exec", name}, catArgv("greeting.txt")...)...), "warm hello")
	}
	mustContain(t, e.ok("ps"), "resume")

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

	// Warming again tops the stage up.
	mustContain(t, e.ok("warm", "test/warm"), "1 resume clone")

	// A warm image is not deleted out from under its stage.
	e.ok("rm", "-f", "before", "one", "two", "after")
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
