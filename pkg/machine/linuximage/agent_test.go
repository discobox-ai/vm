package linuximage

import (
	"bytes"
	"context"
	"debug/elf"
	"encoding/binary"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/google/go-containerregistry/pkg/crane"
	"github.com/google/go-containerregistry/pkg/name"
	"github.com/google/go-containerregistry/pkg/registry"
	v1 "github.com/google/go-containerregistry/pkg/v1"
	"github.com/google/go-containerregistry/pkg/v1/empty"
	"github.com/google/go-containerregistry/pkg/v1/mutate"
	"github.com/google/go-containerregistry/pkg/v1/remote"
)

// elfHeader is the smallest file debug/elf opens: a linux executable for
// arch, with no sections, so CheckAgent can be passed on every OS.
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

// agentImage is an image for linux/arch holding files.
func agentImage(t *testing.T, arch string, files map[string][]byte) v1.Image {
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
	return img
}

// pushAgent pushes an index of images, as a release publishes the agent, to
// an in-process registry, and returns the index's digest reference.
func pushAgent(t *testing.T, server *httptest.Server, repo string, images map[string]v1.Image) string {
	t.Helper()
	var adds []mutate.IndexAddendum
	for arch, img := range images {
		adds = append(adds, mutate.IndexAddendum{Add: img, Platform: &v1.Platform{OS: "linux", Architecture: arch}})
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

// The agent is pulled by the digest of its module version's index, for each
// architecture, into an owner-only cache, and used from there with no
// registry, a relative cache taken from the working directory; a tag, an
// image for another platform, one with no agent, and one whose agent is for
// another architecture are refused.
func TestPullAgent(t *testing.T) {
	ctx := context.Background()
	server := httptest.NewServer(registry.New())
	t.Cleanup(server.Close)
	agents := map[string][]byte{"amd64": elfHeader(t, "amd64"), "arm64": elfHeader(t, "arm64")}
	ref := pushAgent(t, server, "disco-vm-agent", map[string]v1.Image{
		"amd64": agentImage(t, "amd64", map[string][]byte{AgentFile: agents["amd64"], "etc/other": []byte("not the agent")}),
		"arm64": agentImage(t, "arm64", map[string][]byte{AgentFile: agents["arm64"]}),
	})
	cache := t.TempDir()
	paths := map[string]string{}
	for arch, want := range agents {
		path, err := PullAgent(ctx, cache, ref, arch)
		if err != nil {
			t.Fatalf("%s: %v", arch, err)
		}
		if data, err := os.ReadFile(path); err != nil || !bytes.Equal(data, want) {
			t.Fatalf("%s: pulled %q, %v", arch, data, err)
		}
		if entries, err := os.ReadDir(filepath.Dir(path)); err != nil || len(entries) != 1 {
			t.Fatalf("%s: the cache holds %v, %v; want the agent alone", arch, entries, err)
		}
		if runtime.GOOS != "windows" {
			for _, p := range []string{filepath.Join(cache, "agent"), filepath.Dir(path), path} {
				if info, err := os.Stat(p); err != nil || info.Mode().Perm() != 0o700 {
					t.Fatalf("%s: %s is %v, %v; want owner-only", arch, p, info.Mode(), err)
				}
			}
		}
		paths[arch] = path
	}
	if paths["amd64"] == paths["arm64"] {
		t.Fatalf("both architectures cached at %s", paths["amd64"])
	}

	tagRef := strings.SplitN(ref, "@", 2)[0] + ":v0.0.0-20261112045600-f2bec6aaadc5"
	if _, err := PullAgent(ctx, t.TempDir(), tagRef, "amd64"); err == nil || !strings.Contains(err.Error(), "not pinned by digest") {
		t.Fatalf("an agent by tag: %v", err)
	}
	if _, err := PullAgent(ctx, t.TempDir(), ref, "riscv64"); err == nil {
		t.Fatal("an agent for a platform the index lacks")
	}
	single := pushAgent(t, server, "single", map[string]v1.Image{"arm64": agentImage(t, "arm64", map[string][]byte{AgentFile: agents["arm64"]})})
	if _, err := PullAgent(ctx, t.TempDir(), single, "amd64"); err == nil {
		t.Fatal("an arm64-only agent pulled for amd64")
	}
	none := pushAgent(t, server, "none", map[string]v1.Image{"amd64": agentImage(t, "amd64", map[string][]byte{"bin/disco-vm": agents["amd64"]})})
	if _, err := PullAgent(ctx, t.TempDir(), none, "amd64"); err == nil || !strings.Contains(err.Error(), "holds no disco-vm") {
		t.Fatalf("an image with no agent at its root: %v", err)
	}
	wrong := pushAgent(t, server, "wrong", map[string]v1.Image{"amd64": agentImage(t, "amd64", map[string][]byte{AgentFile: agents["arm64"]})})
	if _, err := PullAgent(ctx, t.TempDir(), wrong, "amd64"); err == nil || !strings.Contains(err.Error(), "not a linux/amd64 executable") {
		t.Fatalf("an arm64 agent in an amd64 image: %v", err)
	}

	t.Chdir(t.TempDir())
	if got, err := PullAgent(ctx, "relative", ref, "amd64"); err != nil || !filepath.IsAbs(got) {
		t.Fatalf("a relative cache: %s, %v; want an absolute path", got, err)
	}

	server.Close()
	for arch, want := range paths {
		if got, err := PullAgent(ctx, cache, ref, arch); err != nil || got != want {
			t.Fatalf("%s with no registry: %s, %v; want %s", arch, got, err, want)
		}
	}
}

// TestPullPublishedAgent fetches a published agent by its digest, as
// $DISCO_VM_AGENT_IMAGE names it, for both architectures, and runs the one
// this host can to check it is the module version $DISCO_VM_AGENT_VERSION
// names. A release runs it on what it just pushed (release:linux-agent).
func TestPullPublishedAgent(t *testing.T) {
	ref := os.Getenv("DISCO_VM_AGENT_IMAGE")
	if ref == "" {
		t.Skip("DISCO_VM_AGENT_IMAGE names no published agent")
	}
	cache := t.TempDir()
	for _, arch := range []string{"amd64", "arm64"} {
		path, err := PullAgent(context.Background(), cache, ref, arch)
		if err != nil {
			t.Fatalf("%s: %v", arch, err)
		}
		if runtime.GOOS != "linux" || runtime.GOARCH != arch {
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
