package served

import (
	"context"
	"fmt"
	"io"

	"github.com/discobox-ai/vm/pkg/machine"
)

// View is never served: a window opens in the process that holds the
// driver, and the server reports no Capabilities.View.
func (*Driver) View(context.Context, machine.Instance, string) error {
	return fmt.Errorf("served: a window is never served: %w", machine.ErrUnsupported)
}

// ViewInstall is never served, as Install is not.
func (*Driver) ViewInstall(context.Context, string, string, io.Writer) error {
	return fmt.Errorf("served: a window on an install is never served: %w", machine.ErrUnsupported)
}
