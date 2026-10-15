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
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/discobox-ai/x/gormdb"
	"gorm.io/gorm"
	"gorm.io/gorm/clause"

	"github.com/discobox-ai/vm/internal/fsutil"
	"github.com/discobox-ai/vm/pkg/machine"
)

// ErrNotFound reports an unknown reference or layer.
var ErrNotFound = errors.New("image: not found")

// Layer is one committed layer's metadata.
type Layer struct {
	ID      string     `json:"id" gorm:"primaryKey"`
	Parent  string     `json:"parent,omitempty" gorm:"index"`
	Driver  string     `json:"driver"`
	GuestOS machine.OS `json:"guestOS" gorm:"column:guest_os"`
	Created time.Time  `json:"created"`
	// Comment says what made the layer: "install windows", "build base:toolchains".
	Comment string `json:"comment,omitempty"`
}

// TableName is the layers' table. Every disco-vm table is named disco_vm_*,
// so they can share a database with the tables of a program that embeds the
// engine.
func (Layer) TableName() string { return "disco_vm_layers" }

// tag is one name:tag and the layer it names.
type tag struct {
	Ref     string `gorm:"primaryKey"`
	LayerID string `gorm:"index"`
}

func (tag) TableName() string { return "disco_vm_tags" }

// replacement records the layer built in place of one its driver lost.
type replacement struct {
	ID          string `gorm:"primaryKey"`
	Replacement string
}

func (replacement) TableName() string { return "disco_vm_replaced_layers" }

// Image is a tag and the layer it names.
type Image struct {
	Ref   string `json:"ref"`
	Layer Layer  `json:"layer"`
}

// Store is an image store. Its metadata (layers, tags, and which layer
// replaced which) is rows in a database, and each layer's disk is the
// driver's files under a directory:
//
//	layers/<id>/payload/     the driver's files (machine.Layer.Dir)
type Store struct {
	Root string
	db   *gormdb.Pools
}

// Open creates the store's directories and tables if needed. The store does
// not close db.
func Open(root string, db *gormdb.Pools) (*Store, error) {
	if err := os.MkdirAll(filepath.Join(root, "layers"), 0o755); err != nil {
		return nil, err
	}
	s := &Store{Root: root, db: db}
	// In a transaction, so two commands starting on a new root do not both
	// create the tables.
	err := db.Write.Transaction(func(tx *gorm.DB) error {
		return tx.AutoMigrate(&Layer{}, &tag{}, &replacement{})
	})
	if err != nil {
		return nil, fmt.Errorf("image: migrate: %w", err)
	}
	if err := s.importFiles(); err != nil {
		return nil, err
	}
	return s, nil
}

func (s *Store) layerDir(id string) string { return filepath.Join(s.Root, "layers", id) }

// importFiles moves the metadata a disco-vm from before the database kept in
// JSON files (layers/<id>/layer.json, tags.json, replaced.json) into it. A
// file is removed once its rows are in, so each is imported once, and a row
// that is already there wins.
func (s *Store) importFiles() error {
	entries, err := os.ReadDir(filepath.Join(s.Root, "layers"))
	if err != nil {
		return err
	}
	insert := func(row any) error {
		return s.db.Write.Clauses(clause.OnConflict{DoNothing: true}).Create(row).Error
	}
	for _, entry := range entries {
		if !entry.IsDir() || strings.HasPrefix(entry.Name(), ".") {
			continue
		}
		path := filepath.Join(s.layerDir(entry.Name()), "layer.json")
		var layer Layer
		if err := fsutil.ReadJSON(path, &layer); err != nil || layer.ID != entry.Name() {
			// No file is a layer that is already a row. One that does not
			// parse was never listed either, and stays for someone to read.
			continue
		}
		if err := insert(&layer); err != nil {
			return fmt.Errorf("image: import %s: %w", path, err)
		}
		if err := os.Remove(path); err != nil {
			return err
		}
	}
	tagsPath := filepath.Join(s.Root, "tags.json")
	tags := map[string]string{}
	switch err := fsutil.ReadJSON(tagsPath, &tags); {
	case err == nil:
		for ref, id := range tags {
			if err := insert(&tag{Ref: ref, LayerID: id}); err != nil {
				return fmt.Errorf("image: import %s: %w", tagsPath, err)
			}
		}
		if err := os.Remove(tagsPath); err != nil {
			return err
		}
	case !errors.Is(err, os.ErrNotExist):
		return err
	}
	replacedPath := filepath.Join(s.Root, "replaced.json")
	replaced := map[string]string{}
	switch err := fsutil.ReadJSON(replacedPath, &replaced); {
	case err == nil:
		for id, to := range replaced {
			if err := insert(&replacement{ID: id, Replacement: to}); err != nil {
				return fmt.Errorf("image: import %s: %w", replacedPath, err)
			}
		}
		if err := os.Remove(replacedPath); err != nil {
			return err
		}
	case !errors.Is(err, os.ErrNotExist):
		return err
	}
	// The lock that guarded those files.
	_ = os.Remove(filepath.Join(s.Root, "tags.lock"))
	return nil
}

// MachineLayer is the driver's view of a committed layer.
func (s *Store) MachineLayer(id string) machine.Layer {
	return machine.Layer{ID: id, Dir: filepath.Join(s.layerDir(id), "payload")}
}

// Layer reads a layer's metadata.
func (s *Store) Layer(id string) (Layer, error) {
	var layers []Layer
	if err := s.db.Read.Where("id = ?", id).Limit(1).Find(&layers).Error; err != nil {
		return Layer{}, err
	}
	if len(layers) == 0 {
		return Layer{}, fmt.Errorf("layer %s: %w", short(id), ErrNotFound)
	}
	return layers[0], nil
}

// Has reports whether a layer is committed.
func (s *Store) Has(id string) bool {
	_, err := s.Layer(id)
	return err == nil
}

// Intact reports whether a layer is committed and its driver still holds what
// it keeps for the layer outside the store. Another driver's layer is taken
// at its word.
func (s *Store) Intact(ctx context.Context, id string, driver machine.Driver) (bool, error) {
	layer, err := s.Layer(id)
	if errors.Is(err, ErrNotFound) {
		return false, nil
	}
	if err != nil || layer.Driver != driver.Name() {
		return err == nil, err
	}
	return driver.LayerExists(ctx, s.MachineLayer(id))
}

// Current is the layer a build should use where its inputs say id: id itself,
// or the layer that was built in its place after id's driver lost it.
func (s *Store) Current(id string) (string, error) {
	var replaced []replacement
	if err := s.db.Read.Where("id = ?", id).Limit(1).Find(&replaced).Error; err != nil {
		return "", err
	}
	if len(replaced) == 1 && s.Has(replaced[0].Replacement) {
		return replaced[0].Replacement, nil
	}
	return id, nil
}

// Replace records that to was built in place of id, whose driver lost it. The
// lost layer stays as it is: the layers built on it hold disks made from the
// one that is gone, not from its replacement, so they are still its children
// and not the replacement's. Replacements that have since been removed are
// forgotten.
func (s *Store) Replace(id, to string) error {
	return s.db.Write.Transaction(func(tx *gorm.DB) error {
		row := replacement{ID: id, Replacement: to}
		if err := tx.Clauses(clause.OnConflict{UpdateAll: true}).Create(&row).Error; err != nil {
			return err
		}
		return tx.Where("replacement NOT IN (?)", tx.Model(&Layer{}).Select("id")).Delete(&replacement{}).Error
	})
}

// Layers lists every committed layer.
func (s *Store) Layers() ([]Layer, error) {
	var layers []Layer
	if err := s.db.Read.Order("id").Find(&layers).Error; err != nil {
		return nil, err
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
// renames it into place and records its row in one transaction, so a crash
// leaves at most a stray directory and never a half-written layer under a
// committed ID.
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

// Commit publishes the layer. driver is the one that wrote it, which releases
// what it holds for this copy if another build published the same layer first.
func (p *Pending) Commit(ctx context.Context, driver machine.Driver) (Layer, error) {
	if p.meta.Created.IsZero() {
		p.meta.Created = time.Now().UTC()
	}
	published := false
	err := p.store.db.Write.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		// The row goes in first: it is what serializes two builds committing
		// the same layer, and the second finds the first's row.
		res := tx.Clauses(clause.OnConflict{DoNothing: true}).Create(&p.meta)
		if res.Error != nil || res.RowsAffected == 0 {
			return res.Error
		}
		dir := p.store.layerDir(p.meta.ID)
		// No row named this directory until now, so one that is there is what
		// a crash left between the rename and the commit.
		if err := os.RemoveAll(dir); err != nil {
			return err
		}
		if err := os.Rename(p.tmp, dir); err != nil {
			return err
		}
		published = true
		return nil
	})
	if err != nil {
		return Layer{}, err
	}
	if !published {
		// Another build committed the same inputs first; its layer is
		// identical by construction. This copy may still hold something
		// outside its directory, such as a boxd snapshot.
		p.Abort(ctx, driver)
		return p.store.Layer(p.meta.ID)
	}
	return p.meta, nil
}

// Abort discards the layer, and has driver release anything it holds for the
// layer beyond its directory, as removing a published layer does.
func (p *Pending) Abort(ctx context.Context, driver machine.Driver) {
	if driver != nil {
		_ = driver.DeleteLayer(ctx, p.MachineLayer())
	}
	_ = os.RemoveAll(p.tmp)
}

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
		if (r < 'a' || r > 'z') && (r < '0' || r > '9') && !strings.ContainsRune("._-/", r) {
			return fmt.Errorf("image: reference %q may use only a-z, 0-9, and . _ - /", ref)
		}
	}
	return nil
}

// tagged is the layer a normalized ref names, or empty when it names none.
func tagged(db *gorm.DB, normalized string) (string, error) {
	var tags []tag
	if err := db.Where("ref = ?", normalized).Limit(1).Find(&tags).Error; err != nil || len(tags) == 0 {
		return "", err
	}
	return tags[0].LayerID, nil
}

// Tag points ref at a layer, and has the layer's driver record the tag where
// it keeps the layer. A ref that named another layer is moved.
func (s *Store) Tag(ctx context.Context, ref, id string, driver machine.Driver) error {
	if err := ValidateRef(ref); err != nil {
		return err
	}
	layer, err := s.Layer(id)
	if err != nil {
		return err
	}
	normalized := NormalizeRef(ref)
	if layer.Driver == driver.Name() {
		if err := driver.TagLayer(ctx, s.MachineLayer(id), normalized); err != nil {
			return err
		}
	}
	var old string
	err = s.db.Write.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		var err error
		if old, err = tagged(tx, normalized); err != nil {
			return err
		}
		return tx.Clauses(clause.OnConflict{UpdateAll: true}).Create(&tag{Ref: normalized, LayerID: id}).Error
	})
	if err != nil || old == "" || old == id {
		return err
	}
	return s.untagLayer(ctx, old, normalized, driver)
}

// untagLayer has a layer's driver drop a tag it recorded.
func (s *Store) untagLayer(ctx context.Context, id, normalized string, driver machine.Driver) error {
	if layer, err := s.Layer(id); err != nil || layer.Driver != driver.Name() {
		return nil //nolint:nilerr // a layer that is gone, or another driver's, has nothing of this driver's to drop
	}
	return driver.UntagLayer(ctx, s.MachineLayer(id), normalized)
}

// Resolve turns a reference into a layer ID. It accepts name:tag, a bare name
// (meaning :latest), a full layer ID, or an unambiguous layer ID prefix of at
// least six characters.
func (s *Store) Resolve(ref string) (string, error) {
	if id, err := tagged(s.db.Read, NormalizeRef(ref)); err != nil || id != "" {
		return id, err
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

// Images lists every tag, then every untagged image: a layer that has no tag
// and is no other layer's parent, which is what a build without -t leaves. An
// untagged image has an empty Ref and is named by its layer ID.
func (s *Store) Images() ([]Image, error) {
	var tags []tag
	if err := s.db.Read.Find(&tags).Error; err != nil {
		return nil, err
	}
	layers, err := s.Layers()
	if err != nil {
		return nil, err
	}
	byID := make(map[string]Layer, len(layers))
	named := map[string]bool{}
	for _, layer := range layers {
		byID[layer.ID] = layer
		named[layer.Parent] = true
	}
	images := make([]Image, 0, len(tags))
	for _, t := range tags {
		layer, ok := byID[t.LayerID]
		if !ok {
			layer = Layer{ID: t.LayerID}
		}
		named[t.LayerID] = true
		images = append(images, Image{Ref: t.Ref, Layer: layer})
	}
	sort.Slice(images, func(i, j int) bool { return images[i].Ref < images[j].Ref })
	var untagged []Image
	for _, layer := range layers {
		if !named[layer.ID] {
			untagged = append(untagged, Image{Layer: layer})
		}
	}
	sort.Slice(untagged, func(i, j int) bool { return untagged[i].Layer.Created.After(untagged[j].Layer.Created) })
	return append(images, untagged...), nil
}

// Remove untags ref, then deletes every layer that the removal left unused —
// the layer itself, then its parents in turn — as `docker rmi` does. A ref
// that is a layer ID removes an untagged image; a tagged one is removed by
// name. inUse reports layers something outside the store (an instance) still
// needs. untagged is the tag that was removed, or empty for a layer ID.
func (s *Store) Remove(ctx context.Context, ref string, driver machine.Driver, inUse func(id string) bool) (untagged string, removed []string, err error) {
	// The driver's record of the tag goes first: if that fails the tag is
	// still here, and removing it again tries again.
	normalized := NormalizeRef(ref)
	id, err := tagged(s.db.Read, normalized)
	if err != nil {
		return "", nil, err
	}
	if id != "" {
		if err := s.untagLayer(ctx, id, normalized, driver); err != nil {
			return "", nil, err
		}
	}
	err = s.db.Write.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		// Read again: the tag may have moved or gone since the driver was told.
		var err error
		if id, err = tagged(tx, normalized); err != nil || id == "" {
			return err
		}
		untagged = normalized
		return tx.Delete(&tag{Ref: normalized}).Error
	})
	if err != nil {
		return "", nil, err
	}
	if untagged == "" {
		if id, err = s.Resolve(ref); err != nil {
			return "", nil, err
		}
		used, err := s.used(id, inUse)
		if err != nil {
			return "", nil, err
		}
		if used {
			return "", nil, fmt.Errorf("image %s is tagged, has layers built on it, or is in use; remove what names it first", short(id))
		}
	}
	for id != "" {
		used, err := s.used(id, inUse)
		if err != nil || used {
			return untagged, removed, err
		}
		layer, err := s.Layer(id)
		if err != nil {
			return untagged, removed, err
		}
		if layer.Driver == driver.Name() {
			if err := driver.DeleteLayer(ctx, s.MachineLayer(id)); err != nil {
				return untagged, removed, err
			}
		}
		// The row goes first: a directory with no row is a stray that the next
		// commit of this layer clears, and a row with no directory would be a
		// layer that cannot boot.
		if err := s.db.Write.WithContext(ctx).Delete(&Layer{ID: id}).Error; err != nil {
			return untagged, removed, err
		}
		if err := os.RemoveAll(s.layerDir(id)); err != nil {
			return untagged, removed, err
		}
		removed = append(removed, id)
		id = layer.Parent
	}
	return untagged, removed, nil
}

// used reports whether a layer is tagged, a parent of another layer, or in use
// outside the store.
func (s *Store) used(id string, inUse func(string) bool) (bool, error) {
	if inUse != nil && inUse(id) {
		return true, nil
	}
	var tags, children int64
	if err := s.db.Read.Model(&tag{}).Where("layer_id = ?", id).Count(&tags).Error; err != nil {
		return false, err
	}
	if err := s.db.Read.Model(&Layer{}).Where("parent = ?", id).Count(&children).Error; err != nil {
		return false, err
	}
	return tags > 0 || children > 0, nil
}

func short(id string) string {
	if len(id) > 12 {
		return id[:12]
	}
	return id
}

// Short is the 12-character form of a layer ID people read.
func Short(id string) string { return short(id) }
