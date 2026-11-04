package docker

import (
	"errors"
	"os"
	"path/filepath"
	"testing"

	"golang.org/x/sys/unix"

	"github.com/discobox-ai/vm/pkg/machine"
)

// TestDirs takes the helper's steps on a directory of the test's: a clone
// staged from a key's generation and put in place, a guarded save that
// moves the key and prunes its old generation, one refused once it has
// moved, a listing, a delete, and a removal.
func TestDirs(t *testing.T) {
	root := t.TempDir()
	l := layout{root: root, pool: "p"}
	step := func(req dirsRequest) (dirsResult, error) {
		t.Helper()
		req.Pool = "p"
		return req.run(root)
	}
	must := func(res dirsResult, err error) dirsResult {
		t.Helper()
		if err != nil {
			t.Fatal(err)
		}
		return res
	}
	staged := must(step(dirsRequest{Op: opPrepare, Name: "vm", Data: true, Key: "k"}))
	if staged.Generation != "" || staged.Staging == "" {
		t.Fatalf("prepare of a key with no generation: %+v", staged)
	}
	must(step(dirsRequest{Op: opInstall, Name: "vm", Data: true, Key: "k", Staging: staged.Staging}))
	cache := l.vm(machine.VolumeCache, "vm")
	if err := os.WriteFile(filepath.Join(cache, "f"), []byte("one"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink("f", filepath.Join(cache, "link")); err != nil {
		t.Fatal(err)
	}
	first := must(step(dirsRequest{Op: opSave, Name: "vm", Key: "k", Generation: "g1", Guarded: true}))
	if first.Saved.Generation != "g1" {
		t.Fatalf("saved %+v", first.Saved)
	}
	if _, err := step(dirsRequest{Op: opSave, Name: "vm", Key: "k", Generation: "g2", Guarded: true}); !errors.Is(err, machine.ErrCacheMoved) {
		t.Fatalf("a save guarded on no generation once the key holds one: %v", err)
	}
	// What a save that died left is swept by the next save's prune, and the
	// copy of a save still under way, whose lock it holds, is not.
	dead, live := filepath.Join(l.key("k"), savePrefix+"dead"), filepath.Join(l.key("k"), savePrefix+"live")
	for _, dir := range []string{dead, live} {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	held, err := os.Create(live + ".lock")
	if err != nil {
		t.Fatal(err)
	}
	defer held.Close()
	if err := unix.Flock(int(held.Fd()), unix.LOCK_EX); err != nil {
		t.Fatal(err)
	}
	must(step(dirsRequest{Op: opSave, Name: "vm", Key: "k", Generation: "g3", Guarded: true, IfGeneration: "g1"}))
	if _, err := os.Stat(dead); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("a dead save's copy is kept: %v", err)
	}
	if _, err := os.Stat(live); err != nil {
		t.Fatalf("a live save's copy is swept: %v", err)
	}
	if _, err := os.Stat(filepath.Join(l.key("k"), savePrefix+"g3.lock")); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("a finished save's lock is kept: %v", err)
	}
	if _, err := os.Stat(filepath.Join(l.key("k"), "g1")); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("the generation the key moved off is kept: %v", err)
	}
	caches := must(step(dirsRequest{Op: opCaches})).Caches
	if len(caches) != 1 || caches[0].Key != "k" || caches[0].Generation != "g3" {
		t.Fatalf("caches %+v", caches)
	}
	again := must(step(dirsRequest{Op: opPrepare, Name: "vm2", Key: "k"}))
	if again.Generation != "g3" {
		t.Fatalf("a clone of %+v", again)
	}
	staging := filepath.Join(l.vms(machine.VolumeCache), again.Staging)
	if link, err := os.Readlink(filepath.Join(staging, "link")); err != nil || link != "f" {
		t.Fatalf("the clone's symlink: %q, %v", link, err)
	}
	must(step(dirsRequest{Op: opUnstage, Staging: again.Staging}))
	if _, err := os.Stat(staging); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("an unstaged clone is kept: %v", err)
	}
	must(step(dirsRequest{Op: opDeleteCache, Key: "k"}))
	if caches := must(step(dirsRequest{Op: opCaches})).Caches; len(caches) != 0 {
		t.Fatalf("caches once deleted: %+v", caches)
	}
	must(step(dirsRequest{Op: opRemove, Name: "vm"}))
	for _, dir := range []string{cache, l.vm(machine.VolumeData, "vm")} {
		if _, err := os.Stat(dir); !errors.Is(err, os.ErrNotExist) {
			t.Fatalf("%s outlives its VM: %v", dir, err)
		}
	}
	if _, err := step(dirsRequest{Op: opRemove, Name: "../x"}); err == nil {
		t.Fatal("a name that is not one was taken")
	}
}
