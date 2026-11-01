package vz

import (
	"archive/tar"
	"bytes"
	"context"
	"debug/elf"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path"
	"path/filepath"
	"slices"
	"strings"
	"time"

	"github.com/google/go-containerregistry/pkg/authn"
	"github.com/google/go-containerregistry/pkg/name"
	v1 "github.com/google/go-containerregistry/pkg/v1"
	"github.com/google/go-containerregistry/pkg/v1/mutate"
	"github.com/google/go-containerregistry/pkg/v1/remote"

	"github.com/discobox-ai/vm/pkg/machine"
)

// What a Linux guest is made of (ADR 26-10-31-990). These are pure Go, so
// they build and are tested on every OS; only booting one needs the
// framework.
const (
	// kernelName is the guest's uncompressed kernel, which
	// VZLinuxBootLoader loads directly.
	kernelName = "vmlinux"
	// initrdName is the image's initrd, which brings up the virtio drivers
	// its root needs.
	initrdName = "initrd.img"
	// rootName is the image's root filesystem, a raw ext4 image, in the
	// published image. A bundle keeps it as disk.img.
	rootName = "root.ext4"
	// linuxSourceName records, in a cached guest image, what it was pulled
	// as.
	linuxSourceName = "source.json"

	// agentDir and agentPath are where the appended archive puts the agent
	// in the initramfs. The base's initramfs hook moves it to
	// /run/disco-vm/disco-vm, which its disco-vm-guest.service runs.
	agentDir  = ".disco-vm"
	agentPath = agentDir + "/disco-vm"
)

// DefaultLinuxBase is the Linux guest image a reference of LinuxBaseName
// runs: disco-vm's own (pkg/guest/linux), from its linux/v* release line.
//
// It is a tag until the first release is published and its digest pinned
// here; ADR 26-10-31-990 pins it by digest.
const DefaultLinuxBase = "ghcr.io/discobox-ai/disco-vm-linux:v1"

// LinuxBaseName is the base that runs DefaultLinuxBase.
const LinuxBaseName = "linux"

// AgentEnv names the disco-vm built for linux on this host's architecture,
// which every boot of a Linux guest hands the guest. A darwin disco-vm
// carries no linux one: `go tool task build` builds it beside the darwin
// one, and a release ships both.
const AgentEnv = "DISCO_VM_AGENT"

// linuxArtifacts are the files a Linux guest image must carry.
var linuxArtifacts = []string{kernelName, initrdName, rootName}

// linuxBase is the base ref names: LinuxBaseName, an absolute directory
// holding a guest image's artifacts (used as it is: a guest built from the
// checkout), or a registry reference to a guest image, which Prepare pulls.
func linuxBase(ref string) (machine.Base, bool) {
	switch {
	case ref == LinuxBaseName:
		return machine.Base{Name: LinuxBaseName, ID: DefaultLinuxBase, GuestOS: machine.Linux, Options: map[string]string{"image": DefaultLinuxBase}}, true
	case filepath.IsAbs(ref):
		if completeArtifacts(ref) != nil {
			return machine.Base{}, false
		}
		return machine.Base{Name: ref, ID: ref, GuestOS: machine.Linux, Options: map[string]string{"dir": ref}}, true
	}
	if _, err := name.ParseReference(ref, name.StrictValidation); err != nil {
		return machine.Base{}, false
	}
	return machine.Base{Name: ref, ID: ref, GuestOS: machine.Linux, Options: map[string]string{"image": ref}}, true
}

// linuxSource is a cached guest image's source.json: the reference it was
// pulled as, and the digest of the image for this host's platform.
type linuxSource struct {
	Ref    string `json:"ref"`
	Digest string `json:"digest"`
}

// cachedImage is one guest image in the cache.
type cachedImage struct {
	dir     string
	src     linuxSource
	created time.Time
}

// pinned is the image's reference by its digest, which never moves.
func (c cachedImage) pinned() string {
	r, err := name.ParseReference(c.src.Ref)
	if err != nil {
		return c.src.Digest
	}
	return r.Context().Digest(c.src.Digest).String()
}

// cachedImages is every complete guest image cached under cacheDir, newest
// first.
func cachedImages(cacheDir string) []cachedImage {
	var out []cachedImage
	entries, _ := os.ReadDir(filepath.Join(cacheDir, "linux"))
	for _, entry := range entries {
		if !entry.IsDir() || strings.HasPrefix(entry.Name(), ".") {
			continue
		}
		dir := filepath.Join(cacheDir, "linux", entry.Name())
		var src linuxSource
		data, err := os.ReadFile(filepath.Join(dir, linuxSourceName))
		if err != nil || json.Unmarshal(data, &src) != nil || completeArtifacts(dir) != nil {
			continue
		}
		c := cachedImage{dir: dir, src: src}
		if info, err := entry.Info(); err == nil {
			c.created = info.ModTime().UTC()
		}
		out = append(out, c)
	}
	slices.SortFunc(out, func(a, b cachedImage) int { return b.created.Compare(a.created) })
	return out
}

// linuxBases is the default base and every guest image cached under
// cacheDir, each by its reference pinned to the digest it was pulled at, so
// running a listed one boots exactly that image, with no network.
func linuxBases(cacheDir string) []machine.Base {
	def, _ := linuxBase(LinuxBaseName)
	out := []machine.Base{def}
	for _, c := range cachedImages(cacheDir) {
		pinned := c.pinned()
		out = append(out, machine.Base{Name: pinned, ID: c.src.Digest, GuestOS: machine.Linux, Options: map[string]string{"image": pinned}, Created: c.created})
	}
	return out
}

// cached is the directory of the image ref names in the cache, if it is
// there: by the digest a digest reference names, or the newest pull of a
// reference as it was given.
func cached(cacheDir, ref string) (string, bool) {
	digest := ""
	if d, err := name.NewDigest(ref); err == nil {
		digest = d.DigestStr()
	}
	for _, c := range cachedImages(cacheDir) {
		if c.src.Ref == ref || (digest != "" && (c.src.Digest == digest || strings.HasSuffix(c.src.Ref, "@"+digest))) {
			return c.dir, true
		}
	}
	return "", false
}

// completeArtifacts names the first artifact dir lacks, if any.
func completeArtifacts(dir string) error {
	for _, artifact := range linuxArtifacts {
		if info, err := os.Stat(filepath.Join(dir, artifact)); err != nil || !info.Mode().IsRegular() {
			return fmt.Errorf("vz: %s has no %s", dir, artifact)
		}
	}
	return nil
}

// linuxArtifactDir is the directory holding b's artifacts: its own, or the
// cache's copy of its image for this host's architecture, pulled once per
// manifest digest.
func linuxArtifactDir(ctx context.Context, cacheDir string, b machine.Base, arch string) (string, error) {
	if dir := b.Options["dir"]; dir != "" {
		return dir, completeArtifacts(dir)
	}
	ref := b.Options["image"]
	if ref == "" {
		ref = b.ID
	}
	return pullLinuxImage(ctx, cacheDir, ref, arch)
}

// pullLinuxImage pulls a guest image's artifacts into cacheDir/linux/<digest>
// unless they are there already, and returns that directory. A digest
// reference the cache has is used with no network. A tag is asked of the
// registry, and the newest pull of it is used when the registry cannot be
// reached. An image whose platform is not linux/arch, or that lacks an
// artifact, is refused, naming the image: a bad release reported where it
// can be acted on, not as a VM that never answers.
func pullLinuxImage(ctx context.Context, cacheDir, ref, arch string) (string, error) {
	r, err := name.ParseReference(ref)
	if err != nil {
		return "", fmt.Errorf("vz: guest image %q: %w", ref, err)
	}
	if _, isDigest := r.(name.Digest); isDigest {
		if dir, ok := cached(cacheDir, ref); ok {
			return dir, nil
		}
	}
	platform := v1.Platform{OS: "linux", Architecture: arch}
	desc, err := remote.Get(r, remote.WithContext(ctx), remote.WithPlatform(platform), remote.WithAuthFromKeychain(authn.DefaultKeychain))
	if err != nil {
		if dir, ok := cached(cacheDir, ref); ok {
			return dir, nil
		}
		return "", fmt.Errorf("vz: guest image %s: %w", ref, err)
	}
	img, err := desc.Image()
	if err != nil {
		return "", fmt.Errorf("vz: guest image %s: %w", ref, err)
	}
	// A single-platform image is returned whatever platform was asked for.
	config, err := img.ConfigFile()
	if err != nil {
		return "", fmt.Errorf("vz: guest image %s: %w", ref, err)
	}
	if config.OS != "linux" || config.Architecture != arch {
		return "", fmt.Errorf("vz: guest image %s is for %s/%s, and this Mac boots linux/%s", ref, config.OS, config.Architecture, arch)
	}
	digest, err := img.Digest()
	if err != nil {
		return "", err
	}
	root := filepath.Join(cacheDir, "linux")
	dir := filepath.Join(root, digest.Hex)
	if completeArtifacts(dir) == nil {
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
	if err := extractArtifacts(mutate.Extract(img), tmp); err != nil {
		return "", fmt.Errorf("vz: guest image %s: %w", ref, err)
	}
	if err := completeArtifacts(tmp); err != nil {
		return "", fmt.Errorf("vz: guest image %s is not a Linux guest: it has no %s", ref, strings.TrimPrefix(err.Error(), "vz: "+tmp+" has no "))
	}
	src, err := json.Marshal(linuxSource{Ref: ref, Digest: digest.String()})
	if err != nil {
		return "", err
	}
	if err := os.WriteFile(filepath.Join(tmp, linuxSourceName), src, 0o644); err != nil { //nolint:gosec // G306: a reference and a digest
		return "", err
	}
	if err := os.Rename(tmp, dir); err != nil {
		// Another pull of the same digest won.
		if completeArtifacts(dir) == nil {
			return dir, nil
		}
		return "", err
	}
	return dir, nil
}

// extractArtifacts writes the artifacts a flattened image's tar holds into
// dir, and nothing else.
func extractArtifacts(r io.ReadCloser, dir string) error {
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
		if hdr.Typeflag != tar.TypeReg || !isArtifact(file) {
			continue
		}
		out, err := os.OpenFile(filepath.Join(dir, file), os.O_WRONLY|os.O_CREATE|os.O_TRUNC, 0o644)
		if err != nil {
			return err
		}
		_, err = io.Copy(out, tr) //nolint:gosec // G110: a guest image's artifacts are as large as they are; the registry's digest bounds them
		if err := errors.Join(err, out.Close()); err != nil {
			return err
		}
	}
}

func isArtifact(file string) bool { return slices.Contains(linuxArtifacts, file) }

// linuxCmdline is a Linux guest's kernel command line. panic= makes a
// panicked kernel reset, which the framework sees as a stop, so a running VM
// has a running kernel. systemd.machine_id gives the instance its own
// identity, whatever the image's /etc/machine-id holds. disco-vm.nic=0 tells
// the base it has no network device, so it does not wait for one.
func linuxCmdline(machineID string, nic bool) string {
	cmdline := "console=hvc0 root=/dev/vda rw rootfstype=ext4 panic=5 systemd.machine_id=" + machineID
	if !nic {
		cmdline += " disco-vm.nic=0"
	}
	return cmdline
}

// checkAgent refuses an agent that is not a linux executable for the guest's
// architecture, before a VM is started that could never answer.
func checkAgent(agent, arch string) error {
	if agent == "" {
		return fmt.Errorf("vz: a Linux guest needs the disco-vm built for linux/%s as its agent: set %s to it (`go tool task build` builds it in build/linux-%s)", arch, AgentEnv, arch)
	}
	f, err := elf.Open(agent)
	if err != nil {
		return fmt.Errorf("vz: the agent %s (%s) is not a linux executable: %w", agent, AgentEnv, err)
	}
	defer f.Close()
	want := map[string]elf.Machine{"arm64": elf.EM_AARCH64, "amd64": elf.EM_X86_64}[arch]
	if f.Machine != want || (f.OSABI != elf.ELFOSABI_NONE && f.OSABI != elf.ELFOSABI_LINUX) || (f.Type != elf.ET_EXEC && f.Type != elf.ET_DYN) {
		return fmt.Errorf("vz: the agent %s (%s) is not a linux/%s executable (it is %s, %s)", agent, AgentEnv, arch, f.Machine, f.OSABI)
	}
	return nil
}

// writeBootInitrd writes the initrd a boot hands the kernel to dst: the
// image's initrd, padded to four bytes, then an uncompressed newc archive
// holding the agent at agentPath. The kernel unpacks each archive of a
// concatenation in turn into the initramfs.
func writeBootInitrd(dst, initrd, agent string) error {
	out, err := os.OpenFile(dst, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, 0o600)
	if err != nil {
		return err
	}
	err = func() error {
		in, err := os.Open(initrd)
		if err != nil {
			return err
		}
		defer in.Close()
		n, err := io.Copy(out, in)
		if err != nil {
			return err
		}
		if pad := (4 - n%4) % 4; pad > 0 {
			if _, err := out.Write(make([]byte, pad)); err != nil {
				return err
			}
		}
		data, err := os.ReadFile(agent)
		if err != nil {
			return err
		}
		return writeAgentArchive(out, data)
	}()
	return errors.Join(err, out.Close())
}

// writeAgentArchive writes a newc cpio archive holding agentDir and the
// agent, executable, at agentPath.
func writeAgentArchive(w io.Writer, agent []byte) error {
	var buf bytes.Buffer
	entries := []struct {
		name string
		mode uint32
		data []byte
	}{
		{agentDir, 0o040755, nil},
		{agentPath, 0o100755, agent},
		{"TRAILER!!!", 0, nil},
	}
	for i, e := range entries {
		nlink := 1
		if e.mode&0o040000 != 0 {
			nlink = 2
		}
		// newc: magic, then thirteen 8-digit hex fields: ino, mode, uid,
		// gid, nlink, mtime, filesize, devmajor, devminor, rdevmajor,
		// rdevminor, namesize (with its NUL), check.
		fmt.Fprintf(&buf, "070701%08X%08X%08X%08X%08X%08X%08X%08X%08X%08X%08X%08X%08X",
			i+1, e.mode, 0, 0, nlink, 0, len(e.data), 0, 0, 0, 0, len(e.name)+1, 0)
		buf.WriteString(e.name)
		buf.WriteByte(0)
		pad4(&buf)
		buf.Write(e.data)
		pad4(&buf)
	}
	_, err := w.Write(buf.Bytes())
	return err
}

func pad4(buf *bytes.Buffer) {
	for buf.Len()%4 != 0 {
		buf.WriteByte(0)
	}
}
