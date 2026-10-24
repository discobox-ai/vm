package engine

import (
	"context"
	"errors"
	"fmt"

	"github.com/discobox-ai/vm/pkg/machine"
)

// ErrBase reports a base where a command needs an image: a base is the
// driver's, with no image to tag or remove.
var ErrBase = errors.New("not an image")

// ResolveBase is the driver's base ref names, by name or ID
// (machine.Driver.Base), when there is no image of that name (err, from
// Resolve, is ErrImageNotFound). Any other error, or a ref that is no base
// either, is returned as it came. Everything that takes a ref resolves a base
// this way, after the images, so a tag of the same name hides a base; its ID
// still reaches it.
func (e *Engine) ResolveBase(ctx context.Context, ref string, err error) (machine.Base, error) {
	if !errors.Is(err, ErrImageNotFound) {
		return machine.Base{}, err
	}
	b, found, listErr := e.Driver.Base(ctx, ref)
	if listErr != nil {
		return machine.Base{}, fmt.Errorf("%w; and asking %s for a base of that name: %w", err, e.Driver.Name(), listErr)
	}
	if !found {
		return machine.Base{}, err
	}
	return b, nil
}

// NotABase is err, from Resolve of ref, unless ref names one of
// the driver's bases: then it is ErrBase, saying what to do instead of what a
// command that needs an image cannot do with one.
func (e *Engine) NotABase(ctx context.Context, ref string, err error) error {
	if !errors.Is(err, ErrImageNotFound) {
		return err
	}
	if _, found, _ := e.Driver.Base(ctx, ref); found {
		return fmt.Errorf("%s is one of %s's bases, %w; run it, stop the instance, and `disco-vm snapshot` it to make one", ref, e.Driver.Name(), ErrBase)
	}
	return err
}
