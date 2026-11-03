package local

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"time"

	"github.com/discobox-ai/vm/internal/fsutil"
	"github.com/discobox-ai/vm/pkg/machine"
)

// A cache key is a file, caches/<project>/<key>.json, naming its current
// generation, which is a directory, caches/generations/<id>, written under a
// temporary name and renamed into place as an image is. A key moves by
// replacing its file. Every instance's cache volume is a copy of its own, so
// nothing but a key needs a generation once it has moved off it.

const generationFile = "generation.json"

func (d *Driver) cachesDir() string              { return filepath.Join(d.dir, "caches", d.project) }
func (d *Driver) cacheFile(key string) string    { return filepath.Join(d.cachesDir(), key+".json") }
func (d *Driver) generationsDir() string         { return filepath.Join(d.dir, "caches", "generations") }
func (d *Driver) generationDir(id string) string { return filepath.Join(d.generationsDir(), id) }
func (d *Driver) generationLayer(id string) Layer {
	return Layer{ID: id, Dir: filepath.Join(d.generationDir(id), payloadDir)}
}
func (d *Driver) generationRecord(id string) string {
	return filepath.Join(d.generationDir(id), generationFile)
}

// generation is a generation's generation.json.
type generation struct {
	ID      string    `json:"id"`
	Project string    `json:"project"`
	Key     string    `json:"key"`
	Bytes   int64     `json:"bytes,omitempty"`
	Saved   time.Time `json:"saved"`
}

// readCache reads a key's record, found false where the key has none, or its
// generation is gone (removed behind the driver's back).
func (d *Driver) readCache(key string) (machine.Cache, bool, error) {
	var c machine.Cache
	err := fsutil.ReadJSON(d.cacheFile(key), &c)
	if errors.Is(err, fs.ErrNotExist) {
		return machine.Cache{}, false, nil
	}
	if err != nil {
		return machine.Cache{}, false, err
	}
	if c.Key != key || !validID(c.Generation) {
		return machine.Cache{}, false, fmt.Errorf("local: cache key %s: a record naming %q and %q", key, c.Key, c.Generation)
	}
	var g generation
	if err := fsutil.ReadJSON(d.generationRecord(c.Generation), &g); errors.Is(err, fs.ErrNotExist) {
		return machine.Cache{}, false, nil
	} else if err != nil {
		return machine.Cache{}, false, err
	}
	return c, true, nil
}

// Caches reads every key of the project. A record that cannot be read is an
// error, not a key left out.
func (d *Driver) Caches(ctx context.Context) ([]machine.Cache, error) {
	if err := d.authorize(ctx); err != nil {
		return nil, err
	}
	if !d.hv.Capabilities().Attaches(machine.VolumeCache) {
		return nil, nil
	}
	entries, err := os.ReadDir(d.cachesDir())
	if errors.Is(err, fs.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var out []machine.Cache
	for _, entry := range entries {
		key, ok := strings.CutSuffix(entry.Name(), ".json")
		if !ok || machine.ValidateCacheKey(key) != nil {
			continue
		}
		c, found, err := d.readCache(key)
		if err != nil {
			return nil, err
		}
		if found {
			out = append(out, c)
		}
	}
	slices.SortFunc(out, func(a, b machine.Cache) int { return strings.Compare(a.Key, b.Key) })
	return out, nil
}

// SaveCache writes the stopped instance's cache volume as a new generation,
// then moves the key to it, checking a guarded save's generation before it
// writes and again just before it moves the key, and then removes the
// generation the key moved off. Two saves racing in the same moment may both
// pass the check: the last to move the key wins.
func (d *Driver) SaveCache(ctx context.Context, inst machine.Instance, save machine.CacheSave) (machine.Cache, error) {
	if err := d.authorizeInstance(ctx, inst); err != nil {
		return machine.Cache{}, err
	}
	if !d.hv.Capabilities().Attaches(machine.VolumeCache) {
		return machine.Cache{}, fmt.Errorf("%s: a cache volume: %w", d.hv.Name(), machine.ErrUnsupported)
	}
	if err := machine.ValidateCacheKey(save.Key); err != nil {
		return machine.Cache{}, err
	}
	rec, err := d.readProject(inst.ID)
	if err != nil {
		return machine.Cache{}, err
	}
	if rec.Cache == nil {
		return machine.Cache{}, fmt.Errorf("instance %s has no cache volume", inst.Name)
	}
	if shim, err := d.openShim(inst.ID); err == nil && shim.status(ctx) == nil {
		return machine.Cache{}, fmt.Errorf("instance %s is running; stop it first", inst.Name)
	}
	if _, err := d.guard(save); err != nil {
		return machine.Cache{}, err
	}
	hs, err := d.spec(rec)
	if err != nil {
		return machine.Cache{}, err
	}
	g := generation{ID: fsutil.RandomHex(16), Project: d.project, Key: save.Key, Bytes: rec.Cache.Bytes, Saved: time.Now().UTC()}
	tmp := filepath.Join(d.generationsDir(), ".tmp-"+g.ID)
	if err := os.MkdirAll(filepath.Join(tmp, payloadDir), 0o755); err != nil {
		return machine.Cache{}, err
	}
	if err := d.hv.SaveCache(ctx, hs, Layer{ID: g.ID, Dir: filepath.Join(tmp, payloadDir)}); err != nil {
		_ = os.RemoveAll(tmp)
		return machine.Cache{}, err
	}
	if err := fsutil.WriteJSON(filepath.Join(tmp, generationFile), g); err != nil {
		_ = os.RemoveAll(tmp)
		return machine.Cache{}, err
	}
	if err := os.Rename(tmp, d.generationDir(g.ID)); err != nil {
		_ = os.RemoveAll(tmp)
		return machine.Cache{}, err
	}
	old, err := d.guard(save)
	if err == nil {
		err = os.MkdirAll(d.cachesDir(), 0o755)
	}
	c := machine.Cache{Key: save.Key, Generation: g.ID, Bytes: g.Bytes, Saved: g.Saved}
	if err == nil {
		err = fsutil.WriteJSON(d.cacheFile(save.Key), c)
	}
	if err != nil {
		d.removeGeneration(g.ID)
		return machine.Cache{}, err
	}
	if old != "" && old != g.ID {
		d.removeGeneration(old)
	}
	return c, nil
}

// guard refuses a guarded save whose key does not hold the generation it
// expects, and returns the generation the key holds.
func (d *Driver) guard(save machine.CacheSave) (string, error) {
	c, found, err := d.readCache(save.Key)
	if err != nil {
		return "", err
	}
	if save.Guarded && c.Generation != save.IfGeneration {
		if !found {
			return "", fmt.Errorf("cache %s holds no generation, not %s: %w", save.Key, save.IfGeneration, machine.ErrCacheMoved)
		}
		return "", fmt.Errorf("cache %s holds generation %s: %w", save.Key, c.Generation, machine.ErrCacheMoved)
	}
	return c.Generation, nil
}

// DeleteCache removes the key's record, then its generation.
func (d *Driver) DeleteCache(ctx context.Context, key string) error {
	if err := d.authorize(ctx); err != nil {
		return err
	}
	if !d.hv.Capabilities().Attaches(machine.VolumeCache) {
		return fmt.Errorf("%s: a cache volume: %w", d.hv.Name(), machine.ErrUnsupported)
	}
	if err := machine.ValidateCacheKey(key); err != nil {
		return err
	}
	var c machine.Cache
	err := fsutil.ReadJSON(d.cacheFile(key), &c)
	if errors.Is(err, fs.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	if err := os.Remove(d.cacheFile(key)); err != nil && !errors.Is(err, fs.ErrNotExist) {
		return err
	}
	if validID(c.Generation) {
		d.removeGeneration(c.Generation)
	}
	return nil
}

// removeGeneration renames a generation out of reach, then removes it. A
// Create cloning it in that moment fails and is retried with the key's new
// one (cacheFrom).
func (d *Driver) removeGeneration(id string) {
	gone := filepath.Join(d.generationsDir(), ".del-"+id)
	if err := os.Rename(d.generationDir(id), gone); err == nil {
		_ = os.RemoveAll(gone)
	}
}

// cacheFrom is the generation an instance's cache volume is cloned from:
// the key's current one, or nil for none.
func (d *Driver) cacheFrom(key string) (*Layer, string, error) {
	c, found, err := d.readCache(key)
	if err != nil || !found {
		return nil, "", err
	}
	l := d.generationLayer(c.Generation)
	return &l, c.Generation, nil
}

// generationGone reports that a generation a Create cloned from was removed
// while it did.
func (d *Driver) generationGone(id string) bool {
	_, err := os.Stat(d.generationRecord(id))
	return errors.Is(err, fs.ErrNotExist)
}
