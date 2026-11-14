package vz

import (
	"context"
	"io"
	"net"

	"github.com/discobox-ai/vm/pkg/machine"
	"github.com/discobox-ai/vm/pkg/machine/local"
)

// View is unsupported: vz's window needs the VM object the process that
// booted it holds, so it opens only at boot (BootOptions.GUI), never from
// another process (Capabilities.View).
func (*Hypervisor) View(context.Context, machine.Machine, func(context.Context) (net.Conn, error), string) error {
	return machine.ErrUnsupported
}

// ViewInstall is unsupported: an install's window opens only from the
// install (Capabilities.View).
func (*Hypervisor) ViewInstall(context.Context, local.Layer, string, io.Writer) error {
	return machine.ErrUnsupported
}
