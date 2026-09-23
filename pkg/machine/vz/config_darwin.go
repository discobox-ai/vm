//go:build darwin && cgo

package vz

import (
	"errors"
	"fmt"
	"net"
	"os"
	"strings"

	"github.com/Code-Hex/vz/v3"

	"github.com/discobox-ai/vm/pkg/machine"
)

const (
	displayWidth  = 1920
	displayHeight = 1200
	displayPPI    = 80
)

// platform reads a bundle's identity: hardware model, machine identifier, and
// NVRAM.
func (b bundle) platform() (*vz.MacPlatformConfiguration, error) {
	hardware, err := vz.NewMacHardwareModelWithDataPath(b.path(hardwareName))
	if err != nil {
		return nil, fmt.Errorf("vz: read hardware model: %w", err)
	}
	identifier, err := vz.NewMacMachineIdentifierWithDataPath(b.path(identifierName))
	if err != nil {
		return nil, fmt.Errorf("vz: read machine identifier: %w", err)
	}
	aux, err := vz.NewMacAuxiliaryStorage(b.path(auxName))
	if err != nil {
		return nil, fmt.Errorf("vz: open auxiliary storage: %w", err)
	}
	return vz.NewMacPlatformConfiguration(
		vz.WithMacHardwareModel(hardware),
		vz.WithMacMachineIdentifier(identifier),
		vz.WithMacAuxiliaryStorage(aux),
	)
}

// newIdentifier gives a bundle a machine identifier of its own.
func (b bundle) newIdentifier() error {
	identifier, err := vz.NewMacMachineIdentifier()
	if err != nil {
		return fmt.Errorf("vz: create machine identifier: %w", err)
	}
	return os.WriteFile(b.path(identifierName), identifier.DataRepresentation(), 0o600)
}

// macAddress is the bundle's network address, created on first use. It is
// part of the running machine's identity, like the identifier: stable across
// boots so the guest keeps its lease, and carried by a saved state, whose
// memory holds a network stack configured for it.
func (b bundle) macAddress() (*vz.MACAddress, error) {
	data, err := os.ReadFile(b.path(macName))
	if err == nil {
		hardware, err := net.ParseMAC(strings.TrimSpace(string(data)))
		if err != nil {
			return nil, fmt.Errorf("vz: parse %s: %w", b.path(macName), err)
		}
		return vz.NewMACAddress(hardware)
	}
	if !errors.Is(err, os.ErrNotExist) {
		return nil, fmt.Errorf("vz: %w", err)
	}
	address, err := vz.NewRandomLocallyAdministeredMACAddress()
	if err != nil {
		return nil, fmt.Errorf("vz: network address: %w", err)
	}
	if err := os.WriteFile(b.path(macName), []byte(address.String()+"\n"), 0o600); err != nil {
		return nil, fmt.Errorf("vz: %w", err)
	}
	return address, nil
}

// configuration is a bundle's machine. It is the same set of devices at
// install, at every boot, and at every save and restore, so the guest never
// sees its hardware change and a saved state always matches; only shares are
// optional, and a boot with shares cannot restore a state saved without them.
func (b bundle) configuration(cpus uint, memory uint64, shares []machine.Share) (*vz.VirtualMachineConfiguration, error) {
	platform, err := b.platform()
	if err != nil {
		return nil, err
	}
	address, err := b.macAddress()
	if err != nil {
		return nil, err
	}
	return configure(platform, cpus, memory, b.path(diskName), address, shares)
}

func configure(platform vz.PlatformConfiguration, cpus uint, memory uint64, disk string, address *vz.MACAddress, shares []machine.Share) (*vz.VirtualMachineConfiguration, error) {
	// No kernel, no initrd: a Mac's boot loader is the platform's own, and
	// everything it needs is in the auxiliary storage.
	loader, err := vz.NewMacOSBootLoader()
	if err != nil {
		return nil, fmt.Errorf("vz: boot loader: %w", err)
	}
	config, err := vz.NewVirtualMachineConfiguration(loader, cpus, memory)
	if err != nil {
		return nil, entitlementHint(fmt.Errorf("vz: machine configuration: %w", err))
	}
	config.SetPlatformVirtualMachineConfiguration(platform)

	attachment, err := vz.NewDiskImageStorageDeviceAttachment(disk, false)
	if err != nil {
		return nil, fmt.Errorf("vz: attach disk %s: %w", disk, err)
	}
	block, err := vz.NewVirtioBlockDeviceConfiguration(attachment)
	if err != nil {
		return nil, fmt.Errorf("vz: disk device: %w", err)
	}
	config.SetStorageDevicesVirtualMachineConfiguration([]vz.StorageDeviceConfiguration{block})

	nat, err := vz.NewNATNetworkDeviceAttachment()
	if err != nil {
		return nil, fmt.Errorf("vz: network attachment: %w", err)
	}
	network, err := vz.NewVirtioNetworkDeviceConfiguration(nat)
	if err != nil {
		return nil, fmt.Errorf("vz: network device: %w", err)
	}
	network.SetMACAddress(address)
	config.SetNetworkDevicesVirtualMachineConfiguration([]*vz.VirtioNetworkDeviceConfiguration{network})

	// The Mac framebuffer is attached whether or not anyone looks at it, so
	// that opening a window never changes the hardware the guest sees.
	graphics, err := vz.NewMacGraphicsDeviceConfiguration()
	if err != nil {
		return nil, fmt.Errorf("vz: graphics device: %w", err)
	}
	display, err := vz.NewMacGraphicsDisplayConfiguration(displayWidth, displayHeight, displayPPI)
	if err != nil {
		return nil, fmt.Errorf("vz: display: %w", err)
	}
	graphics.SetDisplays(display)
	config.SetGraphicsDevicesVirtualMachineConfiguration([]vz.GraphicsDeviceConfiguration{graphics})

	keyboard, err := vz.NewMacKeyboardConfiguration()
	if err != nil {
		return nil, fmt.Errorf("vz: keyboard: %w", err)
	}
	config.SetKeyboardsVirtualMachineConfiguration([]vz.KeyboardConfiguration{keyboard})
	pointer, err := vz.NewUSBScreenCoordinatePointingDeviceConfiguration()
	if err != nil {
		return nil, fmt.Errorf("vz: pointing device: %w", err)
	}
	pointing := []vz.PointingDeviceConfiguration{pointer}
	if trackpad, err := vz.NewMacTrackpadConfiguration(); err == nil {
		pointing = append(pointing, trackpad)
	}
	config.SetPointingDevicesVirtualMachineConfiguration(pointing)

	// The agent's transport. A macOS 13 or newer guest has AF_VSOCK.
	socket, err := vz.NewVirtioSocketDeviceConfiguration()
	if err != nil {
		return nil, fmt.Errorf("vz: socket device: %w", err)
	}
	config.SetSocketDevicesVirtualMachineConfiguration([]vz.SocketDeviceConfiguration{socket})

	// No memory balloon. A macOS guest matches the device but ignores a host
	// target, so it would return nothing, and any device added or removed
	// later strands every saved state taken before.

	if len(shares) > 0 {
		devices, err := shareDevices(shares)
		if err != nil {
			return nil, err
		}
		config.SetDirectorySharingDevicesVirtualMachineConfiguration(devices)
	}

	if ok, err := config.Validate(); err != nil || !ok {
		if err == nil {
			err = errors.New("configuration rejected")
		}
		return nil, fmt.Errorf("vz: validate machine configuration: %w", err)
	}
	return config, nil
}

// shareDevices exports host directories over virtiofs. A share with no tag
// gets the automount tag, which a macOS guest mounts by itself at
// /Volumes/My Shared Files; any other tag waits for a mount_virtiofs.
func shareDevices(shares []machine.Share) ([]vz.DirectorySharingDeviceConfiguration, error) {
	devices := make([]vz.DirectorySharingDeviceConfiguration, 0, len(shares))
	for _, share := range shares {
		tag := share.Tag
		if tag == "" {
			automount, err := vz.MacOSGuestAutomountTag()
			if err != nil {
				return nil, fmt.Errorf("vz: automount tag: %w", err)
			}
			tag = automount
		}
		directory, err := vz.NewSharedDirectory(share.HostPath, share.ReadOnly)
		if err != nil {
			return nil, fmt.Errorf("vz: share %s: %w", share.HostPath, err)
		}
		single, err := vz.NewSingleDirectoryShare(directory)
		if err != nil {
			return nil, fmt.Errorf("vz: share %s: %w", share.HostPath, err)
		}
		device, err := vz.NewVirtioFileSystemDeviceConfiguration(tag)
		if err != nil {
			return nil, fmt.Errorf("vz: share %s as %q: %w", share.HostPath, tag, err)
		}
		device.SetDirectoryShare(single)
		devices = append(devices, device)
	}
	return devices, nil
}

// size is a boot's CPUs and memory: what was asked, else what the guest was
// installed at, bounded by what the framework accepts on this host. It asks
// the framework rather than assuming, because a configuration outside the
// range is rejected without saying which field was wrong.
func size(m meta, cpus int, memory uint64) (uint, uint64) {
	c, mem := m.CPUs, m.Memory
	if cpus > 0 {
		c = uint(cpus)
	}
	if memory > 0 {
		mem = memory
	}
	c = min(max(c, vz.VirtualMachineConfigurationMinimumAllowedCPUCount()), vz.VirtualMachineConfigurationMaximumAllowedCPUCount())
	mem = min(max(mem, vz.VirtualMachineConfigurationMinimumAllowedMemorySize()), vz.VirtualMachineConfigurationMaximumAllowedMemorySize())
	// The framework takes whole mebibytes.
	return c, mem &^ (1<<20 - 1)
}

// entitlementHint names the one cause of an opaque framework failure that is
// almost always responsible in development: an unsigned binary.
func entitlementHint(err error) error {
	if err == nil || entitled() {
		return err
	}
	return fmt.Errorf("%w (this binary lacks the %s entitlement; build it with `make build`, which signs it)", err, virtualizationEntitlement)
}
