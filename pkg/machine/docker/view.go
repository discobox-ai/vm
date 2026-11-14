package docker

import (
	"context"
	"io"

	"github.com/discobox-ai/vm/pkg/machine"
)

// View is unsupported: docker shows no guest's desktop (Capabilities.View).
func (*Driver) View(context.Context, machine.Instance, string) error {
	return machine.ErrUnsupported
}

// ViewInstall is unsupported: docker shows no guest's desktop
// (Capabilities.View).
func (*Driver) ViewInstall(context.Context, string, string, io.Writer) error {
	return machine.ErrUnsupported
}
