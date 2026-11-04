package engine

import (
	"context"
	"fmt"

	"github.com/discobox-ai/vm/pkg/machine"
)

// checkVolumes refuses a volume the driver does not attach, a negative size,
// and a cache key that is not one. It never falls back to a directory on the
// root disk (ADR 26-11-02-180).
func (e *Engine) checkVolumes(data *machine.DataVolume, cache *machine.CacheVolume) error {
	caps := e.Driver.Capabilities()
	if data != nil {
		if !caps.Attaches(machine.VolumeData) {
			return fmt.Errorf("driver %s cannot attach a data volume: %w", e.Driver.Name(), machine.ErrUnsupported)
		}
		if data.Bytes < 0 {
			return fmt.Errorf("a data volume of %d bytes", data.Bytes)
		}
	}
	if cache != nil {
		if !caps.Attaches(machine.VolumeCache) {
			return fmt.Errorf("driver %s cannot attach a cache volume: %w", e.Driver.Name(), machine.ErrUnsupported)
		}
		if cache.Bytes < 0 {
			return fmt.Errorf("a cache volume of %d bytes", cache.Bytes)
		}
		if err := machine.ValidateCacheKey(cache.Key); err != nil {
			return err
		}
	}
	return nil
}

// Caches lists the pool's cache keys.
func (e *Engine) Caches(ctx context.Context) ([]machine.Cache, error) {
	return e.Driver.Caches(ctx)
}

// SaveOptions is how SaveCache publishes an instance's cache volume.
type SaveOptions struct {
	// Key is the key to move; empty is the key the instance started from.
	Key string
	// Force saves unguarded. Otherwise a save to the key the instance
	// started from is refused, with machine.ErrCacheMoved, once that key
	// holds another generation than the one the instance was cloned from,
	// so a run that started from an older cache never replaces a newer one.
	Force bool
}

// SaveCache snapshots a stopped instance's cache volume as a new generation
// and moves a key to it. The instance and its volume are kept.
func (e *Engine) SaveCache(ctx context.Context, inst Instance, opts SaveOptions) (machine.Cache, error) {
	if inst.Cache == nil {
		return machine.Cache{}, fmt.Errorf("instance %s has no cache volume", inst.Name)
	}
	save := machine.CacheSave{Key: opts.Key}
	if save.Key == "" {
		save.Key = inst.Cache.Key
	}
	if err := machine.ValidateCacheKey(save.Key); err != nil {
		return machine.Cache{}, err
	}
	if !opts.Force && save.Key == inst.Cache.Key {
		save.Guarded, save.IfGeneration = true, inst.Cache.Generation
	}
	if state := e.State(ctx, inst); state != Stopped {
		return machine.Cache{}, fmt.Errorf("instance %s is %s; stop it first", inst.Name, state)
	}
	c, err := e.Driver.SaveCache(ctx, inst, save)
	if err != nil {
		return machine.Cache{}, fmt.Errorf("save the cache of %s as %s: %w", inst.Name, save.Key, err)
	}
	return c, nil
}

// DeleteCache drops a cache key and its generation. Instances cloned from it
// keep their own volumes.
func (e *Engine) DeleteCache(ctx context.Context, key string) error {
	if err := machine.ValidateCacheKey(key); err != nil {
		return err
	}
	return e.Driver.DeleteCache(ctx, key)
}
