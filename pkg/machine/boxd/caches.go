package boxd

import (
	"context"
	"fmt"

	"github.com/discobox-ai/vm/pkg/machine"
)

// boxd attaches no disk but a machine's root, so it has no volumes and no
// caches (ADR 26-11-02-180).

// Caches lists none.
func (*Driver) Caches(context.Context) ([]machine.Cache, error) { return nil, nil }

// SaveCache is unsupported.
func (*Driver) SaveCache(context.Context, machine.Instance, machine.CacheSave) (machine.Cache, error) {
	return machine.Cache{}, fmt.Errorf("boxd: a cache volume: %w", machine.ErrUnsupported)
}

// DeleteCache is unsupported.
func (*Driver) DeleteCache(context.Context, string) error {
	return fmt.Errorf("boxd: a cache volume: %w", machine.ErrUnsupported)
}
