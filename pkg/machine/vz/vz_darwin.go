package vz

import (
	"context"
	"fmt"

	"github.com/discobox-ai/vm/pkg/machine"
)

func init() {
	machine.Register("vz", func() (machine.Driver, error) { return &Driver{}, nil })
}

// Driver runs macOS (and later Linux) guests under Virtualization.framework.
type Driver struct{}

var _ machine.Driver = (*Driver)(nil)

func (*Driver) Name() string { return "vz" }

// Capabilities is what the driver is designed to offer, which is what the macvm
// proof of concept has shown: APFS clones, save/restore of machine state, and
// the framework's limit of two concurrently running macOS guests.
func (*Driver) Capabilities() machine.Capabilities {
	return machine.Capabilities{
		GuestOS:           []machine.OS{machine.Darwin},
		CloneModes:        []machine.CloneMode{machine.Cold, machine.Resume},
		MaxRunning:        map[machine.OS]int{machine.Darwin: 2},
		SharedDirectories: true,
		Display:           true,
	}
}

func notYet(op string) error {
	return fmt.Errorf("vz: %s: %w (docs/drivers/vz.md)", op, machine.ErrNotImplemented)
}

func (*Driver) Check(context.Context) error { return notYet("check") }

func (*Driver) Install(context.Context, machine.InstallSpec, machine.Layer) error {
	return notYet("install")
}

func (*Driver) Prepare(context.Context, machine.InstanceSpec) error { return notYet("prepare") }

func (*Driver) Boot(context.Context, machine.InstanceSpec, machine.BootOptions) (machine.Machine, error) {
	return nil, notYet("boot")
}

func (*Driver) Commit(context.Context, machine.InstanceSpec, machine.Layer) error {
	return notYet("commit")
}

func (*Driver) Destroy(context.Context, machine.InstanceSpec) error { return notYet("destroy") }

func (*Driver) DeleteLayer(context.Context, machine.Layer) error { return nil }
