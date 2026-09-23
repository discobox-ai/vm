//go:build darwin && !cgo

package vz

import (
	"context"
	"errors"

	"github.com/discobox-ai/vm/pkg/machine"
)

// errNoCgo is every framework call in a build without cgo: the bindings are
// Objective-C.
var errNoCgo = errors.New("vz: this disco-vm was built without cgo, and Virtualization.framework needs it; rebuild with CGO_ENABLED=1 (make build)")

func (*Driver) Check(context.Context) error { return errNoCgo }

func (*Driver) Install(context.Context, machine.InstallSpec, machine.Layer) error { return errNoCgo }

func (*Driver) Prepare(context.Context, machine.InstanceSpec) error { return errNoCgo }

func (*Driver) Boot(context.Context, machine.InstanceSpec, machine.BootOptions) (machine.Machine, error) {
	return nil, errNoCgo
}

func (*Driver) Warm(context.Context, machine.WarmSpec) (machine.Stage, error) { return nil, errNoCgo }

func (*Driver) Warmth(context.Context, machine.WarmSpec) (machine.Warmth, error) {
	return machine.Warmth{Mode: machine.Cold}, nil
}
