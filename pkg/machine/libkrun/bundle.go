//go:build linux

package libkrun

import (
	"context"
	"crypto/rand"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"path/filepath"
	"runtime"
	"slices"
	"strings"

	"golang.org/x/sys/unix"

	"github.com/discobox-ai/vm/pkg/machine/linuximage"
	"github.com/discobox-ai/vm/pkg/machine/local"
)

// A bundle's files.
const (
	kernelName = "vmlinux"
	initrdName = "initrd.img"
	diskName   = "disk.img"
	metaName   = "krun.json"
	// machineIDName and macName are a VM's own identity, given at Create:
	// its systemd machine ID and its network address.
	machineIDName = "machineid"
	macName       = "macaddress"
	// launchName is the launcher's configuration, written at every boot.
	launchName = "launch.conf"
	// bootInitrdName is the initrd a boot hands the kernel: the image's,
	// with the agent appended.
	bootInitrdName = "boot-initrd.img"
	// consoleName is the guest's console, the one record a guest that never
	// answered leaves; prevConsoleName is the boot before's.
	consoleName     = "console.log"
	prevConsoleName = "console.prev.log"
)

const (
	// defaultCPUs and defaultMemory are a guest's size when its instance
	// asks for none.
	defaultCPUs   = 2
	defaultMemory = 2 << 30
	// diskCeiling is a fresh VM's disk ceiling: sparse, so it costs what
	// the guest writes.
	diskCeiling = 64 << 30
	// metaVersion is krun.json's.
	metaVersion = 1
)

// layerFiles are what a committed VM keeps, and what a copy of it copies.
var layerFiles = []string{kernelName, initrdName, diskName, metaName}

// bundle is one VM's files in one directory.
type bundle string

func (b bundle) path(name string) string { return filepath.Join(string(b), name) }

// meta is a bundle's krun.json, carried by every copy.
type meta struct {
	Version int    `json:"version"`
	CPUs    int    `json:"cpus"`
	Memory  uint64 `json:"memory"`
}

func (b bundle) readMeta() (meta, error) {
	var m meta
	data, err := os.ReadFile(b.path(metaName))
	if err != nil {
		return m, fmt.Errorf("libkrun: %w", err)
	}
	if err := json.Unmarshal(data, &m); err != nil {
		return m, fmt.Errorf("libkrun: parse %s: %w", b.path(metaName), err)
	}
	if m.Version != metaVersion {
		return m, fmt.Errorf("libkrun: %s is version %d, and this disco-vm reads %d", b.path(metaName), m.Version, metaVersion)
	}
	return m, nil
}

func (b bundle) writeMeta(m meta) error {
	m.Version = metaVersion
	data, err := json.MarshalIndent(m, "", "  ")
	if err != nil {
		return err
	}
	tmp := b.path(metaName + ".tmp")
	if err := os.WriteFile(tmp, append(data, '\n'), 0o600); err != nil {
		return fmt.Errorf("libkrun: write %s: %w", tmp, err)
	}
	return os.Rename(tmp, b.path(metaName))
}

// layer reports whether every file a committed VM keeps is there, naming the
// first that is not.
func (b bundle) layer() error {
	for _, name := range layerFiles {
		if _, err := os.Stat(b.path(name)); err != nil {
			return fmt.Errorf("libkrun: %s is not a complete VM: %w", b, err)
		}
	}
	return nil
}

// prepared reports whether every file a boot needs is there.
func (b bundle) prepared() error {
	if err := b.layer(); err != nil {
		return err
	}
	for _, name := range []string{machineIDName, macName} {
		if _, err := os.Stat(b.path(name)); err != nil {
			return fmt.Errorf("libkrun: %s is not a complete VM: %w", b, err)
		}
	}
	return nil
}

// freeze makes a stopped VM's bundle a committed one in place: every layer
// file must be there, and everything else is removed.
func (b bundle) freeze() error {
	if err := b.layer(); err != nil {
		return err
	}
	entries, err := os.ReadDir(string(b))
	if err != nil {
		return err
	}
	for _, entry := range entries {
		if slices.Contains(layerFiles, entry.Name()) {
			continue
		}
		if err := os.RemoveAll(b.path(entry.Name())); err != nil {
			return fmt.Errorf("libkrun: commit: %w", err)
		}
	}
	return nil
}

// newIdentity gives a bundle a machine ID and a MAC address of its own, so a
// copy of a committed VM is never its source's twin, on the guest's systemd
// or on passt's leases.
func (b bundle) newIdentity() error {
	id, err := linuximage.NewMachineID()
	if err != nil {
		return err
	}
	if err := os.WriteFile(b.path(machineIDName), []byte(id+"\n"), 0o600); err != nil {
		return err
	}
	var mac [6]byte
	if _, err := rand.Read(mac[:]); err != nil {
		return err
	}
	// Locally administered, unicast.
	mac[0] = mac[0]&^0x01 | 0x02
	return os.WriteFile(b.path(macName), []byte(net.HardwareAddr(mac[:]).String()+"\n"), 0o600)
}

// identity is the bundle's machine ID and MAC address.
func (b bundle) identity() (string, net.HardwareAddr, error) {
	id, err := os.ReadFile(b.path(machineIDName))
	if err != nil {
		return "", nil, fmt.Errorf("libkrun: %w", err)
	}
	data, err := os.ReadFile(b.path(macName))
	if err != nil {
		return "", nil, fmt.Errorf("libkrun: %w", err)
	}
	mac, err := net.ParseMAC(strings.TrimSpace(string(data)))
	if err != nil {
		return "", nil, fmt.Errorf("libkrun: parse %s: %w", b.path(macName), err)
	}
	return strings.TrimSpace(string(id)), mac, nil
}

// Prepare makes the instance's bundle: a copy of the committed VM it is
// created from, or, fresh, the base's libkrun kernel, initrd, and root, the
// root grown sparse to the disk ceiling, which the guest's filesystem grows
// into at boot; and either way an identity of its own. It pulls the runtime
// too, if the cache lacks it, so a runtime that will not pull or run fails
// the Create, not the first start.
func (h *Hypervisor) Prepare(ctx context.Context, inst local.InstanceSpec) error {
	if err := checkForwards(inst.Forwards); err != nil {
		return err
	}
	if err := checkHost(); err != nil {
		return err
	}
	if _, err := h.runtime(ctx, inst.CacheDir, true); err != nil {
		return err
	}
	dst := bundle(inst.Dir)
	if err := os.MkdirAll(string(dst), 0o700); err != nil {
		return err
	}
	if len(inst.Chain) > 0 {
		src := bundle(inst.Parent().Dir)
		if err := src.layer(); err != nil {
			return err
		}
		for _, name := range layerFiles {
			if err := copyFile(src.path(name), dst.path(name)); err != nil {
				return err
			}
		}
		return dst.newIdentity()
	}
	base, err := parseBase(h.base)
	if err != nil {
		return err
	}
	from, err := base.Artifacts(ctx, inst.CacheDir, runtime.GOARCH)
	if err != nil {
		return fmt.Errorf("libkrun: %w", err)
	}
	for artifact, name := range map[string]string{linuximage.KrunKernel: kernelName, linuximage.Initrd: initrdName, linuximage.Root: diskName} {
		if err := copyFile(filepath.Join(from, filepath.FromSlash(artifact)), dst.path(name)); err != nil {
			return err
		}
	}
	if info, err := os.Stat(dst.path(diskName)); err != nil {
		return err
	} else if info.Size() < diskCeiling {
		if err := os.Truncate(dst.path(diskName), diskCeiling); err != nil {
			return fmt.Errorf("libkrun: grow the disk to %d bytes: %w", int64(diskCeiling), err)
		}
	}
	if err := dst.writeMeta(meta{CPUs: defaultCPUs, Memory: defaultMemory}); err != nil {
		return err
	}
	return dst.newIdentity()
}

// copyFile copies from to to, which must not exist: a reflink where the
// filesystem shares blocks (btrfs, XFS), and otherwise a sparse copy of the
// blocks in use, so a 64 GiB disk costs what its guest wrote.
func copyFile(from, to string) error {
	src, err := os.Open(from)
	if err != nil {
		return fmt.Errorf("libkrun: copy %s: %w", from, err)
	}
	defer src.Close()
	info, err := src.Stat()
	if err != nil {
		return err
	}
	dst, err := os.OpenFile(to, os.O_WRONLY|os.O_CREATE|os.O_EXCL, info.Mode().Perm()|0o600)
	if err != nil {
		return fmt.Errorf("libkrun: copy %s: %w", from, err)
	}
	err = func() error {
		if unix.IoctlFileClone(int(dst.Fd()), int(src.Fd())) == nil {
			return nil
		}
		return sparseCopy(src, dst, info.Size())
	}()
	if err := errors.Join(err, dst.Close()); err != nil {
		_ = os.Remove(to)
		return fmt.Errorf("libkrun: copy %s to %s: %w", from, to, err)
	}
	return nil
}

// sparseCopy copies the data extents of src, size bytes long, into dst, and
// leaves its holes holes.
func sparseCopy(src, dst *os.File, size int64) error {
	var off int64
	for off < size {
		data, err := unix.Seek(int(src.Fd()), off, unix.SEEK_DATA)
		if errors.Is(err, unix.ENXIO) {
			break // nothing but a hole to the end
		}
		if err != nil {
			// A filesystem with no SEEK_DATA: copy it all.
			data = off
		}
		hole, err := unix.Seek(int(src.Fd()), data, unix.SEEK_HOLE)
		if err != nil {
			hole = size
		}
		if _, err := dst.Seek(data, io.SeekStart); err != nil {
			return err
		}
		if _, err := io.Copy(dst, io.NewSectionReader(src, data, hole-data)); err != nil {
			return err
		}
		off = hole
	}
	return dst.Truncate(size)
}
