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
	// The base is checked where a fresh VM is made from it (Prepare), so
	// a moved guest-image directory, or a base meant for another driver,
	// stops nothing but that.
	machine.Register("vz", func(cfg machine.Config) (machine.Driver, error) {
		return local.New(cfg, &Hypervisor{base: cfg.Base})
	})
}

// Hypervisor runs macOS and Linux guests under Virtualization.framework,
// under a local.Driver: macOS installed from media, Linux fresh from its
// base (ADR 26-10-31-990, 26-11-03-606), and either created from a
// committed VM.
type Hypervisor struct {
	// base is the Linux guest image a fresh VM starts from
	// (machine.Config.Base), as parseLinuxBase reads it; empty is
	// DefaultLinuxBase.
	base string
}

var _ local.Hypervisor = (*Hypervisor)(nil)

func (*Hypervisor) Name() string { return "vz" }

// Capabilities: macOS guests, installed from media, and the framework's
// limit of two concurrently running ones; Linux guests, from its base, which do
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
		Commit:            true,
		Keys:              machine.KeyCapabilities{Mode: machine.KeysNone},
		Egress:            []machine.EgressFence{machine.EgressNone},
	}
}

// Chains is false: a VM is an APFS clone of the committed VM it was created
// from, so each bundle's files are its own.
func (*Hypervisor) Chains() bool { return false }

// Base is Linux: a fresh VM is a Linux guest from the configured guest
// image. A macOS guest is installed from media.
func (*Hypervisor) Base() (machine.OS, bool) { return machine.Linux, true }

// SharesLoopback is false: a guest reaches neither another guest nor the
// host's loopback.
func (*Hypervisor) SharesLoopback() bool { return false }

// Commit freezes the stopped VM's bundle in place: its layer files (its disk,
// with a Linux guest's kernel and initrd) stay, and everything else goes,
// the MAC address and the machine's identity among them, since they are the
// running machine's and every clone gets its own. vz attaches no volumes.
func (*Hypervisor) Commit(_ context.Context, inst local.InstanceSpec) error {
	return freezeLayer(bundle(inst.Dir), inst.GuestOS)
}

// SaveCache is unsupported: vz attaches no volume yet (ADR 26-11-02-180).
func (*Hypervisor) SaveCache(context.Context, local.InstanceSpec, local.Layer) error {
	return fmt.Errorf("vz: a cache volume: %w", machine.ErrUnsupported)
}

func (*Hypervisor) Destroy(_ context.Context, inst local.InstanceSpec) error {
	return os.RemoveAll(inst.Dir)
}

// Service is guest.ServicePort over vsock; the instance's shim serves it on
// the host's loopback.
func (*Hypervisor) Service(local.InstanceSpec) (string, uint32, error) {
	return fmt.Sprintf("vsock:%d", guest.ServicePort), guest.ServicePort, nil
}
