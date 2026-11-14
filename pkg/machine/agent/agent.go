// Package agent is disco-vm's published guest agent (discobox ADR
// 26-11-11-963 §10, ADR 26-11-13-318 §3) as a host holds it: the index an
// embedder pins by digest and pulls a platform's agent from (Pull), and the
// one check every hypervisor makes of an agent before it hands it a guest
// (Check). It is pure Go, so it builds and is tested on every OS.
package agent

import (
	"archive/tar"
	"context"
	"debug/elf"
	"debug/macho"
	"errors"
	"fmt"
	"io"
	"os"
	"path"
	"path/filepath"
	"strings"

	"github.com/google/go-containerregistry/pkg/authn"
	"github.com/google/go-containerregistry/pkg/name"
	v1 "github.com/google/go-containerregistry/pkg/v1"
	"github.com/google/go-containerregistry/pkg/v1/mutate"
	"github.com/google/go-containerregistry/pkg/v1/remote"

	"github.com/discobox-ai/vm/pkg/machine"
)

// Repository is where disco-vm publishes its agent: one index per module
// version, tagged with that version (v1.2.3, or a pseudo-version for a commit
// on main), holding linux/amd64 and linux/arm64, the static linux disco-vm a
// host hands a Linux guest at every boot, and darwin/arm64, the disco-vm vz
// attaches to a macOS guest on its agent's drive (vz's macosAgent). An
// embedder pins the index's digest beside the module version it links and
// pulls each platform's agent with Pull.
const Repository = "ghcr.io/discobox-ai/disco-vm-agent"

// File is the agent in an agent image, at its root.
const File = "disco-vm"

// Pull pulls the goos/arch agent of the agent image ref pins by digest into
// cacheDir/agent/<digest>-<goos>-<arch>/disco-vm, unless it is there already,
// and returns that path, absolute (a relative cacheDir is taken from the
// working directory). The cache is owner-only: the agent runs in every guest.
// A cached agent is used with no network, since a digest names one image for
// good. A reference that is not by digest, an image for another platform, and
// one whose disco-vm is not a goos/arch executable (Check) are refused, naming
// the image.
func Pull(ctx context.Context, cacheDir, ref, goos, arch string) (string, error) {
	d, err := name.NewDigest(ref)
	if err != nil {
		return "", fmt.Errorf("agent image %q is not pinned by digest: %w", ref, err)
	}
	hash, err := v1.NewHash(d.DigestStr())
	if err != nil {
		return "", fmt.Errorf("agent image %q: %w", ref, err)
	}
	if _, ok := executables[goos]; !ok {
		return "", fmt.Errorf("agent image %s: disco-vm publishes no agent for %s", ref, goos)
	}
	// Check takes an absolute path alone.
	if cacheDir, err = filepath.Abs(cacheDir); err != nil {
		return "", err
	}
	root := filepath.Join(cacheDir, "agent")
	dir := filepath.Join(root, hash.Algorithm+"-"+hash.Hex+"-"+goos+"-"+arch)
	agent := filepath.Join(dir, File)
	if Check(agent, goos, arch) == nil {
		return agent, nil
	}
	desc, err := remote.Get(d, remote.WithContext(ctx), remote.WithPlatform(v1.Platform{OS: goos, Architecture: arch}), remote.WithAuthFromKeychain(authn.DefaultKeychain))
	if err != nil {
		return "", fmt.Errorf("agent image %s: %w", ref, err)
	}
	img, err := desc.Image()
	if err != nil {
		return "", fmt.Errorf("agent image %s: %w", ref, err)
	}
	// A single-platform image is returned whatever platform was asked for.
	config, err := img.ConfigFile()
	if err != nil {
		return "", fmt.Errorf("agent image %s: %w", ref, err)
	}
	if config.OS != goos || config.Architecture != arch {
		return "", fmt.Errorf("agent image %s is for %s/%s, not %s/%s", ref, config.OS, config.Architecture, goos, arch)
	}
	if err := os.MkdirAll(root, 0o700); err != nil {
		return "", err
	}
	// MkdirAll leaves a directory that was there as it was.
	if err := os.Chmod(root, 0o700); err != nil {
		return "", err
	}
	tmp, err := os.MkdirTemp(root, ".pull-")
	if err != nil {
		return "", err
	}
	defer os.RemoveAll(tmp)
	found, err := extract(mutate.Extract(img), filepath.Join(tmp, File))
	if err != nil {
		return "", fmt.Errorf("agent image %s: %w", ref, err)
	}
	if !found {
		return "", fmt.Errorf("agent image %s holds no %s", ref, File)
	}
	if err := Check(filepath.Join(tmp, File), goos, arch); err != nil {
		return "", fmt.Errorf("agent image %s: %w", ref, err)
	}
	// A broken pull of the same digest is replaced; a good one, which a
	// concurrent pull may have just returned and a boot be reading, is not.
	if Check(agent, goos, arch) == nil {
		return agent, nil
	}
	_ = os.RemoveAll(dir)
	if err := os.Rename(tmp, dir); err != nil {
		// Another pull of the same digest won.
		if Check(agent, goos, arch) == nil {
			return agent, nil
		}
		return "", err
	}
	return agent, nil
}

// extract writes the agent a flattened image's tar holds at its root to dst,
// owner-only, and reports whether it held one.
func extract(r io.ReadCloser, dst string) (bool, error) {
	defer r.Close()
	tr := tar.NewReader(r)
	for {
		hdr, err := tr.Next()
		if errors.Is(err, io.EOF) {
			return false, nil
		}
		if err != nil {
			return false, err
		}
		if hdr.Typeflag != tar.TypeReg || path.Clean(strings.TrimPrefix(hdr.Name, "/")) != File {
			continue
		}
		out, err := os.OpenFile(dst, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, 0o700)
		if err != nil {
			return false, err
		}
		_, err = io.Copy(out, tr) //nolint:gosec // G110: the agent is as large as it is; the registry's digest bounds it
		return true, errors.Join(err, out.Close())
	}
}

// executables checks that a file is an executable for one OS and arch, or
// says what it is instead.
var executables = map[string]func(path, arch string) error{
	"linux":  checkELF,
	"darwin": checkMachO,
}

// Check refuses an agent that is not an executable for goos/arch: a linux
// ELF executable for a Linux guest, a Mach-O one for a macOS guest. A
// hypervisor checks before it starts a VM that could never answer, and Pull
// before it caches what it pulled.
func Check(agent, goos, arch string) error {
	check, ok := executables[goos]
	if !ok {
		return fmt.Errorf("the agent %s: no guest runs a %s agent", agent, goos)
	}
	// Nothing else is read from a path the configuration names (ADR
	// 26-11-11-215 §7), and a FIFO or a device would hang the read below.
	if err := machine.CheckFile(agent); err != nil {
		return fmt.Errorf("the agent: %w", err)
	}
	return check(agent, arch)
}

func checkELF(agent, arch string) error {
	f, err := elf.Open(agent)
	if err != nil {
		return fmt.Errorf("the agent %s is not a linux executable: %w", agent, err)
	}
	defer f.Close()
	want := map[string]elf.Machine{"arm64": elf.EM_AARCH64, "amd64": elf.EM_X86_64}[arch]
	if f.Machine != want || (f.OSABI != elf.ELFOSABI_NONE && f.OSABI != elf.ELFOSABI_LINUX) || (f.Type != elf.ET_EXEC && f.Type != elf.ET_DYN) {
		return fmt.Errorf("the agent %s is not a linux/%s executable (it is %s, %s)", agent, arch, f.Machine, f.OSABI)
	}
	return nil
}

func checkMachO(agent, arch string) error {
	f, err := macho.Open(agent)
	if err != nil {
		return fmt.Errorf("the agent %s is not a Mach-O executable: %w", agent, err)
	}
	defer f.Close()
	want := map[string]macho.Cpu{"arm64": macho.CpuArm64, "amd64": macho.CpuAmd64}[arch]
	if f.Type != macho.TypeExec || f.Cpu != want {
		return fmt.Errorf("the agent %s is not a darwin/%s executable (it is a Mach-O %s for %s)", agent, arch, f.Type, f.Cpu)
	}
	return nil
}
