package linuximage

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http/httptest"
	"os"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"testing"

	"github.com/google/go-containerregistry/pkg/crane"
	"github.com/google/go-containerregistry/pkg/name"
	"github.com/google/go-containerregistry/pkg/registry"
	"github.com/google/go-containerregistry/pkg/v1/mutate"
	"github.com/google/go-containerregistry/pkg/v1/remote"
)

// The artifacts vz and libkrun boot.
var (
	vzNeed   = []string{Kernel, Initrd, Root}
	krunNeed = []string{KrunKernel, Initrd, Root}
)

func TestParse(t *testing.T) {
	dir := t.TempDir()
	for _, artifact := range vzNeed {
		if err := os.WriteFile(filepath.Join(dir, artifact), []byte(artifact), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	for _, c := range []struct {
		base string
		ok   bool
		ref  string
		dir  string
		name string
	}{
		{"", true, "", "", DefaultBase},
		{"ghcr.io/example/guest:v2", true, "ghcr.io/example/guest:v2", "", "ghcr.io/example/guest:v2"},
		{dir, true, "", dir, dir},
		{filepath.Join(dir, "missing"), false, "", "", ""},
		{"not a reference!", false, "", "", ""},
	} {
		got, err := Parse(c.base, vzNeed...)
		if (err == nil) != c.ok || got.ref != c.ref || got.dir != c.dir {
			t.Errorf("Parse(%q) = %+v, %v", c.base, got, err)
			continue
		}
		if c.ok && got.String() != c.name {
			t.Errorf("Parse(%q) is named %q, want %q", c.base, got, c.name)
		}
	}
}

// pushGuest pushes a guest image of files for linux/arch to an in-process
// registry, and returns its reference and the registry.
func pushGuest(t *testing.T, files map[string][]byte, arch, repo string) (string, *httptest.Server) {
	t.Helper()
	img, err := crane.Image(files)
	if err != nil {
		t.Fatal(err)
	}
	config, err := img.ConfigFile()
	if err != nil {
		t.Fatal(err)
	}
	config.OS, config.Architecture = "linux", arch
	if img, err = mutate.ConfigFile(img, config); err != nil {
		t.Fatal(err)
	}
	server := httptest.NewServer(registry.New())
	t.Cleanup(server.Close)
	ref := strings.TrimPrefix(server.URL, "http://") + "/" + repo + ":v1"
	tag, err := name.NewTag(ref)
	if err != nil {
		t.Fatal(err)
	}
	if err := remote.Write(tag, img); err != nil {
		t.Fatal(err)
	}
	return ref, server
}

func guestFiles() map[string][]byte {
	return map[string][]byte{Kernel: []byte("kernel"), Initrd: []byte("initrd"), Root: []byte("root"), "etc/other": []byte("not an artifact")}
}

// A guest image is pulled once per digest into the cache, its artifacts and
// nothing else, and a base pinned to that digest runs with no registry, as a
// tag does from its last pull; one for another architecture, or lacking an
// artifact, is refused.
func TestPull(t *testing.T) {
	ctx := context.Background()
	cache := t.TempDir()
	ref, server := pushGuest(t, guestFiles(), "arm64", "guest")
	dir, err := Pull(ctx, cache, ref, "arm64", vzNeed...)
	if err != nil {
		t.Fatal(err)
	}
	for _, artifact := range vzNeed {
		if data, err := os.ReadFile(filepath.Join(dir, artifact)); err != nil || !bytes.Equal(data, guestFiles()[artifact]) {
			t.Fatalf("pulled %s: %q, %v", artifact, data, err)
		}
	}
	if _, err := os.Stat(filepath.Join(dir, "etc")); err == nil {
		t.Fatal("pulled a file that is not an artifact")
	}
	again, err := Pull(ctx, cache, ref, "arm64", vzNeed...)
	if err != nil || again != dir {
		t.Fatalf("a second pull of the same image: %s, %v; want %s", again, err, dir)
	}
	if _, err := Pull(ctx, cache, ref, "amd64", vzNeed...); err == nil || !strings.Contains(err.Error(), "linux/arm64, and this host boots linux/amd64") {
		t.Fatalf("an arm64 image pulled for amd64: %v", err)
	}
	partial := guestFiles()
	delete(partial, Initrd)
	partialRef, _ := pushGuest(t, partial, "arm64", "partial")
	if _, err := Pull(ctx, cache, partialRef, "arm64", vzNeed...); err == nil || !strings.Contains(err.Error(), "no initrd.img") {
		t.Fatalf("an image with no initrd: %v", err)
	}

	var src source
	if data, err := os.ReadFile(filepath.Join(dir, sourceName)); err != nil || json.Unmarshal(data, &src) != nil || src.Digest == "" {
		t.Fatalf("the cached image records no digest: %+v, %v", src, err)
	}
	pinned, err := Parse(strings.TrimSuffix(ref, ":v1") + "@" + src.Digest)
	if err != nil {
		t.Fatal(err)
	}
	server.Close()
	if got, err := pinned.Artifacts(ctx, cache, "arm64"); err != nil || got != dir {
		t.Fatalf("a base pinned to the cached digest, with no registry: %s, %v", got, err)
	}
	if got, err := Pull(ctx, cache, ref, "arm64", vzNeed...); err != nil || got != dir {
		t.Fatalf("a tag with no registry, from its last pull: %s, %v", got, err)
	}
}

// libkrun's kernel is pulled into krun/ beside the others, a directory base
// is checked for it, and an image without it, an arm64 one, is refused.
func TestPullKrunKernel(t *testing.T) {
	ctx := context.Background()
	cache := t.TempDir()
	files := guestFiles()
	files[KrunKernel] = []byte("krun kernel")
	ref, _ := pushGuest(t, files, "amd64", "guest")
	dir, err := Pull(ctx, cache, ref, "amd64", krunNeed...)
	if err != nil {
		t.Fatal(err)
	}
	if data, err := os.ReadFile(filepath.Join(dir, "krun", "vmlinux")); err != nil || string(data) != "krun kernel" {
		t.Fatalf("pulled krun/vmlinux: %q, %v", data, err)
	}
	plainRef, _ := pushGuest(t, guestFiles(), "amd64", "plain")
	if _, err := Pull(ctx, cache, plainRef, "amd64", krunNeed...); err == nil || !strings.Contains(err.Error(), "no krun/vmlinux") {
		t.Fatalf("an image with no libkrun kernel: %v", err)
	}
	if _, err := Parse(t.TempDir(), krunNeed...); err == nil || !strings.Contains(err.Error(), "no krun/vmlinux") {
		t.Fatalf("a directory with no libkrun kernel: %v", err)
	}
	img, err := Parse(dir, krunNeed...)
	if err != nil {
		t.Fatal(err)
	}
	if got, err := img.Artifacts(ctx, cache, "amd64"); err != nil || got != dir {
		t.Fatalf("a directory base: %s, %v", got, err)
	}
}

// readNewc reads a newc archive's entries: name to mode and data.
func readNewc(t *testing.T, data []byte) map[string][2]any {
	t.Helper()
	out := map[string][2]any{}
	for len(data) > 0 {
		if len(data) < 110 || string(data[:6]) != "070701" {
			t.Fatalf("not a newc header: %q", data[:min(len(data), 16)])
		}
		field := func(i int) int {
			v, err := strconv.ParseUint(string(data[6+8*i:14+8*i]), 16, 32)
			if err != nil {
				t.Fatal(err)
			}
			return int(v)
		}
		mode, size, namesize := field(1), field(6), field(11)
		nameEnd := 110 + namesize
		entry := string(data[110 : nameEnd-1])
		dataStart := (nameEnd + 3) &^ 3
		body := data[dataStart : dataStart+size]
		data = data[(dataStart+size+3)&^3:]
		if entry == "TRAILER!!!" {
			break
		}
		out[entry] = [2]any{mode, string(body)}
	}
	return out
}

// A boot's initrd is the image's, padded to four bytes, then a newc archive
// holding the agent, executable, padded to a page.
func TestWriteBootInitrd(t *testing.T) {
	dir := t.TempDir()
	initrd := filepath.Join(dir, "initrd")
	agent := filepath.Join(dir, "agent")
	if err := os.WriteFile(initrd, []byte("12345"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(agent, []byte("the agent"), 0o600); err != nil {
		t.Fatal(err)
	}
	boot := filepath.Join(dir, "boot")
	if err := WriteBootInitrd(boot, initrd, agent); err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(boot)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(data[:8], []byte("12345\x00\x00\x00")) {
		t.Fatalf("the image's initrd is not first, padded: %q", data[:8])
	}
	if len(data)%pageSize != 0 {
		t.Fatalf("the boot's initrd is %d bytes, not whole pages", len(data))
	}
	entries := readNewc(t, data[8:])
	if e, ok := entries[AgentPath]; !ok || e[0] != 0o100755 || e[1] != "the agent" {
		t.Fatalf("the agent's entry is %v (found %v)", e, ok)
	}
	if e, ok := entries[AgentDir]; !ok || e[0] != 0o040755 {
		t.Fatalf("the agent's directory entry is %v (found %v)", e, ok)
	}
}

func TestCheckAgent(t *testing.T) {
	if err := CheckAgent(filepath.Join(t.TempDir(), "missing"), "arm64"); err == nil {
		t.Fatal("a missing agent passed")
	}
	notELF := filepath.Join(t.TempDir(), "script")
	if err := os.WriteFile(notELF, []byte("#!/bin/sh\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := CheckAgent(notELF, "arm64"); err == nil {
		t.Fatal("a script passed as the agent")
	}
	if runtime.GOOS != "linux" || (runtime.GOARCH != "arm64" && runtime.GOARCH != "amd64") {
		t.Skip("the test binary is not a linux agent")
	}
	exe, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	if err := CheckAgent(exe, runtime.GOARCH); err != nil {
		t.Fatalf("a linux/%s binary: %v", runtime.GOARCH, err)
	}
	other := map[string]string{"arm64": "amd64", "amd64": "arm64"}[runtime.GOARCH]
	if err := CheckAgent(exe, other); err == nil {
		t.Fatalf("a linux/%s binary passed for %s", runtime.GOARCH, other)
	}
}

func TestLinuxCmdline(t *testing.T) {
	with := Cmdline("0123", true)
	for _, want := range []string{"root=/dev/vda", " rw ", "panic=5", "console=hvc0", "systemd.machine_id=0123"} {
		if !strings.Contains(with, want) {
			t.Errorf("%q lacks %q", with, want)
		}
	}
	if strings.Contains(with, "disco-vm.nic=0") {
		t.Errorf("a guest with a NIC is told it has none: %q", with)
	}
	if without := Cmdline("0123", false); !strings.HasSuffix(without, " disco-vm.nic=0") {
		t.Errorf("a guest with no NIC is not told: %q", without)
	}
}
