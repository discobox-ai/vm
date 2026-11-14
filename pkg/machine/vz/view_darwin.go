package vz

import (
	"context"
	"io"
	"net"

	"github.com/discobox-ai/vm/pkg/machine"
	"github.com/discobox-ai/vm/pkg/machine/local"
)

// View is not implemented yet: vz's window needs the VM object the process
// that booted it holds, so it opens only at boot (BootOptions.GUI). A window
// after boot is deferred (ADR 26-11-02-274); until then vz does not report
// Capabilities.View, and the engine refuses gui before asking.
func (*Hypervisor) View(context.Context, machine.Machine, func(context.Context) (net.Conn, error), string) error {
	return machine.ErrNotImplemented
}

// ViewInstall is not implemented yet: an install's window opens only from
// the install (install --gui).
func (*Hypervisor) ViewInstall(context.Context, local.Layer, string, io.Writer) error {
	return machine.ErrNotImplemented
}
