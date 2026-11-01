//go:build darwin && !cgo

package vz

import (
	"context"
	"errors"

	"github.com/discobox-ai/vm/pkg/machine"
	"github.com/discobox-ai/vm/pkg/machine/local"
)

// errNoCgo is every framework call in a build without cgo: the bindings are
// Objective-C.
var errNoCgo = errors.New("vz: this disco-vm was built without cgo, and Virtualization.framework needs it; rebuild with CGO_ENABLED=1 (go tool task build)")

func (*Hypervisor) Check(context.Context) error { return errNoCgo }

func (*Hypervisor) Install(context.Context, local.InstallSpec, local.Layer) error { return errNoCgo }

func (*Hypervisor) Prepare(context.Context, local.InstanceSpec) error { return errNoCgo }

func (*Hypervisor) Boot(context.Context, local.InstanceSpec, local.BootOptions) (machine.Machine, error) {
	return nil, errNoCgo
}
