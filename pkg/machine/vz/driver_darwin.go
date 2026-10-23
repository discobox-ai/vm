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

// Hypervisor runs macOS guests under Virtualization.framework, under a
// local.Driver.
type Hypervisor struct{}

var _ local.Hypervisor = (*Hypervisor)(nil)

func (*Hypervisor) Name() string { return "vz" }

// Capabilities: macOS guests, and the framework's limit of two concurrently
// running ones.
func (*Hypervisor) Capabilities() machine.Capabilities {
	return machine.Capabilities{
		GuestOS:           []machine.OS{machine.Darwin},
		MaxRunning:        map[machine.OS]int{machine.Darwin: 2},
		SharedDirectories: true,
		Display:           true,
		Forward:           true,
		Agent:             true,
	}
}

// Commit moves the stopped instance's disk into the layer. The MAC address and
// the machine identifier stay behind: they are the running machine's identity,
// and every clone of the layer gets its own.
func (*Hypervisor) Commit(_ context.Context, inst local.InstanceSpec, dst local.Layer) error {
	return moveLayer(bundle(inst.Dir), bundle(dst.Dir))
}

func (*Hypervisor) Destroy(_ context.Context, inst local.InstanceSpec) error {
	return os.RemoveAll(inst.Dir)
}

// Bases is empty: every base layer is installed from media.
func (*Hypervisor) Bases(context.Context) ([]machine.Base, error) { return nil, nil }

// Service is guest.ServicePort over vsock; the instance's shim serves it on
// the host's loopback.
func (*Hypervisor) Service(local.InstanceSpec) (string, uint32, error) {
	return fmt.Sprintf("vsock:%d", guest.ServicePort), guest.ServicePort, nil
}
