// Package image is the local image store: immutable layers, each a committed
// disk state owned by a driver, and name:tag references to them.
//
// A layer's ID is its cache key — a hash of everything that produced it (its
// parent, the driver, the steps, the bytes of every file copied in) — so the
// store is also the build cache: a layer that exists never has to be built
// again. An image is a tag on a layer; its chain is the layer and its parents.
//
// Layers are driver-specific. An hcs layer is a differencing VHDX, a vz layer
// is an APFS-cloned macOS bundle, and neither can boot on the other, so every
// layer records its driver and the store refuses to mix them in one chain.
package image

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"slices"
	"sort"
	"strings"
	"time"

	"github.com/discobox-ai/vm/internal/fsutil"
	"github.com/discobox-ai/vm/pkg/machine"
)

// ErrNotFound reports an unknown reference or layer.
var ErrNotFound = errors.New("image: not found")

// Layer is one committed layer's metadata.
type Layer struct {
	ID      string     `json:"id"`
	Parent  string     `json:"parent,omitempty"`
	Driver  string     `json:"driver"`
	GuestOS machine.OS `json:"guestOS"`
	Created time.Time  `json:"created"`
	// Comment says what made the layer: "install windows", "build base:toolchains".
	Comment string `json:"comment,omitempty"`
}

// Image is a tag and the layer it names.
type Image struct {
	Ref   string `json:"ref"`
	Layer Layer  `json:"layer"`
}

// Store is an image store rooted at a directory:
//
//	layers/<id>/layer.json   metadata
//	layers/<id>/payload/     the driver's files (machine.Layer.Dir)
//	tags.json                ref -> layer id
type Store struct {
	Root string
}

// Open creates the store's directories if needed.
func Open(root string) (*Store, error) {
	if err := os.MkdirAll(filepath.Join(root, "layers"), 0o755); err != nil {
		return nil, err
	}
	return &Store{Root: root}, nil
}

func (s *Store) layerDir(id string) string { return filepath.Join(s.Root, "layers", id) }
func (s *Store) tagsPath() string          { return filepath.Join(s.Root, "tags.json") }

// MachineLayer is the driver's view of a committed layer.
func (s *Store) MachineLayer(id string) machine.Layer {
	return machine.Layer{ID: id, Dir: filepath.Join(s.layerDir(id), "payload")}
}

// Layer reads a layer's metadata.
func (s *Store) Layer(id string) (Layer, error) {
	var layer Layer
	if err := fsutil.ReadJSON(filepath.Join(s.layerDir(id), "layer.json"), &layer); err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return Layer{}, fmt.Errorf("layer %s: %w", short(id), ErrNotFound)
		}
		return Layer{}, err
	}
	return layer, nil
}

// Has reports whether a layer is committed.
func (s *Store) Has(id string) bool {
	_, err := os.Stat(filepath.Join(s.layerDir(id), "layer.json"))
	return err == nil
}

// Layers lists every committed layer.
func (s *Store) Layers() ([]Layer, error) {
	entries, err := os.ReadDir(filepath.Join(s.Root, "layers"))
	if err != nil {
		return nil, err
	}
	var layers []Layer
	for _, entry := range entries {
		if !entry.IsDir() || strings.HasPrefix(entry.Name(), ".") {
			continue
		}
		if layer, err := s.Layer(entry.Name()); err == nil {
			layers = append(layers, layer)
		}
	}
	return layers, nil
}

// Chain is the layer and its ancestors, base first — the order a driver's
// InstanceSpec.Chain wants.
func (s *Store) Chain(id string) ([]Layer, error) {
	var chain []Layer
	for id != "" {
		layer, err := s.Layer(id)
		if err != nil {
			return nil, err
		}
		if len(chain) > 0 && layer.Driver != chain[0].Driver {
			return nil, fmt.Errorf("image: layer %s (%s) has a parent from driver %s", short(chain[0].ID), chain[0].Driver, layer.Driver)
		}
		chain = append([]Layer{layer}, chain...)
		id = layer.Parent
	}
	return chain, nil
}

// MachineChain is Chain in the driver's terms.
func (s *Store) MachineChain(id string) ([]machine.Layer, error) {
	chain, err := s.Chain(id)
	if err != nil {
		return nil, err
	}
	out := make([]machine.Layer, len(chain))
	for i, layer := range chain {
		out[i] = s.MachineLayer(layer.ID)
	}
	return out, nil
}

// Pending is a layer being written. Its Dir is where the driver writes; Commit
// renames it into place atomically, so a crash leaves at most a stray
// temporary directory and never a half-written layer under its real ID.
type Pending struct {
	store *Store
	meta  Layer
	tmp   string
}

// Begin starts writing the layer described by meta.
func (s *Store) Begin(meta Layer) (*Pending, error) {
	if s.Has(meta.ID) {
		return nil, fmt.Errorf("image: layer %s already exists", short(meta.ID))
	}
	tmp := filepath.Join(s.Root, "layers", ".tmp-"+short(meta.ID)+"-"+fsutil.RandomHex(4))
	if err := os.MkdirAll(filepath.Join(tmp, "payload"), 0o755); err != nil {
		return nil, err
	}
	return &Pending{store: s, meta: meta, tmp: tmp}, nil
}

// MachineLayer is where the driver writes the layer.
func (p *Pending) MachineLayer() machine.Layer {
	return machine.Layer{ID: p.meta.ID, Dir: filepath.Join(p.tmp, "payload")}
}

// Commit publishes the layer.
func (p *Pending) Commit() (Layer, error) {
	if p.meta.Created.IsZero() {
		p.meta.Created = time.Now().UTC()
	}
	if err := fsutil.WriteJSON(filepath.Join(p.tmp, "layer.json"), p.meta); err != nil {
		return Layer{}, err
	}
	if err := os.Rename(p.tmp, p.store.layerDir(p.meta.ID)); err != nil {
		if p.store.Has(p.meta.ID) {
			// Another build committed the same inputs first; its layer is
			// identical by construction.
			_ = os.RemoveAll(p.tmp)
			return p.store.Layer(p.meta.ID)
		}
		return Layer{}, err
	}
	return p.meta, nil
}

// Abort discards the layer.
func (p *Pending) Abort() { _ = os.RemoveAll(p.tmp) }

// NormalizeRef adds ":latest" to a reference without a tag.
func NormalizeRef(ref string) string {
	ref = strings.TrimSpace(ref)
	if ref == "" {
		return ""
	}
	if i := strings.LastIndex(ref, ":"); i <= strings.LastIndex(ref, "/") {
		return ref + ":latest"
	}
	return ref
}

// ValidateRef checks a name:tag.
func ValidateRef(ref string) error {
	name, tag, _ := strings.Cut(NormalizeRef(ref), ":")
	if name == "" || tag == "" {
		return fmt.Errorf("image: invalid reference %q", ref)
	}
	for _, r := range name + tag {
		if !(r >= 'a' && r <= 'z' || r >= '0' && r <= '9' || strings.ContainsRune("._-/", r)) {
			return fmt.Errorf("image: reference %q may use only a-z, 0-9, and . _ - /", ref)
		}
	}
	return nil
}

func (s *Store) readTags() (map[string]string, error) {
	tags := map[string]string{}
	if err := fsutil.ReadJSON(s.tagsPath(), &tags); err != nil && !errors.Is(err, fs.ErrNotExist) {
		return nil, err
	}
	return tags, nil
}

func (s *Store) updateTags(update func(tags map[string]string) error) error {
	lock, err := fsutil.Acquire(filepath.Join(s.Root, "tags.lock"), 10*time.Second)
	if err != nil {
		return err
	}
	defer lock.Release()
	tags, err := s.readTags()
	if err != nil {
		return err
	}
	if err := update(tags); err != nil {
		return err
	}
	return fsutil.WriteJSON(s.tagsPath(), tags)
}

// Tag points ref at a layer.
func (s *Store) Tag(ref, id string) error {
	if err := ValidateRef(ref); err != nil {
		return err
	}
	if !s.Has(id) {
		return fmt.Errorf("layer %s: %w", short(id), ErrNotFound)
	}
	return s.updateTags(func(tags map[string]string) error {
		tags[NormalizeRef(ref)] = id
		return nil
	})
}

// Resolve turns a reference into a layer ID. It accepts name:tag, a bare name
// (meaning :latest), a full layer ID, or an unambiguous layer ID prefix of at
// least six characters.
func (s *Store) Resolve(ref string) (string, error) {
	tags, err := s.readTags()
	if err != nil {
		return "", err
	}
	if id, ok := tags[NormalizeRef(ref)]; ok {
		return id, nil
	}
	if len(ref) >= 6 && !strings.ContainsAny(ref, ":/") {
		layers, err := s.Layers()
		if err != nil {
			return "", err
		}
		var match []string
		for _, layer := range layers {
			if strings.HasPrefix(layer.ID, ref) {
				match = append(match, layer.ID)
			}
		}
		switch len(match) {
		case 1:
			return match[0], nil
		case 0:
		default:
			return "", fmt.Errorf("image: %q matches %d layers", ref, len(match))
		}
	}
	return "", fmt.Errorf("image %q: %w", ref, ErrNotFound)
}

// Images lists every tag.
func (s *Store) Images() ([]Image, error) {
	tags, err := s.readTags()
	if err != nil {
		return nil, err
	}
	images := make([]Image, 0, len(tags))
	for ref, id := range tags {
		layer, err := s.Layer(id)
		if err != nil {
			layer = Layer{ID: id}
		}
		images = append(images, Image{Ref: ref, Layer: layer})
	}
	sort.Slice(images, func(i, j int) bool { return images[i].Ref < images[j].Ref })
	return images, nil
}

// Remove untags ref, then deletes every layer that the removal left unused —
// the layer itself, then its parents in turn — as `docker rmi` does. inUse
// reports layers something outside the store (an instance) still needs.
func (s *Store) Remove(ctx context.Context, ref string, driver machine.Driver, inUse func(id string) bool) ([]string, error) {
	var id string
	err := s.updateTags(func(tags map[string]string) error {
		normalized := NormalizeRef(ref)
		var ok bool
		if id, ok = tags[normalized]; !ok {
			return fmt.Errorf("image %q: %w", ref, ErrNotFound)
		}
		delete(tags, normalized)
		return nil
	})
	if err != nil {
		return nil, err
	}
	var removed []string
	for id != "" {
		used, err := s.used(id, inUse)
		if err != nil || used {
			return removed, err
		}
		layer, err := s.Layer(id)
		if err != nil {
			return removed, err
		}
		if layer.Driver == driver.Name() {
			if err := driver.DeleteLayer(ctx, s.MachineLayer(id)); err != nil {
				return removed, err
			}
		}
		if err := os.RemoveAll(s.layerDir(id)); err != nil {
			return removed, err
		}
		removed = append(removed, id)
		id = layer.Parent
	}
	return removed, nil
}

// used reports whether a layer is tagged, a parent of another layer, or in use
// outside the store.
func (s *Store) used(id string, inUse func(string) bool) (bool, error) {
	if inUse != nil && inUse(id) {
		return true, nil
	}
	tags, err := s.readTags()
	if err != nil {
		return false, err
	}
	for _, tagged := range tags {
		if tagged == id {
			return true, nil
		}
	}
	layers, err := s.Layers()
	if err != nil {
		return false, err
	}
	return slices.ContainsFunc(layers, func(l Layer) bool { return l.Parent == id }), nil
}

func short(id string) string {
	if len(id) > 12 {
		return id[:12]
	}
	return id
}

// Short is the 12-character form of a layer ID people read.
func Short(id string) string { return short(id) }
