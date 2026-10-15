package e2e

import (
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

// TestConverge builds an image from state files, then changes a running
// instance by converging it to changed ones. It uses files and a script
// only, which every guest OS converges, so it runs on a real driver too;
// packages and services are proven where there is a guest that has them.
func TestConverge(t *testing.T) {
	e := newEnv(t)
	e.run("rm", "-f", "conv")
	e.run("rmi", "test/conv")

	guestOS := runtime.GOOS
	switch tg.driver {
	case "hcs":
		guestOS = "windows"
	case "vz":
		guestOS = "darwin"
	case "docker":
		guestOS = "linux"
	}
	appendRan := "echo ran >> " + tg.cmd("conv-ran.txt")
	if guestOS == "windows" {
		appendRan = "Add-Content -Encoding ascii -Path " + tg.cmd("conv-ran.txt") + " -Value ran"
	}
	state := func(inline string, keep bool) string {
		kept := ""
		if keep {
			kept = fmt.Sprintf("  - path: %s\n    content: going\n", tg.file("conv/going.txt"))
		}
		return fmt.Sprintf(`
files:
  - path: %s
    source: tool.txt
  - path: %s
    content: %s
%sexec:
  - name: ran
    run: %s
`, tg.file("conv/tool.txt"), tg.file("conv/inline.txt"), inline, kept, appendRan)
	}

	contextDir := t.TempDir()
	write := func(rel, content string) string {
		t.Helper()
		path := filepath.Join(contextDir, rel)
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
		return path
	}
	app := write("state/10-app.yaml", state("v1", true))
	write("state/tool.txt", "tool v1")
	// One state file, naming a directory elsewhere in the context: a layer
	// of its own.
	write("more/20-extra.yaml", fmt.Sprintf("files:\n  - path: %s\n    source: ../tree\n", tg.file("conv/tree")))
	write("tree/deep/leaf.txt", "leaf")
	base := "install: " + tg.install
	if tg.root != "" {
		base = "image: e2e/base"
	}
	specPath := write("disco-vm.yaml", fmt.Sprintf(`
from:
  %s
layers:
  - name: state
    steps:
      - converge: state
  - name: more
    steps:
      - converge: more/20-extra.yaml
`, base))

	// Boot, converge, snapshot: the layer is committed once the guest matches.
	out := e.ok("build", "-f", specPath, "-t", "test/conv")
	mustContain(t, out, "converge: ", "committed")
	mustContain(t, e.ok("build", "-f", specPath, "-t", "test/conv"), "state: CACHED", "more: CACHED")
	// A source's content is part of what a converge step is, wherever it is.
	write("tree/deep/leaf.txt", "leaf v2")
	mustContain(t, e.ok("build", "-f", specPath, "-t", "test/conv"), "state: CACHED", "more: committed")

	e.ok("run", "--name", "conv", "test/conv")
	t.Cleanup(func() { e.run("rm", "-f", "conv") })
	cat := func(path string) string {
		t.Helper()
		return strings.TrimSpace(e.ok(append([]string{"exec", "conv"}, catArgv(path)...)...))
	}
	// An instance of the image starts converged: the image carries the
	// record, so the same state files apply nothing.
	mustContain(t, e.ok("converge", "conv"), `"state": "converged"`, "10-app.yaml", "20-extra.yaml")
	if out := e.ok("converge", "conv", filepath.Join(contextDir, "state"), filepath.Join(contextDir, "more")); strings.Contains(out, "-->") {
		t.Fatalf("converging an instance to its image's state files applied something:\n%s", out)
	}
	if got := cat("conv/tool.txt"); got != "tool v1" {
		t.Fatalf("a source file holds %q", got)
	}
	if got := cat("conv/tree/deep/leaf.txt"); got != "leaf v2" {
		t.Fatalf("a file from a source directory holds %q", got)
	}
	if got := cat("conv-ran.txt"); got != "ran" {
		t.Fatalf("the script ran other than once, at build: %q", got)
	}

	// A changed state file: one file changes, one leaves, and the script,
	// which did not change, does not run again. The other state file is not
	// given, and stays as it is.
	write("state/10-app.yaml", state("v2", false))
	mustContain(t, e.ok("converge", "conv", app), "converged")
	if got := cat("conv/inline.txt"); got != "v2" {
		t.Fatalf("converge returned with %q applied, not v2", got)
	}
	if _, code := e.run(append([]string{"exec", "conv"}, catArgv("conv/going.txt")...)...); code == 0 {
		t.Fatal("a file that left the state files is still there")
	}
	if got := cat("conv-ran.txt"); got != "ran" {
		t.Fatalf("an unchanged script ran again: %q", got)
	}
	if got := cat("conv/tree/deep/leaf.txt"); got != "leaf v2" {
		t.Fatalf("a state file that was not given lost its file: %q", got)
	}

	// State files that cannot be applied fail, say why, and change nothing.
	out, code := e.run("converge", "conv", write("bad/10-app.yaml", "nope: true\n"))
	if code == 0 {
		t.Fatalf("converging to a state file with an unknown key succeeded:\n%s", out)
	}
	mustContain(t, out, "nope")
	mustContain(t, e.ok("converge", "conv"), `"state": "converged"`)

	// What was applied outlives a stop and start.
	e.ok("stop", "conv")
	e.ok("start", "conv")
	write("state/10-app.yaml", state("v3", false))
	mustContain(t, e.ok("converge", "conv", filepath.Join(contextDir, "state")), "file:"+tg.file("conv/inline.txt"), "converged")
	if got := cat("conv/inline.txt"); got != "v3" {
		t.Fatalf("after a restart, %q", got)
	}
}
