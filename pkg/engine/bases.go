package engine

import (
	"context"
	"errors"
	"fmt"

	"github.com/discobox-ai/vm/pkg/image"
	"github.com/discobox-ai/vm/pkg/machine"
)

// ErrBase reports a base where a command needs an image: a base has no layer
// to tag or remove.
var ErrBase = errors.New("not an image")

// Base finds the driver's base that ref names, by name or by ID
// (machine.Driver.Bases). The store's references come first everywhere one is
// taken, so a tag of the same name hides a base; its ID still reaches it.
func (e *Engine) Base(ctx context.Context, ref string) (machine.Base, bool, error) {
	bases, err := e.Driver.Bases(ctx)
	if err != nil {
		return machine.Base{}, false, err
	}
	for _, b := range bases {
		if b.Name == ref || b.ID == ref {
			return b, true, nil
		}
	}
	return machine.Base{}, false, nil
}

// ResolveBase is the base ref names when the store has no image of that name
// (err, from the store's Resolve, is image.ErrNotFound). Any other error, or a
// ref that is no base either, is returned as it came. Everything that takes a
// ref resolves a base this way, after the store.
func (e *Engine) ResolveBase(ctx context.Context, ref string, err error) (machine.Base, error) {
	if !errors.Is(err, image.ErrNotFound) {
		return machine.Base{}, err
	}
	b, found, listErr := e.Base(ctx, ref)
	if listErr != nil {
		return machine.Base{}, fmt.Errorf("%w; and listing %s's bases: %w", err, e.Driver.Name(), listErr)
	}
	if !found {
		return machine.Base{}, err
	}
	return b, nil
}

// NotABase is err, from the store's Resolve of ref, unless ref names one of
// the driver's bases: then it is ErrBase, saying what to do instead of what a
// command that needs an image cannot do with one.
func (e *Engine) NotABase(ctx context.Context, ref string, err error) error {
	if !errors.Is(err, image.ErrNotFound) {
		return err
	}
	if _, found, _ := e.Base(ctx, ref); found {
		return fmt.Errorf("%s is one of %s's bases, %w; run it, stop the instance, and `disco-vm snapshot` that to make one", ref, e.Driver.Name(), ErrBase)
	}
	return err
}
