package engine_test

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/discobox-ai/vm/pkg/build"
	"github.com/discobox-ai/vm/pkg/engine"
	"github.com/discobox-ai/vm/pkg/machine/fake"
)

// A program that embeds the engine starts its shims as a subcommand of its
// own (ShimCommand). Here that program is a script, which records how it was
// called and then runs the real shim.
func TestShimCommand(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("the stand-in program is a shell script")
	}
	dir := t.TempDir()
	binary := filepath.Join(dir, "disco-vm")
	if out, err := exec.Command("go", "build", "-o", binary, "../../cmd/disco-vm").CombinedOutput(); err != nil {
		t.Fatalf("build: %v\n%s", err, out)
	}
	root := filepath.Join(dir, "root")
	mark := filepath.Join(dir, "called")
	script := filepath.Join(dir, "embedder")
	body := "#!/bin/sh\necho \"$@\" > " + mark + "\nshift\nexec " + binary + " --root " + root + " --driver fake shim \"$@\"\n"
	if err := os.WriteFile(script, []byte(body), 0o755); err != nil {
		t.Fatal(err)
	}

	e, err := engine.Open(root, &fake.Driver{Agent: binary})
	if err != nil {
		t.Fatal(err)
	}
	e.ShimCommand = []string{script, "vm-shim"}
	spec := filepath.Join(dir, "spec.yaml")
	if err := os.WriteFile(spec, []byte("from:\n  install: {os: "+runtime.GOOS+", media: latest}\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	if _, err := (&build.Builder{Engine: e, Out: os.Stderr}).Build(ctx, build.Options{File: spec, Tags: []string{"t"}}); err != nil {
		t.Fatal(err)
	}
	inst, err := e.Create(ctx, "t", engine.CreateOptions{Name: "one"})
	if err != nil {
		t.Fatal(err)
	}
	if err := e.Start(ctx, inst, engine.StartOptions{Timeout: 30 * time.Second}); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = e.Remove(context.Background(), inst, true) })
	called, _ := os.ReadFile(mark)
	if got := strings.TrimSpace(string(called)); got != "vm-shim "+inst.ID {
		t.Fatalf("the embedder was called with %q, want %q", got, "vm-shim "+inst.ID)
	}
	if err := e.Guest(inst).Health(ctx); err != nil {
		t.Fatalf("the instance its shim runs does not answer: %v", err)
	}
}

func TestShimMainArgs(t *testing.T) {
	e := &engine.Engine{}
	for _, args := range [][]string{nil, {"a", "b"}, {"--gui", "--warm", "x"}} {
		if err := e.ShimMain(context.Background(), args); err == nil || !strings.Contains(err.Error(), "want [--gui] INSTANCE") {
			t.Errorf("ShimMain(%q) = %v, want a usage error", args, err)
		}
	}
}
