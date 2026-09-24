package vz

import (
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"

	"golang.org/x/sys/unix"
)

const (
	diskName       = "disk.img"
	auxName        = "aux.img"
	hardwareName   = "hardwaremodel"
	identifierName = "machineidentifier"
	macName        = "macaddress"
	stateName      = "state.bin"
	metaName       = "vz.json"
)

// bundle is one macOS guest's files in one directory.
type bundle string

func (b bundle) path(name string) string { return filepath.Join(string(b), name) }

// layerFiles are what a layer is made of, and what a clone of it copies. The
// machine identifier is not among them: a clone gets its own.
var layerFiles = []string{diskName, auxName, hardwareName, metaName}

// installed reports whether every file a boot needs is present, naming the
// first one that is not. A partial bundle is what an interrupted install
// leaves, and booting one fails deep inside the framework with nothing to act
// on.
func (b bundle) installed() error {
	if _, err := os.Stat(b.path(identifierName)); err != nil {
		return fmt.Errorf("vz: %s is not a complete guest: %w", b, err)
	}
	return b.installedLayer()
}

// installedLayer is installed for a layer, which has no identity of its own.
func (b bundle) installedLayer() error {
	for _, name := range layerFiles {
		if _, err := os.Stat(b.path(name)); err != nil {
			return fmt.Errorf("vz: %s is not a complete layer: %w", b, err)
		}
	}
	return nil
}

// meta is what an install chose, carried by every clone of the layer.
type meta struct {
	MacOSVersion string `json:"macosVersion"`
	BuildVersion string `json:"buildVersion"`
	// CPUs and Memory are the size the guest was installed at, which is what a
	// boot that asks for no size gets. They are at least the restore image's
	// minimums.
	CPUs   uint   `json:"cpus"`
	Memory uint64 `json:"memory"`
	// User and Password are the account provisioning created, which the agent
	// was installed through. The password is here because nothing else knows
	// it; the file is private to the owner.
	User     string `json:"user"`
	UID      int    `json:"uid,omitempty"`
	Password string `json:"password,omitempty"`
	// Login is the account a warm stage created and logs in at boot; every
	// clone of the stage has it.
	Login *login `json:"login,omitempty"`
	// Template is the template a resume clone was restored from, whose
	// identity it still has; its next boot gives it its own.
	Template string `json:"template,omitempty"`
	// Saved is set on a staged clone: the size and host its state was saved
	// at. A restore needs the size exactly, and a host OS update can
	// invalidate a state, so a state from another host build is stale.
	Saved *saved `json:"saved,omitempty"`
}

type login struct {
	Name     string `json:"name"`
	UID      int    `json:"uid,omitempty"`
	Password string `json:"password"`
	// Version is how the account was set up (loginVersion); a base made an
	// older way is made again.
	Version int `json:"version,omitempty"`
}

type saved struct {
	CPUs      uint   `json:"cpus"`
	Memory    uint64 `json:"memory"`
	HostBuild string `json:"hostBuild"`
}

func (b bundle) readMeta() (meta, error) {
	var m meta
	data, err := os.ReadFile(b.path(metaName))
	if err != nil {
		return m, fmt.Errorf("vz: %w", err)
	}
	if err := json.Unmarshal(data, &m); err != nil {
		return m, fmt.Errorf("vz: parse %s: %w", b.path(metaName), err)
	}
	return m, nil
}

func (b bundle) writeMeta(m meta) error {
	data, err := json.MarshalIndent(m, "", "  ")
	if err != nil {
		return err
	}
	tmp := b.path(metaName + ".tmp")
	if err := os.WriteFile(tmp, append(data, '\n'), 0o600); err != nil {
		return fmt.Errorf("vz: write %s: %w", tmp, err)
	}
	return os.Rename(tmp, b.path(metaName))
}

// cloneLayer clones a layer's files into dst, which must not hold them yet.
func cloneLayer(src, dst bundle) error {
	if err := os.MkdirAll(string(dst), 0o700); err != nil {
		return err
	}
	for _, name := range layerFiles {
		if err := clonefile(src.path(name), dst.path(name)); err != nil {
			return err
		}
	}
	return nil
}

// moveLayer moves a stopped guest's layer files into dst. It is how Commit
// consumes an instance: a rename within the volume, so nothing is copied.
func moveLayer(src, dst bundle) error {
	if err := os.MkdirAll(string(dst), 0o700); err != nil {
		return err
	}
	for _, name := range layerFiles {
		if err := os.Rename(src.path(name), dst.path(name)); err != nil {
			return fmt.Errorf("vz: move %s into the layer: %w", name, err)
		}
	}
	return nil
}

// clonefile is APFS's copy-on-write copy. The driver's state lives on one
// volume, so a clone that cannot share blocks is an error that names why,
// never a silent 100 GiB read.
func clonefile(from, to string) error {
	if err := unix.Clonefile(from, to, 0); err != nil {
		switch {
		case errors.Is(err, fs.ErrNotExist):
			return fmt.Errorf("vz: clone %s: it does not exist", from)
		case errors.Is(err, unix.EXDEV):
			return fmt.Errorf("vz: %s and %s are on different volumes, so they cannot share blocks", from, to)
		case errors.Is(err, unix.ENOTSUP):
			return fmt.Errorf("vz: %s is not on a filesystem that clones (APFS)", from)
		}
		return fmt.Errorf("vz: clone %s to %s: %w", from, to, err)
	}
	return nil
}

// hostBuild is the host's OS build, which a saved state is only good for.
func hostBuild() string {
	build, err := unix.Sysctl("kern.osversion")
	if err != nil {
		return ""
	}
	return build
}
