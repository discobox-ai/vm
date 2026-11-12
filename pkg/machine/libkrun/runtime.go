//go:build linux

package libkrun

import (
	"archive/tar"
	"bytes"
	"context"
	"debug/elf"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"path"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
	"time"

	"github.com/google/go-containerregistry/pkg/authn"
	"github.com/google/go-containerregistry/pkg/name"
	v1 "github.com/google/go-containerregistry/pkg/v1"
	"github.com/google/go-containerregistry/pkg/v1/mutate"
	"github.com/google/go-containerregistry/pkg/v1/remote"
)

// DefaultRuntime is the libkrun runtime the driver runs its VMs with (ADR
// 26-11-03-530 §3): disco-vm's own (pkg/machine/libkrun/runtime), from its
// krun/v* release line.
//
// It is a tag until the first release is published and its digest pinned
// here; the ADR pins it by digest, and the pin moves by hand.
const DefaultRuntime = "ghcr.io/discobox-ai/disco-vm-krun:v1"

// launchVersion is the version of the launcher's configuration (launch.conf)
// this driver writes, which a runtime's runtime.json and its launcher's
// --version must name.
const launchVersion = "1"

// A runtime's files.
const (
	launcherName = "bin/krun-launch"
	passtName    = "bin/passt"
	libDir       = "lib"
	manifestName = "runtime.json"
	// runtimeSourceName records, in a cached runtime, what it was pulled as.
	runtimeSourceName = "source.json"
)

// loaders is the runtime's dynamic loader, in lib/, per host architecture.
// The runtime is published for amd64 alone (ADR 26-11-03-530 §10).
var loaders = map[string]string{"amd64": "ld-linux-x86-64.so.2", "arm64": "ld-linux-aarch64.so.1"}

// runtimeFiles are what a runtime directory must hold.
func runtimeFiles() []string {
	return []string{launcherName, passtName, path.Join(libDir, loaders[runtime.GOARCH]), path.Join(libDir, "libkrun.so.1"), manifestName}
}

// manifest is a runtime's runtime.json.
type manifest struct {
	Version string `json:"version"`
	Libkrun string `json:"libkrun"`
	Passt   string `json:"passt"`
	Config  string `json:"config"`
}

// krunRuntime is a runtime directory, checked complete.
type krunRuntime string

func (r krunRuntime) path(name string) string {
	return filepath.Join(string(r), filepath.FromSlash(name))
}

// command runs the runtime's launcher with args through the runtime's own
// loader, so nothing of the host's userland is loaded (ADR 26-11-03-530 §4).
func (r krunRuntime) command(ctx context.Context, args ...string) *exec.Cmd {
	lib := r.path(libDir)
	// --inhibit-cache keeps the loader out of the host's ld.so.cache. It
	// still searches the host's /lib for a library lib/ lacks, so
	// completeRuntime checks that lib/ has every one.
	argv := append([]string{"--inhibit-cache", "--library-path", lib, r.path(launcherName)}, args...)
	return exec.CommandContext(ctx, filepath.Join(lib, loaders[runtime.GOARCH]), argv...) //nolint:gosec // G204: the runtime's own loader and launcher
}

// completeRuntime names the first file dir lacks, if any: a runtime file,
// or a library the launcher needs, directly or through another, that lib/
// does not hold. The loader would take one it lacks from the host's /lib,
// which the runtime exists to avoid (ADR 26-11-03-530 §4).
func completeRuntime(dir string) error {
	for _, file := range runtimeFiles() {
		if info, err := os.Stat(filepath.Join(dir, filepath.FromSlash(file))); err != nil || !info.Mode().IsRegular() {
			return fmt.Errorf("%s has no %s", dir, file)
		}
	}
	return neededLibraries(dir)
}

// neededLibraries checks that lib/ holds every shared library the launcher
// needs, following each one's own needs, by their ELF DT_NEEDED entries. A
// file that is not ELF needs nothing.
func neededLibraries(dir string) error {
	seen := map[string]bool{}
	queue := []string{filepath.Join(dir, filepath.FromSlash(launcherName))}
	for len(queue) > 0 {
		file := queue[0]
		queue = queue[1:]
		f, err := elf.Open(file)
		if err != nil {
			continue
		}
		needed, err := f.ImportedLibraries()
		_ = f.Close()
		if err != nil {
			return fmt.Errorf("%s: %w", file, err)
		}
		for _, lib := range needed {
			if seen[lib] {
				continue
			}
			seen[lib] = true
			path := filepath.Join(dir, libDir, lib)
			if info, err := os.Stat(path); err != nil || !info.Mode().IsRegular() {
				return fmt.Errorf("%s has no %s/%s, which %s needs", dir, libDir, lib, filepath.Base(file))
			}
			queue = append(queue, path)
		}
	}
	return nil
}

// readManifest reads a runtime's runtime.json and refuses one whose launcher
// reads another configuration version than this driver writes, naming both.
func readManifest(dir string) (manifest, error) {
	var m manifest
	data, err := os.ReadFile(filepath.Join(dir, manifestName))
	if err != nil {
		return m, fmt.Errorf("libkrun: runtime %s: %w", dir, err)
	}
	if err := json.Unmarshal(data, &m); err != nil {
		return m, fmt.Errorf("libkrun: runtime %s: %s: %w", dir, manifestName, err)
	}
	if m.Config != launchVersion {
		return m, fmt.Errorf("libkrun: runtime %s (%s) reads launch configuration version %q, and this disco-vm writes %q; use a runtime made for it", dir, m.Version, m.Config, launchVersion)
	}
	return m, nil
}

// checkRuntime runs the runtime's launcher --version through its own loader,
// which loads libkrun and every library it needs from the runtime and makes
// a libkrun context, and refuses a runtime that fails, with its output.
func checkRuntime(ctx context.Context, dir string) error {
	if err := completeRuntime(dir); err != nil {
		return fmt.Errorf("libkrun: runtime %w", err)
	}
	if _, err := readManifest(dir); err != nil {
		return err
	}
	ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	out, err := krunRuntime(dir).command(ctx, "--version").CombinedOutput()
	if err != nil {
		return fmt.Errorf("libkrun: the runtime in %s does not run (%w): %s", dir, err, strings.TrimSpace(string(out)))
	}
	if got := strings.TrimSpace(string(out)); got != "krun-launch "+launchVersion {
		return fmt.Errorf("libkrun: the runtime in %s answers %q, and this disco-vm writes launch configuration version %s", dir, got, launchVersion)
	}
	return nil
}

// cachedRuntime is the runtime the driver would run with, if it needs no
// pull: the configured one (Config.Runtime), or DefaultRuntime's in the cache.
func (h *Hypervisor) cachedRuntime() (string, bool, error) {
	if h.runtimeDir != "" {
		if !filepath.IsAbs(h.runtimeDir) {
			return "", false, fmt.Errorf("libkrun: runtime %q is not an absolute directory", h.runtimeDir)
		}
		return h.runtimeDir, true, nil
	}
	dir, ok := cachedRuntimeOf(h.cacheDir, DefaultRuntime)
	return dir, ok, nil
}

// runtime is the runtime a VM runs with: the configured one, checked
// complete, or DefaultRuntime in cacheDir. Prepare passes pull, which asks
// the registry for DefaultRuntime (a tag, until it is pinned by digest);
// Boot does not, and uses the cached one, pulling only if it was removed
// behind the driver's back, so a start needs no network. That pull is in
// the shim, which is never given machine.Config.Dial, so it alone dials
// the process's own way.
func (h *Hypervisor) runtime(ctx context.Context, cacheDir string, pull bool) (krunRuntime, error) {
	if h.runtimeDir != "" {
		if !filepath.IsAbs(h.runtimeDir) {
			return "", fmt.Errorf("libkrun: runtime %q is not an absolute directory", h.runtimeDir)
		}
		if err := completeRuntime(h.runtimeDir); err != nil {
			return "", fmt.Errorf("libkrun: runtime (%s) %w", h.runtimeDir, err)
		}
		if _, err := readManifest(h.runtimeDir); err != nil {
			return "", err
		}
		return krunRuntime(h.runtimeDir), nil
	}
	dir, ok := cachedRuntimeOf(cacheDir, DefaultRuntime)
	if pull || !ok {
		var err error
		if dir, err = pullRuntime(ctx, h.transport, cacheDir, DefaultRuntime); err != nil {
			return "", err
		}
	}
	if _, err := readManifest(dir); err != nil {
		return "", err
	}
	return krunRuntime(dir), nil
}

// runtimeSource is a cached runtime's source.json.
type runtimeSource struct {
	Ref    string `json:"ref"`
	Digest string `json:"digest"`
}

// cachedRuntimeOf is the directory of the runtime ref names in cacheDir, if
// it is there: by the digest a digest reference names, or the newest pull of
// a reference as it was given.
func cachedRuntimeOf(cacheDir, ref string) (string, bool) {
	digest := ""
	if d, err := name.NewDigest(ref); err == nil {
		digest = d.DigestStr()
	}
	root := filepath.Join(cacheDir, "runtime")
	entries, _ := os.ReadDir(root)
	type hit struct {
		dir     string
		created time.Time
	}
	var hits []hit
	for _, entry := range entries {
		if !entry.IsDir() || strings.HasPrefix(entry.Name(), ".") {
			continue
		}
		dir := filepath.Join(root, entry.Name())
		var src runtimeSource
		data, err := os.ReadFile(filepath.Join(dir, runtimeSourceName))
		if err != nil || json.Unmarshal(data, &src) != nil || completeRuntime(dir) != nil {
			continue
		}
		if src.Ref != ref && (digest == "" || src.Digest != digest) {
			continue
		}
		h := hit{dir: dir}
		if info, err := entry.Info(); err == nil {
			h.created = info.ModTime()
		}
		hits = append(hits, h)
	}
	if len(hits) == 0 {
		return "", false
	}
	slices.SortFunc(hits, func(a, b hit) int { return b.created.Compare(a.created) })
	return hits[0].dir, true
}

// pullRuntime pulls a runtime image into cacheDir/runtime/<digest> unless it
// is there already, and returns that directory. A digest reference the cache
// has is used with no network; a tag is asked of the registry, and its newest
// pull is used when the registry cannot be reached. An image for another
// platform, one lacking a runtime file, or one whose launcher does not run
// is refused, naming it, and never renamed into place. The registry is
// called through transport.
func pullRuntime(ctx context.Context, transport http.RoundTripper, cacheDir, ref string) (string, error) {
	r, err := name.ParseReference(ref)
	if err != nil {
		return "", fmt.Errorf("libkrun: runtime %q: %w", ref, err)
	}
	if _, isDigest := r.(name.Digest); isDigest {
		if dir, ok := cachedRuntimeOf(cacheDir, ref); ok {
			return dir, nil
		}
	}
	platform := v1.Platform{OS: "linux", Architecture: runtime.GOARCH}
	desc, err := remote.Get(r, remote.WithContext(ctx), remote.WithTransport(transport), remote.WithPlatform(platform), remote.WithAuthFromKeychain(authn.DefaultKeychain))
	if err != nil {
		if dir, ok := cachedRuntimeOf(cacheDir, ref); ok {
			return dir, nil
		}
		return "", fmt.Errorf("libkrun: runtime %s: %w", ref, err)
	}
	img, err := desc.Image()
	if err != nil {
		return "", fmt.Errorf("libkrun: runtime %s: %w", ref, err)
	}
	config, err := img.ConfigFile()
	if err != nil {
		return "", fmt.Errorf("libkrun: runtime %s: %w", ref, err)
	}
	if config.OS != "linux" || config.Architecture != runtime.GOARCH {
		return "", fmt.Errorf("libkrun: runtime %s is for %s/%s, and this host is linux/%s", ref, config.OS, config.Architecture, runtime.GOARCH)
	}
	digest, err := img.Digest()
	if err != nil {
		return "", err
	}
	root := filepath.Join(cacheDir, "runtime")
	dir := filepath.Join(root, digest.Hex)
	if completeRuntime(dir) == nil {
		return dir, nil
	}
	if err := os.MkdirAll(root, 0o755); err != nil {
		return "", err
	}
	tmp, err := os.MkdirTemp(root, ".pull-")
	if err != nil {
		return "", err
	}
	defer os.RemoveAll(tmp)
	if err := extractRuntime(mutate.Extract(img), tmp); err != nil {
		return "", fmt.Errorf("libkrun: runtime %s: %w", ref, err)
	}
	if err := completeRuntime(tmp); err != nil {
		return "", fmt.Errorf("libkrun: runtime %s is not a libkrun runtime: %w", ref, err)
	}
	if err := checkRuntime(ctx, tmp); err != nil {
		return "", fmt.Errorf("libkrun: runtime %s: %w", ref, err)
	}
	src, err := json.Marshal(runtimeSource{Ref: ref, Digest: digest.String()})
	if err != nil {
		return "", err
	}
	if err := os.WriteFile(filepath.Join(tmp, runtimeSourceName), src, 0o644); err != nil { //nolint:gosec // G306: a reference and a digest
		return "", err
	}
	if err := os.Rename(tmp, dir); err != nil {
		// Another pull of the same digest won.
		if completeRuntime(dir) == nil {
			return dir, nil
		}
		return "", err
	}
	return dir, nil
}

// extractRuntime writes the regular files of a flattened runtime image that
// are its (bin/, lib/, runtime.json) into dir, with their permission bits,
// and nothing else.
func extractRuntime(r io.ReadCloser, dir string) error {
	defer r.Close()
	tr := tar.NewReader(r)
	for {
		hdr, err := tr.Next()
		if errors.Is(err, io.EOF) {
			return nil
		}
		if err != nil {
			return err
		}
		file := path.Clean(strings.TrimPrefix(hdr.Name, "/"))
		ours := file == manifestName || strings.HasPrefix(file, "bin/") || strings.HasPrefix(file, libDir+"/")
		if hdr.Typeflag != tar.TypeReg || !ours || strings.Contains(file, "..") {
			continue
		}
		dst := filepath.Join(dir, filepath.FromSlash(file))
		if err := os.MkdirAll(filepath.Dir(dst), 0o755); err != nil {
			return err
		}
		out, err := os.OpenFile(dst, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, os.FileMode(hdr.Mode).Perm()&0o755) //nolint:gosec // G115: a tar header's mode bits
		if err != nil {
			return err
		}
		_, err = io.Copy(out, tr) //nolint:gosec // G110: the registry's digest bounds the runtime
		if err := errors.Join(err, out.Close()); err != nil {
			return err
		}
	}
}

// tail is at most the last n bytes of b, from a line start where there is
// one.
func tail(b []byte, n int) string {
	if len(b) > n {
		b = b[len(b)-n:]
		if i := bytes.IndexByte(b, '\n'); i >= 0 {
			b = b[i+1:]
		}
	}
	return strings.TrimSpace(string(b))
}
