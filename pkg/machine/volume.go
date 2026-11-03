package machine

import (
	"errors"
	"fmt"
	"time"
)

// Volume is one of the two volumes an instance can have besides its root
// disk (ADR 26-11-02-180). There are only these two, and every driver mounts
// each at the same path for a guest OS (VolumePath).
type Volume string

const (
	// VolumeData is the instance's own: made at Create, kept across stop and
	// start, removed by Destroy, and never part of a committed VM.
	VolumeData Volume = "data"
	// VolumeCache is a private clone of a cache key's current generation,
	// made at Create, which only SaveCache publishes.
	VolumeCache Volume = "cache"
)

// VolumePath is where every driver mounts v in a guest of os:
// /.discobox/data and /.discobox/cache, and C:\.discobox\... on Windows.
func VolumePath(os OS, v Volume) string {
	if os == Windows {
		return `C:\.discobox\` + string(v)
	}
	return "/.discobox/" + string(v)
}

// DataVolume is an instance's data volume.
type DataVolume struct {
	// Bytes is its size; zero is the driver's default. A provider that sizes
	// storage holds it as a cap, and one whose storage grows on demand takes
	// it as a hint.
	Bytes int64 `json:"bytes,omitempty"`
}

// CacheVolume is an instance's cache volume: a private, writable clone of
// the generation its key held when the instance was created, or an empty
// volume where the key held none.
type CacheVolume struct {
	// Key names the project's cache the instance starts from
	// (ValidateCacheKey).
	Key string `json:"key"`
	// Bytes is its size, as DataVolume's: a clone smaller than it grows to
	// it.
	Bytes int64 `json:"bytes,omitempty"`
	// Generation is what the driver reports: the generation the volume was
	// cloned from, or empty where the key held none. A spec leaves it empty.
	Generation string `json:"generation,omitempty"`
}

// Cache is one of a project's cache keys and the generation it points at.
type Cache struct {
	Key string `json:"key"`
	// Generation is the driver's ID of the key's current generation, an
	// immutable snapshot of a cache volume.
	Generation string    `json:"generation"`
	Bytes      int64     `json:"bytes,omitempty"`
	Saved      time.Time `json:"saved,omitzero"`
}

// CacheSave publishes a stopped instance's cache volume as a key's new
// generation (Driver.SaveCache).
type CacheSave struct {
	// Key is the key to move to the new generation. It may differ from the
	// key the instance started from.
	Key string
	// Guarded refuses the save, with ErrCacheMoved, unless Key holds
	// IfGeneration, where an empty IfGeneration is no generation at all.
	Guarded      bool
	IfGeneration string
}

// ErrCacheMoved reports a guarded save whose key no longer holds the
// generation the caller expected (CacheSave.Guarded): nothing was saved.
var ErrCacheMoved = errors.New("machine: the cache key has moved since")

// ValidateCacheKey checks a cache key: 1 to 63 lowercase letters, digits,
// and single hyphens, as an instance name.
func ValidateCacheKey(key string) error {
	if ValidateName(key) != nil {
		return fmt.Errorf("machine: cache key %q is not 1 to 63 lowercase letters, digits, and single hyphens, starting and ending with a letter or digit", key)
	}
	return nil
}
