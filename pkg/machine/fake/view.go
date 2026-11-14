package fake

import (
	"context"
	"io"
	"net"

	"github.com/discobox-ai/vm/pkg/machine"
	"github.com/discobox-ai/vm/pkg/machine/local"
)

// View is unsupported: a fake guest has no desktop (Capabilities.View).
func (*Hypervisor) View(context.Context, machine.Machine, func(context.Context) (net.Conn, error), string) error {
	return machine.ErrUnsupported
}

// ViewInstall is unsupported: a fake guest has no desktop
// (Capabilities.View).
func (*Hypervisor) ViewInstall(context.Context, local.Layer, string, io.Writer) error {
	return machine.ErrUnsupported
}
