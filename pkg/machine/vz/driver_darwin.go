package vz

import (
	"context"
	"fmt"
	"os"

	"github.com/discobox-ai/vm/pkg/guest"
	"github.com/discobox-ai/vm/pkg/machine"
)

func init() {
	machine.Register("vz", func() (machine.Driver, error) { return &Driver{}, nil })
}

// Driver runs macOS guests under Virtualization.framework.
type Driver struct{}

var _ machine.Driver = (*Driver)(nil)

func (*Driver) Name() string { return "vz" }

// Capabilities: macOS guests, and the framework's limit of two concurrently
// running ones.
func (*Driver) Capabilities() machine.Capabilities {
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
func (*Driver) Commit(_ context.Context, inst machine.InstanceSpec, dst machine.Layer) error {
	return moveLayer(bundle(inst.Dir), bundle(dst.Dir))
}

func (*Driver) Destroy(_ context.Context, inst machine.InstanceSpec) error {
	return os.RemoveAll(inst.Dir)
}

func (*Driver) DeleteLayer(context.Context, machine.Layer) error { return nil }

// LayerExists is true: a layer lives only in the store.
func (*Driver) LayerExists(context.Context, machine.Layer) (bool, error) { return true, nil }

// TagLayer and UntagLayer do nothing: a layer lives only in the store.
func (*Driver) TagLayer(context.Context, machine.Layer, string) error   { return nil }
func (*Driver) UntagLayer(context.Context, machine.Layer, string) error { return nil }

// Orphans is empty: nothing the driver makes lives outside the store.
func (*Driver) Orphans(context.Context, machine.Known) ([]machine.Orphan, error) { return nil, nil }

// RemoveOrphan is never asked of a driver that lists no orphans.
func (*Driver) RemoveOrphan(context.Context, machine.Orphan) error { return machine.ErrUnsupported }

// Service is guest.ServicePort over vsock; the engine's shim serves it on the
// host's loopback.
func (*Driver) Service(context.Context, machine.InstanceSpec) (machine.Service, error) {
	return machine.Service{Listen: fmt.Sprintf("vsock:%d", guest.ServicePort), Port: guest.ServicePort}, nil
}
