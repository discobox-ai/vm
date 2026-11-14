package agent

import (
	"bytes"
	"context"
	"debug/elf"
	"debug/macho"
	"encoding/binary"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/google/go-containerregistry/pkg/authn"
	"github.com/google/go-containerregistry/pkg/crane"
	"github.com/google/go-containerregistry/pkg/name"
	"github.com/google/go-containerregistry/pkg/registry"
	v1 "github.com/google/go-containerregistry/pkg/v1"
	"github.com/google/go-containerregistry/pkg/v1/empty"
	"github.com/google/go-containerregistry/pkg/v1/mutate"
	"github.com/google/go-containerregistry/pkg/v1/remote"
)

// elfHeader is the smallest file debug/elf opens: a linux executable for
// arch, with no sections, so Check can be passed on every OS.
func elfHeader(t *testing.T, arch string) []byte {
	t.Helper()
	machine := map[string]elf.Machine{"amd64": elf.EM_X86_64, "arm64": elf.EM_AARCH64}[arch]
	h := elf.Header64{
		Type:    uint16(elf.ET_EXEC),
		Machine: uint16(machine),
		Version: uint32(elf.EV_CURRENT),
		Ehsize:  64,
	}
	copy(h.Ident[:], elf.ELFMAG)
	h.Ident[elf.EI_CLASS] = byte(elf.ELFCLASS64)
	h.Ident[elf.EI_DATA] = byte(elf.ELFDATA2LSB)
	h.Ident[elf.EI_VERSION] = byte(elf.EV_CURRENT)
	var buf bytes.Buffer
	if err := binary.Write(&buf, binary.LittleEndian, h); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}

// machOHeader is the smallest file debug/macho opens: a Mach-O file of
// fileType for arch, with no load commands.
func machOHeader(arch string, fileType macho.Type) []byte {
	cpu := map[string]macho.Cpu{"amd64": macho.CpuAmd64, "arm64": macho.CpuArm64}[arch]
	header := make([]byte, 32)
	binary.LittleEndian.PutUint32(header, macho.Magic64)
	binary.LittleEndian.PutUint32(header[4:], uint32(cpu))
	binary.LittleEndian.PutUint32(header[12:], uint32(fileType))
	return header
}

// agentImage is an image for platform holding files.
func agentImage(t *testing.T, platform platform, files map[string][]byte) v1.Image {
	t.Helper()
	img, err := crane.Image(files)
	if err != nil {
		t.Fatal(err)
	}
	config, err := img.ConfigFile()
	if err != nil {
		t.Fatal(err)
	}
	config.OS, config.Architecture = platform.OS, platform.Architecture
	if img, err = mutate.ConfigFile(img, config); err != nil {
		t.Fatal(err)
	}
	return img
}

// pushAgent pushes an index of images, each listed for the platform its
// config names, as a release publishes the agent, to an in-process
// registry, and returns the index's digest reference.
func pushAgent(t *testing.T, server *httptest.Server, repo string, images ...v1.Image) string {
	t.Helper()
	var adds []mutate.IndexAddendum
	for _, img := range images {
		config, err := img.ConfigFile()
		if err != nil {
			t.Fatal(err)
		}
		adds = append(adds, mutate.IndexAddendum{Add: img, Platform: &v1.Platform{OS: config.OS, Architecture: config.Architecture}})
	}
	index := mutate.AppendManifests(empty.Index, adds...)
	tag, err := name.NewTag(strings.TrimPrefix(server.URL, "http://") + "/" + repo + ":v0.0.0-20261112045600-f2bec6aaadc5")
	if err != nil {
		t.Fatal(err)
	}
	if err := remote.WriteIndex(tag, index); err != nil {
		t.Fatal(err)
	}
	digest, err := index.Digest()
	if err != nil {
		t.Fatal(err)
	}
	return tag.Context().Digest(digest.String()).String()
}

// platform is a platform the agent is published for.
type platform struct{ OS, Architecture string }

func (p platform) String() string { return p.OS + "/" + p.Architecture }

var (
	linuxAMD64  = platform{"linux", "amd64"}
	linuxARM64  = platform{"linux", "arm64"}
	darwinARM64 = platform{"darwin", "arm64"}
)

// The agent is pulled by the digest of its module version's index, for each
// platform the index publishes, into an owner-only cache, and used from
// there with no registry, a relative cache taken from the working directory;
// a tag, an OS disco-vm publishes no agent for, a platform the index lacks,
// an image with no agent, and one whose agent is not an executable for its
// platform are refused.
func TestPull(t *testing.T) {
	ctx := context.Background()
	server := httptest.NewServer(registry.New())
	t.Cleanup(server.Close)
	agents := map[platform][]byte{
		linuxAMD64:  elfHeader(t, "amd64"),
		linuxARM64:  elfHeader(t, "arm64"),
		darwinARM64: machOHeader("arm64", macho.TypeExec),
	}
	ref := pushAgent(t, server, "disco-vm-agent",
		agentImage(t, linuxAMD64, map[string][]byte{File: agents[linuxAMD64], "etc/other": []byte("not the agent")}),
		agentImage(t, linuxARM64, map[string][]byte{File: agents[linuxARM64]}),
		agentImage(t, darwinARM64, map[string][]byte{File: agents[darwinARM64]}),
	)
	cache := t.TempDir()
	paths := map[platform]string{}
	for platform, want := range agents {
		path, err := Pull(ctx, cache, ref, platform.OS, platform.Architecture)
		if err != nil {
			t.Fatalf("%s: %v", platform, err)
		}
		if data, err := os.ReadFile(path); err != nil || !bytes.Equal(data, want) {
			t.Fatalf("%s: pulled %q, %v", platform, data, err)
		}
		if entries, err := os.ReadDir(filepath.Dir(path)); err != nil || len(entries) != 1 {
			t.Fatalf("%s: the cache holds %v, %v; want the agent alone", platform, entries, err)
		}
		if runtime.GOOS != "windows" {
			for _, p := range []string{filepath.Join(cache, "agent"), filepath.Dir(path), path} {
				if info, err := os.Stat(p); err != nil || info.Mode().Perm() != 0o700 {
					t.Fatalf("%s: %s is %v, %v; want owner-only", platform, p, info.Mode(), err)
				}
			}
		}
		for other, at := range paths {
			if at == path {
				t.Fatalf("%s and %s both cached at %s", platform, other, path)
			}
		}
		paths[platform] = path
	}

	tagRef := strings.SplitN(ref, "@", 2)[0] + ":v0.0.0-20261112045600-f2bec6aaadc5"
	if _, err := Pull(ctx, t.TempDir(), tagRef, "linux", "amd64"); err == nil || !strings.Contains(err.Error(), "not pinned by digest") {
		t.Fatalf("an agent by tag: %v", err)
	}
	if _, err := Pull(ctx, t.TempDir(), ref, "windows", "amd64"); err == nil || !strings.Contains(err.Error(), "no agent for windows") {
		t.Fatalf("an agent for an OS disco-vm publishes none for: %v", err)
	}
	if _, err := Pull(ctx, t.TempDir(), ref, "linux", "riscv64"); err == nil {
		t.Fatal("an agent for a platform the index lacks")
	}
	if _, err := Pull(ctx, t.TempDir(), ref, "darwin", "amd64"); err == nil {
		t.Fatal("a darwin/amd64 agent from an index with darwin/arm64 alone")
	}
	linuxOnly := pushAgent(t, server, "linux-only", agentImage(t, linuxARM64, map[string][]byte{File: agents[linuxARM64]}))
	if _, err := Pull(ctx, t.TempDir(), linuxOnly, "linux", "amd64"); err == nil {
		t.Fatal("an arm64-only agent pulled for amd64")
	}
	if _, err := Pull(ctx, t.TempDir(), linuxOnly, "darwin", "arm64"); err == nil || !strings.Contains(err.Error(), "darwin/arm64") {
		t.Fatalf("a darwin agent from an index without one: %v", err)
	}
	// A registry hands back an image that is no index whatever platform was
	// asked for.
	image := agentImage(t, linuxARM64, map[string][]byte{File: agents[linuxARM64]})
	imageTag, err := name.NewTag(strings.TrimPrefix(server.URL, "http://") + "/image:v0.0.0-20261112045600-f2bec6aaadc5")
	if err != nil {
		t.Fatal(err)
	}
	if err := remote.Write(imageTag, image); err != nil {
		t.Fatal(err)
	}
	imageDigest, err := image.Digest()
	if err != nil {
		t.Fatal(err)
	}
	if _, err := Pull(ctx, t.TempDir(), imageTag.Context().Digest(imageDigest.String()).String(), "darwin", "arm64"); err == nil || !strings.Contains(err.Error(), "is for linux/arm64, not darwin/arm64") {
		t.Fatalf("a linux image pulled as the darwin agent: %v", err)
	}
	none := pushAgent(t, server, "none", agentImage(t, linuxAMD64, map[string][]byte{"bin/disco-vm": agents[linuxAMD64]}))
	if _, err := Pull(ctx, t.TempDir(), none, "linux", "amd64"); err == nil || !strings.Contains(err.Error(), "holds no disco-vm") {
		t.Fatalf("an image with no agent at its root: %v", err)
	}
	for name, tc := range map[string]struct {
		platform platform
		agent    []byte
		want     string
	}{
		"an arm64 agent in an amd64 image":      {linuxAMD64, agents[linuxARM64], "not a linux/amd64 executable"},
		"a Mach-O agent in a linux image":       {linuxARM64, agents[darwinARM64], "not a linux executable"},
		"a linux agent in a darwin image":       {darwinARM64, agents[linuxARM64], "not a Mach-O executable"},
		"a Mach-O library in a darwin image":    {darwinARM64, machOHeader("arm64", macho.TypeDylib), "not a darwin/arm64 executable"},
		"an amd64 Mach-O in a darwin/arm64 one": {darwinARM64, machOHeader("amd64", macho.TypeExec), "not a darwin/arm64 executable"},
		"a script in a darwin image":            {darwinARM64, []byte("#!/bin/sh\n"), "not a Mach-O executable"},
	} {
		wrong := pushAgent(t, server, "wrong", agentImage(t, tc.platform, map[string][]byte{File: tc.agent}))
		if _, err := Pull(ctx, t.TempDir(), wrong, tc.platform.OS, tc.platform.Architecture); err == nil || !strings.Contains(err.Error(), tc.want) {
			t.Errorf("%s: %v, want a refusal saying %q", name, err, tc.want)
		}
	}

	t.Chdir(t.TempDir())
	if got, err := Pull(ctx, "relative", ref, "linux", "amd64"); err != nil || !filepath.IsAbs(got) {
		t.Fatalf("a relative cache: %s, %v; want an absolute path", got, err)
	}

	server.Close()
	for platform, want := range paths {
		if got, err := Pull(ctx, cache, ref, platform.OS, platform.Architecture); err != nil || got != want {
			t.Fatalf("%s with no registry: %s, %v; want %s", platform, got, err, want)
		}
	}
}

// Check takes a linux ELF executable for a linux agent and a Mach-O one for
// a darwin agent, each for its architecture, and refuses anything else.
func TestCheck(t *testing.T) {
	dir := t.TempDir()
	write := func(name string, data []byte) string {
		path := filepath.Join(dir, name)
		if err := os.WriteFile(path, data, 0o600); err != nil {
			t.Fatal(err)
		}
		return path
	}
	elfARM64 := write("elf-arm64", elfHeader(t, "arm64"))
	machOARM64 := write("macho-arm64", machOHeader("arm64", macho.TypeExec))
	for _, ok := range []struct{ path, goos, arch string }{
		{elfARM64, "linux", "arm64"},
		{write("elf-amd64", elfHeader(t, "amd64")), "linux", "amd64"},
		{machOARM64, "darwin", "arm64"},
	} {
		if err := Check(ok.path, ok.goos, ok.arch); err != nil {
			t.Errorf("%s as %s/%s: %v", filepath.Base(ok.path), ok.goos, ok.arch, err)
		}
	}
	script := write("script", []byte("#!/bin/sh\n"))
	for _, bad := range []struct{ path, goos, arch string }{
		{filepath.Join(dir, "missing"), "linux", "arm64"},
		{"relative", "linux", "arm64"},
		{dir, "darwin", "arm64"},
		{script, "linux", "arm64"},
		{script, "darwin", "arm64"},
		{elfARM64, "linux", "amd64"},
		{elfARM64, "darwin", "arm64"},
		{machOARM64, "linux", "arm64"},
		{machOARM64, "darwin", "amd64"},
		{write("dylib", machOHeader("arm64", macho.TypeDylib)), "darwin", "arm64"},
		{elfARM64, "windows", "arm64"},
	} {
		if err := Check(bad.path, bad.goos, bad.arch); err == nil {
			t.Errorf("%s passed as %s/%s", bad.path, bad.goos, bad.arch)
		}
	}
	if runtime.GOOS != "linux" && runtime.GOOS != "darwin" {
		return
	}
	// The test binary itself is a real executable of this host's platform.
	exe, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	if err := Check(exe, runtime.GOOS, runtime.GOARCH); err != nil {
		t.Fatalf("this %s/%s test binary: %v", runtime.GOOS, runtime.GOARCH, err)
	}
	other := map[string]string{"arm64": "amd64", "amd64": "arm64"}[runtime.GOARCH]
	if err := Check(exe, runtime.GOOS, other); err == nil {
		t.Fatalf("a %s/%s binary passed for %s", runtime.GOOS, runtime.GOARCH, other)
	}
}

// TestPullPublishedAgent fetches a published agent by its digest, as
// $DISCO_VM_AGENT_IMAGE names it: every platform its index lists, each of
// $DISCO_VM_AGENT_PLATFORMS (os/arch, space-separated) required among them.
// It runs the one this host can, to check it is the module version
// $DISCO_VM_AGENT_VERSION names. A release runs it on what it just pushed
// (release:agent).
func TestPullPublishedAgent(t *testing.T) {
	ref := os.Getenv("DISCO_VM_AGENT_IMAGE")
	if ref == "" {
		t.Skip("DISCO_VM_AGENT_IMAGE names no published agent")
	}
	d, err := name.NewDigest(ref)
	if err != nil {
		t.Fatal(err)
	}
	index, err := remote.Index(d, remote.WithContext(t.Context()), remote.WithAuthFromKeychain(authn.DefaultKeychain))
	if err != nil {
		t.Fatal(err)
	}
	manifest, err := index.IndexManifest()
	if err != nil {
		t.Fatal(err)
	}
	listed := map[string]bool{}
	for _, m := range manifest.Manifests {
		// buildx lists each image's attestation as unknown/unknown.
		if m.Platform == nil || m.Platform.OS == "unknown" {
			continue
		}
		listed[m.Platform.OS+"/"+m.Platform.Architecture] = true
	}
	for want := range strings.FieldsSeq(os.Getenv("DISCO_VM_AGENT_PLATFORMS")) {
		if !listed[want] {
			t.Errorf("%s lists no %s agent (it lists %v)", ref, want, listed)
		}
	}
	cache := t.TempDir()
	for platform := range listed {
		goos, arch, _ := strings.Cut(platform, "/")
		path, err := Pull(t.Context(), cache, ref, goos, arch)
		if err != nil {
			t.Fatalf("%s: %v", platform, err)
		}
		t.Logf("%s: %s", platform, path)
		if runtime.GOOS != goos || runtime.GOARCH != arch {
			continue
		}
		out, err := exec.CommandContext(t.Context(), path, "--version").CombinedOutput() //nolint:gosec // G204: the agent just pulled
		if err != nil {
			t.Fatalf("%s --version: %v: %s", path, err, out)
		}
		if want := os.Getenv("DISCO_VM_AGENT_VERSION"); want != "" && !strings.Contains(string(out), " "+want+"\n") {
			t.Fatalf("%s --version: %q, want %s", path, out, want)
		}
	}
}
