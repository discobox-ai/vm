package agent

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

// carry writes data as the goos/arch agent a disco-vm carries, in a new
// directory it returns.
func carry(t *testing.T, data []byte, goos, arch string) string {
	t.Helper()
	src := filepath.Join(t.TempDir(), "disco-vm")
	if err := os.WriteFile(src, data, 0o755); err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	if err := Carry(dir, src, goos, arch); err != nil {
		t.Fatal(err)
	}
	return dir
}

// Unpack writes a carried agent out once, owner-only, named by its SHA-256
// beside Pull's; uses a good one already there; replaces a broken one; and
// finds none where none is carried.
func TestUnpack(t *testing.T) {
	data := elfHeader(t, "arm64")
	carried := os.DirFS(carry(t, data, "linux", "arm64"))
	cache := t.TempDir()
	sum := sha256.Sum256(data)
	want := filepath.Join(cache, "agent", "sha256-"+hex.EncodeToString(sum[:])+"-linux-arm64", File)
	path, err := Unpack(cache, carried, "linux", "arm64")
	if err != nil || path != want {
		t.Fatalf("Unpack: %s, %v; want %s", path, err, want)
	}
	if got, err := os.ReadFile(path); err != nil || !bytes.Equal(got, data) {
		t.Fatalf("wrote %q, %v", got, err)
	}
	if entries, err := os.ReadDir(filepath.Join(cache, "agent")); err != nil || len(entries) != 1 {
		t.Fatalf("the cache holds %v, %v; want the agent's directory alone", entries, err)
	}
	if runtime.GOOS != "windows" {
		for _, p := range []string{filepath.Join(cache, "agent"), filepath.Dir(path), path} {
			if info, err := os.Stat(p); err != nil || info.Mode().Perm() != 0o700 {
				t.Fatalf("%s is %v, %v; want owner-only", p, info.Mode(), err)
			}
		}
	}
	// One already there is used as it is.
	before, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if again, err := Unpack(cache, carried, "linux", "arm64"); err != nil || again != path {
		t.Fatalf("again: %s, %v", again, err)
	}
	if after, err := os.Stat(path); err != nil || !os.SameFile(before, after) {
		t.Fatalf("again: the agent was written anew (%v)", err)
	}
	// A broken one is replaced.
	if err := os.WriteFile(path, []byte("broken"), 0o700); err != nil {
		t.Fatal(err)
	}
	if again, err := Unpack(cache, carried, "linux", "arm64"); err != nil || again != path {
		t.Fatalf("over a broken one: %s, %v", again, err)
	}
	if got, err := os.ReadFile(path); err != nil || !bytes.Equal(got, data) {
		t.Fatalf("over a broken one: %q, %v", got, err)
	}
	// None carried for the platform, or nothing carried at all.
	for name, c := range map[string]struct {
		dir        string
		goos, arch string
	}{
		"another architecture": {"x", "linux", "amd64"},
		"another OS":           {"x", "darwin", "arm64"},
		"nothing":              {"", "linux", "arm64"},
	} {
		fsys := carried
		if c.dir == "" {
			fsys = nil
		}
		if path, err := Unpack(t.TempDir(), fsys, c.goos, c.arch); err != nil || path != "" {
			t.Errorf("%s: %q, %v; want none", name, path, err)
		}
	}
	if !Carries(carried, "linux", "arm64") || Carries(carried, "linux", "amd64") || Carries(nil, "linux", "arm64") {
		t.Error("Carries says otherwise than what is carried")
	}
}

// What is carried is refused unless it is the agent its SHA-256 records,
// and an executable for its platform; Carry refuses one that is not.
func TestUnpackRefuses(t *testing.T) {
	dir := carry(t, elfHeader(t, "arm64"), "linux", "arm64")
	other := carry(t, elfHeader(t, "amd64"), "linux", "amd64")
	// The amd64 agent under arm64's digest.
	gz, err := os.ReadFile(filepath.Join(other, "linux-amd64.gz"))
	if err != nil {
		t.Fatal(err)
	}
	tampered := t.TempDir()
	for name, data := range map[string][]byte{"linux-arm64.gz": gz} {
		if err := os.WriteFile(filepath.Join(tampered, name), data, 0o644); err != nil {
			t.Fatal(err)
		}
	}
	digest, err := os.ReadFile(filepath.Join(dir, "linux-arm64.sha256"))
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(tampered, "linux-arm64.sha256"), digest, 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := Unpack(t.TempDir(), os.DirFS(tampered), "linux", "arm64"); err == nil || !strings.Contains(err.Error(), "not the sha256:") {
		t.Fatalf("another agent under the recorded digest: %v, want refused", err)
	}
	if err := os.WriteFile(filepath.Join(tampered, "linux-arm64.sha256"), []byte("nope\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := Unpack(t.TempDir(), os.DirFS(tampered), "linux", "arm64"); err == nil {
		t.Fatal("a recorded digest that is not one: unpacked")
	}
	// Carried as arm64, though it is amd64: its digest is its own, and
	// Check refuses it.
	mislabeled := t.TempDir()
	for _, ext := range []string{".gz", ".sha256"} {
		data, err := os.ReadFile(filepath.Join(other, "linux-amd64"+ext))
		if err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(mislabeled, "linux-arm64"+ext), data, 0o644); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := Unpack(t.TempDir(), os.DirFS(mislabeled), "linux", "arm64"); err == nil || !strings.Contains(err.Error(), "not a linux/arm64 executable") {
		t.Fatalf("another architecture's agent: %v, want refused", err)
	}
	src := filepath.Join(t.TempDir(), "script")
	if err := os.WriteFile(src, []byte("#!/bin/sh\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := Carry(t.TempDir(), src, "linux", "arm64"); err == nil {
		t.Fatal("Carry took a script")
	}
}
