package vz

import (
	"bytes"
	"context"
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

	"github.com/discobox-ai/vm/pkg/machine"
)

func TestLinuxBase(t *testing.T) {
	dir := t.TempDir()
	for _, artifact := range linuxArtifacts {
		if err := os.WriteFile(filepath.Join(dir, artifact), []byte(artifact), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	for _, c := range []struct {
		ref   string
		found bool
		image string
		dir   string
	}{
		{LinuxBaseName, true, DefaultLinuxBase, ""},
		{"ghcr.io/example/guest:v2", true, "ghcr.io/example/guest:v2", ""},
		{dir, true, "", dir},
		{filepath.Join(dir, "missing"), false, "", ""},
		{"not a reference!", false, "", ""},
	} {
		b, found := linuxBase(c.ref)
		if found != c.found || b.Options["image"] != c.image || b.Options["dir"] != c.dir {
			t.Errorf("linuxBase(%q) = %+v, %v", c.ref, b, found)
			continue
		}
		if found && b.GuestOS != machine.Linux {
			t.Errorf("linuxBase(%q) is a %s guest", c.ref, b.GuestOS)
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
	return map[string][]byte{kernelName: []byte("kernel"), initrdName: []byte("initrd"), rootName: []byte("root"), "etc/other": []byte("not an artifact")}
}

// A guest image is pulled once per digest into the cache, its artifacts and
// nothing else, and listed as a base pinned to that digest, which runs with
// no registry, as a tag does from its last pull; one for another
// architecture, or lacking an artifact, is refused.
func TestPullLinuxImage(t *testing.T) {
	ctx := context.Background()
	cache := t.TempDir()
	ref, server := pushGuest(t, guestFiles(), "arm64", "guest")
	dir, err := pullLinuxImage(ctx, cache, ref, "arm64")
	if err != nil {
		t.Fatal(err)
	}
	for _, artifact := range linuxArtifacts {
		if data, err := os.ReadFile(filepath.Join(dir, artifact)); err != nil || !bytes.Equal(data, guestFiles()[artifact]) {
			t.Fatalf("pulled %s: %q, %v", artifact, data, err)
		}
	}
	if _, err := os.Stat(filepath.Join(dir, "etc")); err == nil {
		t.Fatal("pulled a file that is not an artifact")
	}
	again, err := pullLinuxImage(ctx, cache, ref, "arm64")
	if err != nil || again != dir {
		t.Fatalf("a second pull of the same image: %s, %v; want %s", again, err, dir)
	}
	if _, err := pullLinuxImage(ctx, cache, ref, "amd64"); err == nil || !strings.Contains(err.Error(), "linux/arm64, and this Mac boots linux/amd64") {
		t.Fatalf("an arm64 image pulled for amd64: %v", err)
	}
	partial := guestFiles()
	delete(partial, initrdName)
	partialRef, _ := pushGuest(t, partial, "arm64", "partial")
	if _, err := pullLinuxImage(ctx, cache, partialRef, "arm64"); err == nil || !strings.Contains(err.Error(), "no initrd.img") {
		t.Fatalf("an image with no initrd: %v", err)
	}

	var listed *machine.Base
	for _, b := range linuxBases(cache) {
		if strings.HasPrefix(b.Name, strings.TrimSuffix(ref, ":v1")+"@sha256:") {
			listed = &b
		}
	}
	if listed == nil || listed.Options["image"] != listed.Name {
		t.Fatalf("the cached image is not listed pinned to its digest: %+v", linuxBases(cache))
	}
	server.Close()
	if got, err := linuxArtifactDir(ctx, cache, *listed, "arm64"); err != nil || got != dir {
		t.Fatalf("a listed base with no registry: %s, %v", got, err)
	}
	if got, err := pullLinuxImage(ctx, cache, ref, "arm64"); err != nil || got != dir {
		t.Fatalf("a tag with no registry, from its last pull: %s, %v", got, err)
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
// holding the agent, executable.
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
	if err := writeBootInitrd(boot, initrd, agent); err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(boot)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(data[:8], []byte("12345\x00\x00\x00")) {
		t.Fatalf("the image's initrd is not first, padded: %q", data[:8])
	}
	entries := readNewc(t, data[8:])
	if e, ok := entries[agentPath]; !ok || e[0] != 0o100755 || e[1] != "the agent" {
		t.Fatalf("the agent's entry is %v (found %v)", e, ok)
	}
	if e, ok := entries[agentDir]; !ok || e[0] != 0o040755 {
		t.Fatalf("the agent's directory entry is %v (found %v)", e, ok)
	}
}

func TestCheckAgent(t *testing.T) {
	if err := checkAgent("", "arm64"); err == nil || !strings.Contains(err.Error(), AgentEnv) {
		t.Fatalf("no agent: %v, want it to name %s", err, AgentEnv)
	}
	notELF := filepath.Join(t.TempDir(), "script")
	if err := os.WriteFile(notELF, []byte("#!/bin/sh\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := checkAgent(notELF, "arm64"); err == nil {
		t.Fatal("a script passed as the agent")
	}
	if runtime.GOOS != "linux" || (runtime.GOARCH != "arm64" && runtime.GOARCH != "amd64") {
		t.Skip("the test binary is not a linux agent")
	}
	exe, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	if err := checkAgent(exe, runtime.GOARCH); err != nil {
		t.Fatalf("a linux/%s binary: %v", runtime.GOARCH, err)
	}
	other := map[string]string{"arm64": "amd64", "amd64": "arm64"}[runtime.GOARCH]
	if err := checkAgent(exe, other); err == nil {
		t.Fatalf("a linux/%s binary passed for %s", runtime.GOARCH, other)
	}
}

func TestLinuxCmdline(t *testing.T) {
	with := linuxCmdline("0123", true)
	for _, want := range []string{"root=/dev/vda", " rw ", "panic=5", "console=hvc0", "systemd.machine_id=0123"} {
		if !strings.Contains(with, want) {
			t.Errorf("%q lacks %q", with, want)
		}
	}
	if strings.Contains(with, "disco-vm.nic=0") {
		t.Errorf("a guest with a NIC is told it has none: %q", with)
	}
	if without := linuxCmdline("0123", false); !strings.HasSuffix(without, " disco-vm.nic=0") {
		t.Errorf("a guest with no NIC is not told: %q", without)
	}
}
