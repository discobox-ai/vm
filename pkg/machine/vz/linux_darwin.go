//go:build darwin && cgo

package vz

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"os"
	"runtime"
	"strconv"
	"strings"

	"github.com/Code-Hex/vz/v3"

	"github.com/discobox-ai/vm/pkg/machine"
	"github.com/discobox-ai/vm/pkg/machine/local"
)

const (
	// linuxCPUs and linuxMemory are a Linux guest's size when its instance
	// asks for none.
	linuxCPUs   = 2
	linuxMemory = 2 << 30
	// linuxDisk is a Linux instance's disk ceiling when its base asks for
	// none: sparse, so it costs what the guest writes.
	linuxDisk = 64 << 30
	// diskOption is the base option that sets a Linux instance's disk
	// ceiling, in bytes.
	diskOption = "disk"
)

// newMachineID is a systemd machine ID: 32 lowercase hex digits.
func newMachineID() (string, error) {
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "", err
	}
	return hex.EncodeToString(b[:]), nil
}

// prepareLinux makes a Linux instance from its base (ADR 26-10-31-990): the
// base's image pulled into the cache once per digest, or its directory; its
// kernel and initrd cloned; its root cloned as the instance's own disk and
// grown, sparse, to the disk ceiling, which the guest's filesystem grows into
// at boot; and an identity of its own.
func prepareLinux(ctx context.Context, inst local.InstanceSpec) error {
	if inst.Base.GuestOS != machine.Linux {
		return fmt.Errorf("vz: base %s is a %s guest, and vz's bases are Linux guests", inst.Base.Name, inst.Base.GuestOS)
	}
	ceiling := int64(linuxDisk)
	if raw := inst.Base.Options[diskOption]; raw != "" {
		n, err := strconv.ParseInt(raw, 10, 64)
		if err != nil || n <= 0 {
			return fmt.Errorf("vz: base %s: %s=%q is not a size in bytes", inst.Base.Name, diskOption, raw)
		}
		ceiling = n
	}
	src, err := linuxArtifactDir(ctx, inst.CacheDir, *inst.Base, runtime.GOARCH)
	if err != nil {
		return err
	}
	dst := bundle(inst.Dir)
	if err := os.MkdirAll(string(dst), 0o700); err != nil {
		return err
	}
	from := bundle(src)
	for name, to := range map[string]string{kernelName: kernelName, initrdName: initrdName, rootName: diskName} {
		if err := clonefile(from.path(name), dst.path(to)); err != nil {
			return err
		}
	}
	if info, err := os.Stat(dst.path(diskName)); err != nil {
		return err
	} else if info.Size() < ceiling {
		if err := os.Truncate(dst.path(diskName), ceiling); err != nil {
			return fmt.Errorf("vz: grow the disk to %d bytes: %w", ceiling, err)
		}
	}
	if err := dst.writeMeta(meta{GuestOS: machine.Linux, CPUs: linuxCPUs, Memory: linuxMemory}); err != nil {
		return err
	}
	return dst.newLinuxIdentity()
}

// newLinuxIdentity gives a Linux bundle a machine ID and a MAC address of
// its own, so a clone of a snapshot is never its source's twin, on the
// guest's systemd or on the NAT's leases.
func (b bundle) newLinuxIdentity() error {
	id, err := newMachineID()
	if err != nil {
		return err
	}
	if err := os.WriteFile(b.path(machineIDName), []byte(id+"\n"), 0o600); err != nil {
		return err
	}
	_ = os.Remove(b.path(macName))
	_, err = b.macAddress()
	return err
}

// bootLinux boots a Linux bundle from power-off: VZLinuxBootLoader with the
// image's kernel, the initrd with this host's linux agent appended, and the
// command line naming the instance's identity, and whether it has a network
// device.
func bootLinux(inst local.InstanceSpec, m meta, opts local.BootOptions) (*vmMachine, error) {
	if opts.GUI {
		return nil, fmt.Errorf("vz: a Linux guest has no display to show: %w", machine.ErrUnsupported)
	}
	console := opts.Console
	if console == nil {
		console = io.Discard
	}
	b := bundle(inst.Dir)
	if err := b.installed(machine.Linux); err != nil {
		return nil, err
	}
	agent := os.Getenv(AgentEnv)
	if err := checkAgent(agent, runtime.GOARCH); err != nil {
		return nil, err
	}
	if err := writeBootInitrd(b.path(bootInitrdName), b.path(initrdName), agent); err != nil {
		return nil, fmt.Errorf("vz: the boot's initrd: %w", err)
	}
	id, err := os.ReadFile(b.path(machineIDName))
	if err != nil {
		return nil, fmt.Errorf("vz: %w", err)
	}
	nic := !fenced(inst)
	cpus, memory := size(m, opts.CPUs, opts.Memory)
	config, err := b.linuxConfiguration(cpus, memory, linuxCmdline(strings.TrimSpace(string(id)), nic), nic, opts.Shares)
	if err != nil {
		return nil, err
	}
	vm, err := vz.NewVirtualMachine(config)
	if err != nil {
		return nil, entitlementHint(fmt.Errorf("vz: create virtual machine: %w", err))
	}
	if err := vm.Start(); err != nil {
		return nil, entitlementHint(fmt.Errorf("vz: start: %w", err))
	}
	network := "a NAT network device"
	if !nic {
		network = "no network device"
	}
	fmt.Fprintf(console, "vz: booted Linux with %d CPUs, %d MiB, and %s\n", cpus, memory>>20, network)
	return watch(vm), nil
}

// linuxConfiguration is a Linux bundle's machine: discobox's device set
// (ADR 0062): its disk, a socket device for the agent, the serial console to
// console.log, entropy, a balloon the guest gives memory back through, the
// NAT when nic, and the shares a boot asks for. There is no framebuffer.
func (b bundle) linuxConfiguration(cpus uint, memory uint64, cmdline string, nic bool, shares []machine.Share) (*vz.VirtualMachineConfiguration, error) {
	loader, err := vz.NewLinuxBootLoader(b.path(kernelName), vz.WithCommandLine(cmdline), vz.WithInitrd(b.path(bootInitrdName)))
	if err != nil {
		return nil, fmt.Errorf("vz: boot loader: %w", err)
	}
	config, err := vz.NewVirtualMachineConfiguration(loader, cpus, memory)
	if err != nil {
		return nil, entitlementHint(fmt.Errorf("vz: machine configuration: %w", err))
	}

	attachment, err := vz.NewDiskImageStorageDeviceAttachment(b.path(diskName), false)
	if err != nil {
		return nil, fmt.Errorf("vz: attach disk %s: %w", b.path(diskName), err)
	}
	block, err := vz.NewVirtioBlockDeviceConfiguration(attachment)
	if err != nil {
		return nil, fmt.Errorf("vz: disk device: %w", err)
	}
	config.SetStorageDevicesVirtualMachineConfiguration([]vz.StorageDeviceConfiguration{block})

	if nic {
		address, err := b.macAddress()
		if err != nil {
			return nil, err
		}
		network, err := natDevice(address)
		if err != nil {
			return nil, err
		}
		config.SetNetworkDevicesVirtualMachineConfiguration([]*vz.VirtioNetworkDeviceConfiguration{network})
	}

	socket, err := vz.NewVirtioSocketDeviceConfiguration()
	if err != nil {
		return nil, fmt.Errorf("vz: socket device: %w", err)
	}
	config.SetSocketDevicesVirtualMachineConfiguration([]vz.SocketDeviceConfiguration{socket})

	serial, err := vz.NewFileSerialPortAttachment(b.path(consoleName), true)
	if err != nil {
		return nil, fmt.Errorf("vz: serial console: %w", err)
	}
	console, err := vz.NewVirtioConsoleDeviceSerialPortConfiguration(serial)
	if err != nil {
		return nil, fmt.Errorf("vz: console device: %w", err)
	}
	config.SetSerialPortsVirtualMachineConfiguration([]*vz.VirtioConsoleDeviceSerialPortConfiguration{console})

	entropy, err := vz.NewVirtioEntropyDeviceConfiguration()
	if err != nil {
		return nil, fmt.Errorf("vz: entropy device: %w", err)
	}
	config.SetEntropyDevicesVirtualMachineConfiguration([]*vz.VirtioEntropyDeviceConfiguration{entropy})

	// A Linux guest honors its balloon, unlike a macOS one: memory it frees
	// goes back to the Mac.
	balloon, err := vz.NewVirtioTraditionalMemoryBalloonDeviceConfiguration()
	if err != nil {
		return nil, fmt.Errorf("vz: memory balloon device: %w", err)
	}
	config.SetMemoryBalloonDevicesVirtualMachineConfiguration([]vz.MemoryBalloonDeviceConfiguration{balloon})

	if len(shares) > 0 {
		for _, share := range shares {
			if share.Tag == "" {
				return nil, fmt.Errorf("vz: a Linux guest's share of %s needs a tag to mount it by", share.HostPath)
			}
		}
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
