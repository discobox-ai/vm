package hcs

import (
	"context"
	"fmt"

	"github.com/discobox-ai/vm/pkg/machine"
)

func init() {
	machine.Register("hcs", func() (machine.Driver, error) { return &Driver{}, nil })
}

// Driver runs Windows guests as HCS virtual machines.
type Driver struct{}

var (
	_ machine.Driver = (*Driver)(nil)
	_ machine.Warmer = (*Driver)(nil)
)

func (*Driver) Name() string { return "hcs" }

// Capabilities is what the driver is designed to offer, which is what sandboxw
// has proven on HCS: live-template fork (many clones in about a second) and no
// concurrency cap on guests booted from our own image.
func (*Driver) Capabilities() machine.Capabilities {
	return machine.Capabilities{
		GuestOS:    []machine.OS{machine.Windows},
		CloneModes: []machine.CloneMode{machine.Cold, machine.Fork},
		Display:    true,
	}
}

func notYet(op string) error {
	return fmt.Errorf("hcs: %s: %w (docs/drivers/hcs.md)", op, machine.ErrNotImplemented)
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

func (*Driver) Warm(context.Context, machine.WarmSpec) (machine.Stage, error) {
	return nil, notYet("warm")
}

func (*Driver) Warmth(context.Context, machine.WarmSpec) (machine.Warmth, error) {
	return machine.Warmth{Mode: machine.Cold}, notYet("warmth")
}

func (*Driver) Cool(context.Context, machine.WarmSpec) error { return notYet("cool") }
