// Package linuximage is disco-vm's Linux guest image (pkg/guest/linux; ADR
// 26-10-31-990 §3) as a hypervisor below the seam boots it: the base a fresh
// VM starts from, pulled per manifest digest into the hypervisor's cache,
// what every boot hands the kernel, and the published agent an embedder pulls
// by digest (PullAgent). vz and libkrun share it (ADR
// 26-11-03-530 §6). It is pure Go, so it builds and is tested on every OS.
package linuximage

import (
	"archive/tar"
	"bytes"
	"context"
	"crypto/rand"
	"debug/elf"
	"encoding/hex"
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

// What a Linux guest image is made of. A hypervisor names the ones it boots
// (Parse); a pull extracts every one the image has.
const (
	// Kernel is Debian's uncompressed kernel, which vz loads directly.
	Kernel = "vmlinux"
	// KrunKernel is the kernel libkrun boots, an x86-64 ELF built from
	// libkrunfw's source and patches (ADR 26-11-03-530 §5). Only the amd64
	// variant carries it.
	KrunKernel = "krun/vmlinux"
	// Initrd is the image's initrd, which brings up the virtio drivers its
	// root needs and moves the agent a boot appends.
	Initrd = "initrd.img"
	// Root is the image's root filesystem, a raw ext4 image.
	Root = "root.ext4"
	// sourceName records, in a cached guest image, what it was pulled as.
	sourceName = "source.json"

	// AgentDir and AgentPath are where the appended archive puts the agent
	// in the initramfs. The base's initramfs hook moves it to
	// /run/disco-vm/disco-vm, which its disco-vm-guest.service runs.
	AgentDir  = ".disco-vm"
	AgentPath = AgentDir + "/disco-vm"

	// pageSize is what a boot's initrd is padded to.
	pageSize = 4096
)

// DefaultBase is the Linux guest image a fresh VM starts from when its
// driver is configured with no base (machine.Config.Base): disco-vm's own
// (pkg/guest/linux), from its linux/v* release line.
//
// It is a tag until the first release is published and its digest pinned
// here; ADR 26-10-31-990 pins it by digest.
const DefaultBase = "ghcr.io/discobox-ai/disco-vm-linux:v1"

// artifacts are every file a pull extracts.
var artifacts = []string{Kernel, KrunKernel, Initrd, Root}

// Image is a hypervisor's base: the Linux guest image a fresh VM starts from
// (ADR 26-11-03-606), and the artifacts that hypervisor boots of it. Exactly
// one of ref and dir is set, or neither, which is DefaultBase.
type Image struct {
	// ref is a registry reference to a guest image, which Prepare pulls.
	ref string
	// dir is an absolute directory holding a guest image's artifacts, used
	// as it is: a guest built from the checkout.
	dir  string
	need []string
}

// Parse is the base machine.Config.Base configures, for a hypervisor that
// boots the artifacts need: empty, for DefaultBase; an absolute directory
// holding them; or a registry reference to a guest image.
func Parse(base string, need ...string) (Image, error) {
	switch {
	case base == "":
		return Image{need: need}, nil
	case filepath.IsAbs(base):
		if err := complete(base, need); err != nil {
			return Image{}, err
		}
		return Image{dir: base, need: need}, nil
	}
	if _, err := name.ParseReference(base, name.StrictValidation); err != nil {
		return Image{}, fmt.Errorf("base %q is neither a Linux guest image's reference nor an absolute directory holding one: %w", base, err)
	}
	return Image{ref: base, need: need}, nil
}

// String names the image, for an error.
func (i Image) String() string {
	switch {
	case i.dir != "":
		return i.dir
	case i.ref != "":
		return i.ref
	}
	return DefaultBase
}

// Artifacts is the directory holding the image's artifacts: its own, or the
// cache's copy of its image for linux/arch, pulled into cacheDir once per
// manifest digest.
func (i Image) Artifacts(ctx context.Context, cacheDir, arch string) (string, error) {
	if i.dir != "" {
		return i.dir, complete(i.dir, i.need)
	}
	ref := i.ref
	if ref == "" {
		ref = DefaultBase
	}
	return Pull(ctx, cacheDir, ref, arch, i.need...)
}

// source is a cached guest image's source.json: the reference it was pulled
// as, and the digest of the image for this host's platform.
type source struct {
	Ref    string `json:"ref"`
	Digest string `json:"digest"`
}

// cachedImage is one guest image in the cache.
type cachedImage struct {
	dir     string
	src     source
	created time.Time
}

// cachedImages is every guest image cached under cacheDir that holds need,
// newest first.
func cachedImages(cacheDir string, need []string) []cachedImage {
	var out []cachedImage
	entries, _ := os.ReadDir(filepath.Join(cacheDir, "linux"))
	for _, entry := range entries {
		if !entry.IsDir() || strings.HasPrefix(entry.Name(), ".") {
			continue
		}
		dir := filepath.Join(cacheDir, "linux", entry.Name())
		var src source
		data, err := os.ReadFile(filepath.Join(dir, sourceName))
		if err != nil || json.Unmarshal(data, &src) != nil || complete(dir, need) != nil {
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

// cached is the directory of the image ref names in the cache, if it is
// there with need: by the digest a digest reference names, or the newest
// pull of a reference as it was given.
func cached(cacheDir, ref string, need []string) (string, bool) {
	digest := ""
	if d, err := name.NewDigest(ref); err == nil {
		digest = d.DigestStr()
	}
	for _, c := range cachedImages(cacheDir, need) {
		if c.src.Ref == ref || (digest != "" && (c.src.Digest == digest || strings.HasSuffix(c.src.Ref, "@"+digest))) {
			return c.dir, true
		}
	}
	return "", false
}

// complete names the first artifact of need that dir lacks, if any.
func complete(dir string, need []string) error {
	for _, artifact := range need {
		if info, err := os.Stat(filepath.Join(dir, filepath.FromSlash(artifact))); err != nil || !info.Mode().IsRegular() {
			return fmt.Errorf("%s has no %s", dir, artifact)
		}
	}
	return nil
}

// Pull pulls a guest image's artifacts into cacheDir/linux/<digest> unless
// they are there already, and returns that directory. A digest reference the
// cache has is used with no network. A tag is asked of the registry, and the
// newest pull of it is used when the registry cannot be reached. An image
// whose platform is not linux/arch, or that lacks an artifact of need, is
// refused, naming the image: a bad release reported where it can be acted
// on, not as a VM that never answers.
func Pull(ctx context.Context, cacheDir, ref, arch string, need ...string) (string, error) {
	r, err := name.ParseReference(ref)
	if err != nil {
		return "", fmt.Errorf("guest image %q: %w", ref, err)
	}
	if _, isDigest := r.(name.Digest); isDigest {
		if dir, ok := cached(cacheDir, ref, need); ok {
			return dir, nil
		}
	}
	platform := v1.Platform{OS: "linux", Architecture: arch}
	desc, err := remote.Get(r, remote.WithContext(ctx), remote.WithPlatform(platform), remote.WithAuthFromKeychain(authn.DefaultKeychain))
	if err != nil {
		if dir, ok := cached(cacheDir, ref, need); ok {
			return dir, nil
		}
		return "", fmt.Errorf("guest image %s: %w", ref, err)
	}
	img, err := platformImage(desc, "guest image", ref, arch)
	if err != nil {
		return "", err
	}
	digest, err := img.Digest()
	if err != nil {
		return "", err
	}
	root := filepath.Join(cacheDir, "linux")
	dir := filepath.Join(root, digest.Hex)
	if complete(dir, need) == nil {
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
	if err := extract(mutate.Extract(img), tmp, artifacts, 0o644); err != nil {
		return "", fmt.Errorf("guest image %s: %w", ref, err)
	}
	for _, artifact := range need {
		if complete(tmp, []string{artifact}) != nil {
			return "", fmt.Errorf("guest image %s is not a Linux guest this host boots: it has no %s", ref, artifact)
		}
	}
	src, err := json.Marshal(source{Ref: ref, Digest: digest.String()})
	if err != nil {
		return "", err
	}
	if err := os.WriteFile(filepath.Join(tmp, sourceName), src, 0o644); err != nil { //nolint:gosec // G306: a reference and a digest
		return "", err
	}
	// A pull of the same digest by another hypervisor's image, lacking an
	// artifact this one needs, is replaced.
	if complete(dir, need) != nil {
		_ = os.RemoveAll(dir)
	}
	if err := os.Rename(tmp, dir); err != nil {
		// Another pull of the same digest won.
		if complete(dir, need) == nil {
			return dir, nil
		}
		return "", err
	}
	return dir, nil
}

// platformImage is desc's image for linux/arch, refused, naming what it is,
// for another platform.
func platformImage(desc *remote.Descriptor, what, ref, arch string) (v1.Image, error) {
	img, err := desc.Image()
	if err != nil {
		return nil, fmt.Errorf("%s %s: %w", what, ref, err)
	}
	// A single-platform image is returned whatever platform was asked for.
	config, err := img.ConfigFile()
	if err != nil {
		return nil, fmt.Errorf("%s %s: %w", what, ref, err)
	}
	if config.OS != "linux" || config.Architecture != arch {
		return nil, fmt.Errorf("%s %s is for %s/%s, and this host boots linux/%s", what, ref, config.OS, config.Architecture, arch)
	}
	return img, nil
}

// extract writes the files of files a flattened image's tar holds into dir,
// each with mode, and nothing else.
func extract(r io.ReadCloser, dir string, files []string, mode os.FileMode) error {
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
		if hdr.Typeflag != tar.TypeReg || !slices.Contains(files, file) {
			continue
		}
		dst := filepath.Join(dir, filepath.FromSlash(file))
		if err := os.MkdirAll(filepath.Dir(dst), 0o755); err != nil {
			return err
		}
		out, err := os.OpenFile(dst, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, mode)
		if err != nil {
			return err
		}
		_, err = io.Copy(out, tr) //nolint:gosec // G110: a guest image's artifacts are as large as they are; the registry's digest bounds them
		if err := errors.Join(err, out.Close()); err != nil {
			return err
		}
	}
}

// Cmdline is a Linux guest's kernel command line. panic= makes a panicked
// kernel reset, which the hypervisor sees as a stop, so a running VM has a
// running kernel. systemd.machine_id gives the instance its own identity,
// whatever the image's /etc/machine-id holds. disco-vm.nic=0 tells the base
// it has no network device, so it does not wait for one.
func Cmdline(machineID string, nic bool) string {
	cmdline := "console=hvc0 root=/dev/vda rw rootfstype=ext4 panic=5 systemd.machine_id=" + machineID
	if !nic {
		cmdline += " disco-vm.nic=0"
	}
	return cmdline
}

// NewMachineID is a systemd machine ID: 32 lowercase hex digits.
func NewMachineID() (string, error) {
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "", err
	}
	return hex.EncodeToString(b[:]), nil
}

// CheckAgent refuses an agent that is not a linux executable for the guest's
// architecture, before a VM is started that could never answer.
func CheckAgent(agent, arch string) error {
	// Nothing else is read from a path the configuration names (ADR
	// 26-11-11-215 §7).
	if err := machine.CheckFile(agent); err != nil {
		return fmt.Errorf("the agent: %w", err)
	}
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

// WriteBootInitrd writes the initrd a boot hands the kernel to dst: the
// image's initrd, padded to four bytes, then an uncompressed newc archive
// holding the agent at AgentPath, and zeros to a whole page. The kernel
// unpacks each archive of a concatenation in turn into the initramfs, and
// skips zeros between them. The page is libkrun's: it places the initrd to
// end on a page, so one of a whole number of pages starts on one too, which
// the kernel's freeing of it after boot requires.
func WriteBootInitrd(dst, initrd, agent string) error {
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
		var archive bytes.Buffer
		if err := writeAgentArchive(&archive, data); err != nil {
			return err
		}
		n += (4-n%4)%4 + int64(archive.Len())
		archive.Write(make([]byte, (pageSize-n%pageSize)%pageSize))
		_, err = out.Write(archive.Bytes())
		return err
	}()
	return errors.Join(err, out.Close())
}

// writeAgentArchive writes a newc cpio archive holding AgentDir and the
// agent, executable, at AgentPath.
func writeAgentArchive(w io.Writer, agent []byte) error {
	var buf bytes.Buffer
	entries := []struct {
		name string
		mode uint32
		data []byte
	}{
		{AgentDir, 0o040755, nil},
		{AgentPath, 0o100755, agent},
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
