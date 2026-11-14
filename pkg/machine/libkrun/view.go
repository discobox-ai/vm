//go:build linux

package libkrun

import (
	"context"
	"io"
	"net"

	"github.com/discobox-ai/vm/pkg/machine"
	"github.com/discobox-ai/vm/pkg/machine/local"
)

// View is unsupported: a libkrun guest has no display (Capabilities.View).
func (*Hypervisor) View(context.Context, machine.Machine, func(context.Context) (net.Conn, error), string) error {
	return machine.ErrUnsupported
}

// ViewInstall is unsupported: libkrun installs nothing (Capabilities.View).
func (*Hypervisor) ViewInstall(context.Context, local.Layer, string, io.Writer) error {
	return machine.ErrUnsupported
}
