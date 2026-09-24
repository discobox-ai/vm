//go:build darwin && cgo

package vz

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/url"
	"os"
	"os/exec"
	"path"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/Code-Hex/vz/v3"

	"github.com/discobox-ai/vm/internal/fsutil"
	"github.com/discobox-ai/vm/pkg/machine"
)

// Install creates a macOS base layer: it installs the restore image onto a new
// bundle, then boots it once with guest provisioning to create an account and
// bake in the agent, and shuts it down through the agent.
func (d *Driver) Install(ctx context.Context, spec machine.InstallSpec, dst machine.Layer) error {
	if spec.GuestOS != machine.Darwin {
		return fmt.Errorf("vz: can install darwin guests, not %s", spec.GuestOS)
	}
	if err := d.Check(ctx); err != nil {
		return err
	}
	opts, err := parseOptions(spec.Options)
	if err != nil {
		return err
	}
	if spec.Agent == "" {
		return errors.New("vz: install needs the agent binary")
	}
	if spec.DiskBytes <= 0 {
		return errors.New("vz: install needs a disk size")
	}
	log := spec.Log
	if log == nil {
		log = io.Discard
	}
	ipsw, err := restoreImage(ctx, spec.Media, spec.CacheDir, log)
	if err != nil {
		return err
	}
	b := bundle(dst.Dir)
	m, err := install(ctx, b, ipsw, spec, opts, log)
	if err != nil {
		return err
	}
	if err := bootstrap(ctx, b, m, spec.Agent, opts, spec.GUI, log); err != nil {
		return err
	}
	// A layer carries no running machine's identity.
	return os.Remove(b.path(macName))
}

// options are the install options a spec passes through (from.install.options).
type options struct {
	user, password, fullName string
	uid                      int
	autoLogin                bool
	asif                     bool
}

// defaultUID is where the provisioned account is moved, off the 501 that the
// host's first user has, so a warm stage can give 501 to that user.
const defaultUID = 600

func parseOptions(in map[string]string) (options, error) {
	o := options{user: "admin", uid: defaultUID, autoLogin: true}
	keys := make([]string, 0, len(in))
	for k := range in {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, k := range keys {
		v := in[k]
		switch k {
		case "username":
			o.user = v
		case "password":
			o.password = v
		case "fullname":
			o.fullName = v
		case "uid":
			n, err := strconv.Atoi(v)
			if err != nil || n < 501 {
				return o, fmt.Errorf("vz: option uid is a number from 501 up (below it macOS treats an account as a system one), not %q", v)
			}
			o.uid = n
		case "autologin":
			b, err := strconv.ParseBool(v)
			if err != nil {
				return o, fmt.Errorf("vz: option autologin: %w", err)
			}
			o.autoLogin = b
		case "disk-format":
			switch v {
			case "raw":
			case "asif":
				o.asif = true
			default:
				return o, fmt.Errorf("vz: option disk-format is raw or asif, not %q", v)
			}
		default:
			return o, fmt.Errorf("vz: unknown install option %q (have username, password, fullname, uid, autologin, disk-format)", k)
		}
	}
	if o.user == "" {
		return o, errors.New("vz: option username is empty")
	}
	if o.password == "" {
		// Nothing but the bootstrap needs it, and the layer records it.
		o.password = fsutil.RandomHex(12)
	}
	if o.fullName == "" {
		o.fullName = o.user
	}
	return o, nil
}

// restoreImage is the IPSW to install: media itself, or for "latest" the
// newest one this Mac supports, downloaded once into the cache under the name
// Apple gives it. It is about 20 GB, so a partial download is resumed, and it
// is renamed into place only when complete.
func restoreImage(ctx context.Context, media, cacheDir string, log io.Writer) (string, error) {
	if media != "latest" {
		if _, err := os.Stat(media); err != nil {
			return "", fmt.Errorf("vz: restore image: %w", err)
		}
		return media, nil
	}
	if cacheDir == "" {
		return "", errors.New("vz: media latest needs a cache directory to download into")
	}
	latest, err := vz.GetLatestSupportedMacOSRestoreImageURL()
	if err != nil {
		return "", fmt.Errorf("vz: find the latest restore image: %w", err)
	}
	u, err := url.Parse(latest)
	if err != nil {
		return "", fmt.Errorf("vz: restore image URL %q: %w", latest, err)
	}
	dst := filepath.Join(cacheDir, path.Base(u.Path))
	if _, err := os.Stat(dst); err == nil {
		fmt.Fprintf(log, "vz: restore image %s (cached)\n", filepath.Base(dst))
		return dst, nil
	}
	if err := os.MkdirAll(cacheDir, 0o755); err != nil {
		return "", err
	}
	partial := dst + ".download"
	fmt.Fprintf(log, "vz: downloading %s\n", latest)
	reader, err := vz.FetchLatestSupportedMacOSRestoreImage(ctx, partial)
	if err != nil {
		return "", fmt.Errorf("vz: download restore image: %w", err)
	}
	ticker := time.NewTicker(30 * time.Second)
	defer ticker.Stop()
	for done := false; !done; {
		select {
		case <-ctx.Done():
			return "", ctx.Err()
		case <-reader.Finished():
			if err := reader.Err(); err != nil {
				return "", fmt.Errorf("vz: download restore image: %w", err)
			}
			done = true
		case <-ticker.C:
			fmt.Fprintf(log, "vz: downloaded %.0f%% (%d MiB)\n", reader.FractionCompleted()*100, reader.Current()>>20)
		}
	}
	if err := os.Rename(partial, dst); err != nil {
		return "", err
	}
	return dst, nil
}

// install creates a bundle's files and runs the macOS installer on them.
//
// The hardware model is the restore image's, not a choice: it decides how the
// auxiliary storage is laid out and which macOS will boot.
func install(ctx context.Context, b bundle, ipsw string, spec machine.InstallSpec, opts options, log io.Writer) (meta, error) {
	image, err := vz.LoadMacOSRestoreImageFromPath(ipsw)
	if err != nil {
		return meta{}, entitlementHint(fmt.Errorf("vz: load restore image %s: %w", ipsw, err))
	}
	requirements := image.MostFeaturefulSupportedConfiguration()
	if requirements == nil {
		return meta{}, fmt.Errorf("vz: this Mac supports no configuration in %s", ipsw)
	}
	version := image.OperatingSystemVersion()
	if version.MajorVersion < 27 {
		// Before 27 the first boot is Setup Assistant, which needs a person at
		// a window, and nothing can put the agent on the disk before it.
		return meta{}, fmt.Errorf("vz: %s is macOS %s; an unattended install needs macOS 27 or newer, whose first boot can be provisioned", filepath.Base(ipsw), version)
	}
	hardware := requirements.HardwareModel()
	if !hardware.Supported() {
		return meta{}, fmt.Errorf("vz: this Mac does not support the hardware model in %s", ipsw)
	}
	if err := os.MkdirAll(string(b), 0o700); err != nil {
		return meta{}, err
	}
	if err := os.WriteFile(b.path(hardwareName), hardware.DataRepresentation(), 0o600); err != nil {
		return meta{}, err
	}
	if err := b.newIdentifier(); err != nil {
		return meta{}, err
	}
	if _, err := vz.NewMacAuxiliaryStorage(b.path(auxName), vz.WithCreatingMacAuxiliaryStorage(hardware)); err != nil {
		return meta{}, fmt.Errorf("vz: create auxiliary storage: %w", err)
	}
	if err := createDisk(ctx, b.path(diskName), spec.DiskBytes, opts.asif); err != nil {
		return meta{}, err
	}

	m := meta{
		MacOSVersion: fmt.Sprintf("%d.%d.%d", version.MajorVersion, version.MinorVersion, version.PatchVersion),
		BuildVersion: image.BuildVersion(),
		CPUs:         max(uint(spec.CPUs), uint(requirements.MinimumSupportedCPUCount())),
		Memory:       max(spec.Memory, requirements.MinimumSupportedMemorySize()),
		User:         opts.user,
		UID:          opts.uid,
		Password:     opts.password,
	}
	m.CPUs, m.Memory = size(m, 0, 0)
	if err := b.writeMeta(m); err != nil {
		return meta{}, err
	}
	vm, err := newVM(b, m.CPUs, m.Memory, nil)
	if err != nil {
		return meta{}, err
	}
	installer, err := vz.NewMacOSInstaller(vm, ipsw)
	if err != nil {
		return meta{}, fmt.Errorf("vz: create installer: %w", err)
	}
	if spec.GUI {
		// Closed when the installer is done, which lets go of the VM, so the
		// first boot can lock the bundle again.
		closeWindow, err := openWindow(vm, "disco-vm install: macOS "+m.MacOSVersion)
		if err != nil {
			return meta{}, err
		}
		defer closeWindow()
	}
	fmt.Fprintf(log, "vz: installing macOS %s (%s)\n", m.MacOSVersion, m.BuildVersion)
	reported, cancel := context.WithCancel(ctx)
	defer cancel()
	go func() {
		ticker := time.NewTicker(30 * time.Second)
		defer ticker.Stop()
		for {
			select {
			case <-reported.Done():
				return
			case <-installer.Done():
				return
			case <-ticker.C:
				fmt.Fprintf(log, "vz: installed %.0f%%\n", installer.FractionCompleted()*100)
			}
		}
	}()
	if err := installer.Install(ctx); err != nil {
		return meta{}, fmt.Errorf("vz: install macOS: %w", err)
	}
	return m, nil
}

// createDisk makes the disk the installer formats: raw by default, which needs
// nothing but the framework and is sparse on APFS, or ASIF, Apple's sparse
// image format for VM storage from macOS 26. Both clone. The disk cannot grow
// after install without resizing the guest's container from inside it.
func createDisk(ctx context.Context, path string, size int64, asif bool) error {
	if !asif {
		if err := vz.CreateDiskImage(path, size); err != nil {
			return fmt.Errorf("vz: create disk image: %w", err)
		}
		return nil
	}
	// --fs None leaves the image blank; the installer would only discard a
	// volume made here.
	cmd := exec.CommandContext(ctx, "diskutil", "image", "create", "blank",
		"--format", "ASIF", "--fs", "None", "--size", strconv.FormatInt(size, 10), path)
	if out, err := cmd.CombinedOutput(); err != nil {
		return fmt.Errorf("vz: create ASIF disk image: %w: %s", err, strings.TrimSpace(string(out)))
	}
	return nil
}
