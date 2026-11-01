package vz

import (
	"context"
	"fmt"
	"os"

	"github.com/discobox-ai/vm/pkg/guest"
	"github.com/discobox-ai/vm/pkg/machine"
	"github.com/discobox-ai/vm/pkg/machine/local"
)

func init() {
	machine.Register("vz", func(cfg machine.Config) (machine.Driver, error) { return local.New(cfg, &Hypervisor{}) })
}

// Hypervisor runs macOS and Linux guests under Virtualization.framework,
// under a local.Driver: macOS installed from media, Linux from a base
// (ADR 26-10-31-990).
type Hypervisor struct{}

var _ local.Hypervisor = (*Hypervisor)(nil)

func (*Hypervisor) Name() string { return "vz" }

// Capabilities: macOS guests, installed from media, and the framework's
// limit of two concurrently running ones; Linux guests, from bases, which do
// not count against it; and an instance with no network device, the empty
// fence.
func (*Hypervisor) Capabilities() machine.Capabilities {
	return machine.Capabilities{
		GuestOS:           []machine.OS{machine.Darwin, machine.Linux},
		MaxRunning:        map[machine.OS]int{machine.Darwin: 2},
		SharedDirectories: true,
		Display:           true,
		Ports:             true,
		Agent:             true,
		Install:           []machine.OS{machine.Darwin},
		Keys:              machine.KeyCapabilities{Mode: machine.KeysNone},
		Egress:            []machine.EgressFence{machine.EgressNone},
	}
}

// Chains is false: an instance is an APFS clone of its image, and a snapshot
// is the instance's bundle moved, so each image's files are its own.
func (*Hypervisor) Chains() bool { return false }

// SharesLoopback is false: a guest reaches neither another guest nor the
// host's loopback.
func (*Hypervisor) SharesLoopback() bool { return false }

// Commit moves the stopped instance's disk into the image, with a Linux
// guest's kernel and initrd. The MAC address and the machine's identity stay
// behind: they are the running machine's, and every clone of the image gets
// its own.
func (*Hypervisor) Commit(_ context.Context, inst local.InstanceSpec, dst local.Layer) error {
	return moveLayer(bundle(inst.Dir), bundle(dst.Dir), inst.GuestOS)
}

func (*Hypervisor) Destroy(_ context.Context, inst local.InstanceSpec) error {
	return os.RemoveAll(inst.Dir)
}

// Bases is the Linux guest image disco-vm runs by default (LinuxBaseName),
// and every one already cached. A macOS guest's first image is installed
// from media.
func (*Hypervisor) Bases(_ context.Context, cacheDir string) ([]machine.Base, error) {
	return linuxBases(cacheDir), nil
}

// Base is the Linux base ref names: LinuxBaseName, a guest image's
// reference, which Prepare pulls, or a directory holding its artifacts.
func (*Hypervisor) Base(_ context.Context, cacheDir, ref string) (machine.Base, bool, error) {
	if b, found := machine.FindBase(linuxBases(cacheDir), ref); found {
		return b, true, nil
	}
	b, found := linuxBase(ref)
	return b, found, nil
}

// Service is guest.ServicePort over vsock; the instance's shim serves it on
// the host's loopback.
func (*Hypervisor) Service(local.InstanceSpec) (string, uint32, error) {
	return fmt.Sprintf("vsock:%d", guest.ServicePort), guest.ServicePort, nil
}
