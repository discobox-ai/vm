package engine

import (
	"context"

	"github.com/discobox-ai/vm/pkg/converge"
)

// Converge makes a running instance match state files, which are read here,
// on the client, with the files they name. It holds a lease on the guest for
// as long as it runs, so a second caller is refused with
// converge.ErrLeaseHeld. See docs/state-file.md.
func (e *Engine) Converge(ctx context.Context, inst *Instance, opts converge.Options) error {
	client := e.Guest(inst)
	defer client.Close()
	return converge.Run(ctx, client, opts)
}

// ConvergeStatus is how an instance's last Converge left it. An instance that
// was never converged has a status with no State.
func (e *Engine) ConvergeStatus(ctx context.Context, inst *Instance) (converge.Status, error) {
	client := e.Guest(inst)
	defer client.Close()
	return converge.ReadStatus(ctx, client)
}
