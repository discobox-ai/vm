package engine

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strings"

	"github.com/discobox-ai/vm/pkg/machine"
)

// ErrImageNotFound reports a reference that names no tag and no layer.
var ErrImageNotFound = errors.New("not found")

// Image is a tag and the layer it names, or an untagged layer.
type Image struct {
	// Ref is the tag, or empty for an untagged image, named by its layer ID.
	Ref   string        `json:"ref"`
	Layer machine.Layer `json:"layer"`
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

// ValidateRef checks a name:tag. The name is components of lowercase letters
// and digits, separated within a component by one '.', one or two '_', or
// any number of '-', and joined by '/'; the tag is letters, digits, '_', '.',
// and '-', starting with a letter, a digit, or '_'. That is Docker's grammar,
// so that a driver can keep a tag as an image's.
func ValidateRef(ref string) error {
	name, tag, _ := strings.Cut(NormalizeRef(ref), ":")
	if name == "" || tag == "" || len(name)+len(tag) > 200 {
		return fmt.Errorf("invalid reference %q", ref)
	}
	for component := range strings.SplitSeq(name, "/") {
		if !validComponent(component) {
			return fmt.Errorf("reference %q: %q is not lowercase letters and digits, separated by '.', '_', '__', or '-'", ref, component)
		}
	}
	if len(tag) > 128 || tag[0] == '.' || tag[0] == '-' {
		return fmt.Errorf("reference %q: tag %q is not letters, digits, '_', '.', and '-', starting with a letter, a digit, or '_'", ref, tag)
	}
	for _, r := range tag {
		if !alnum(r) && (r < 'A' || r > 'Z') && !strings.ContainsRune("_.-", r) {
			return fmt.Errorf("reference %q: tag %q is not letters, digits, '_', '.', and '-'", ref, tag)
		}
	}
	return nil
}

func alnum(r rune) bool { return r >= 'a' && r <= 'z' || r >= '0' && r <= '9' }

// validComponent is one '/'-separated part of a name in Docker's grammar.
func validComponent(c string) bool {
	if c == "" || !alnum(rune(c[0])) || !alnum(rune(c[len(c)-1])) {
		return false
	}
	for i := 0; i < len(c); {
		if alnum(rune(c[i])) {
			i++
			continue
		}
		j := i
		for j < len(c) && !alnum(rune(c[j])) {
			j++
		}
		sep := c[i:j]
		if sep != "." && sep != "_" && sep != "__" && strings.Trim(sep, "-") != "" {
			return false
		}
		i = j
	}
	return true
}

// Short is the 12-character form of a layer ID people read.
func Short(id string) string {
	if len(id) > 12 {
		return id[:12]
	}
	return id
}

// Resolve is the layer a reference names: name:tag, a bare name (meaning
// :latest), a full layer ID, or an unambiguous layer ID prefix of at least six
// characters.
func (e *Engine) Resolve(ctx context.Context, ref string) (machine.Layer, error) {
	tags, err := e.Driver.Tags(ctx)
	if err != nil {
		return machine.Layer{}, err
	}
	normalized := NormalizeRef(ref)
	for _, t := range tags {
		if t.Ref == normalized {
			layer, err := e.Driver.Layer(ctx, t.Layer)
			if errors.Is(err, machine.ErrNotFound) {
				return machine.Layer{}, fmt.Errorf("image %s names layer %s, which %s no longer has: %w", ref, Short(t.Layer), e.Driver.Name(), ErrImageNotFound)
			}
			return layer, err
		}
	}
	if len(ref) < 6 || strings.ContainsAny(ref, ":/") {
		return machine.Layer{}, fmt.Errorf("image %q: %w", ref, ErrImageNotFound)
	}
	if layer, err := e.Driver.Layer(ctx, ref); err == nil || !errors.Is(err, machine.ErrNotFound) {
		return layer, err
	}
	layers, err := e.Driver.Layers(ctx)
	if err != nil {
		return machine.Layer{}, err
	}
	var match []machine.Layer
	for _, layer := range layers {
		if strings.HasPrefix(layer.ID, ref) {
			match = append(match, layer)
		}
	}
	switch len(match) {
	case 1:
		return match[0], nil
	case 0:
		return machine.Layer{}, fmt.Errorf("image %q: %w", ref, ErrImageNotFound)
	default:
		return machine.Layer{}, fmt.Errorf("image %q matches %d layers", ref, len(match))
	}
}

// Tag points ref at a layer, moving it from any other.
func (e *Engine) Tag(ctx context.Context, ref, layer string) error {
	if err := ValidateRef(ref); err != nil {
		return err
	}
	return e.Driver.Tag(ctx, NormalizeRef(ref), layer)
}

// Images lists every tag, then every untagged image: a layer that has no tag
// and is no other layer's parent, which is what a build without -t leaves. all
// lists every untagged layer, the intermediate layers of a chain too, as
// `docker images -a` does.
func (e *Engine) Images(ctx context.Context, all bool) ([]Image, error) {
	tags, err := e.Driver.Tags(ctx)
	if err != nil {
		return nil, err
	}
	layers, err := e.Driver.Layers(ctx)
	if err != nil {
		return nil, err
	}
	byID := make(map[string]machine.Layer, len(layers))
	named := map[string]bool{}
	for _, layer := range layers {
		byID[layer.ID] = layer
		if !all {
			named[layer.Parent] = true
		}
	}
	images := make([]Image, 0, len(tags))
	for _, t := range tags {
		layer, ok := byID[t.Layer]
		if !ok {
			layer = machine.Layer{ID: t.Layer}
		}
		named[t.Layer] = true
		images = append(images, Image{Ref: t.Ref, Layer: layer})
	}
	slices.SortFunc(images, func(a, b Image) int { return strings.Compare(a.Ref, b.Ref) })
	var untagged []Image
	for _, layer := range layers {
		if !named[layer.ID] {
			untagged = append(untagged, Image{Layer: layer})
		}
	}
	slices.SortFunc(untagged, func(a, b Image) int { return b.Layer.Created.Compare(a.Layer.Created) })
	return append(images, untagged...), nil
}

// RemoveImage untags ref, then deletes every layer that the removal left
// unused — the layer itself, then its parents in turn — as `docker rmi` does.
// A ref that is a layer ID removes an untagged image; a tagged one is removed
// by name. A layer is used while a tag names it, a layer is built on it, or
// an instance was made from it. untagged is the tag that was removed, or
// empty for a layer ID.
func (e *Engine) RemoveImage(ctx context.Context, ref string) (untagged string, removed []string, err error) {
	tags, err := e.Driver.Tags(ctx)
	if err != nil {
		return "", nil, err
	}
	normalized := NormalizeRef(ref)
	var id string
	for _, t := range tags {
		if t.Ref == normalized {
			id, untagged = t.Layer, normalized
		}
	}
	if untagged != "" {
		if err := e.Driver.Untag(ctx, normalized); err != nil {
			return "", nil, err
		}
	} else {
		layer, err := e.Resolve(ctx, ref)
		if err != nil {
			return "", nil, err
		}
		id = layer.ID
		used, err := e.layerUsed(ctx, id)
		if err != nil {
			return "", nil, err
		}
		if used {
			return "", nil, fmt.Errorf("image %s is tagged, has layers built on it, or is in use; remove what names it first", Short(id))
		}
	}
	// The listing, not Layer, says what each layer's parent is: a layer that
	// cannot boot (a local one whose parent is gone) is still listed, and is
	// deleted like any other.
	layers, err := e.Driver.Layers(ctx)
	if err != nil {
		return untagged, removed, err
	}
	parents := map[string]string{}
	for _, layer := range layers {
		parents[layer.ID] = layer.Parent
	}
	for id != "" {
		used, err := e.layerUsed(ctx, id)
		if err != nil || used {
			return untagged, removed, err
		}
		parent, listed := parents[id]
		if !listed {
			return untagged, removed, nil
		}
		if err := e.Driver.DeleteLayer(ctx, id); err != nil {
			return untagged, removed, err
		}
		removed = append(removed, id)
		id = parent
	}
	return untagged, removed, nil
}

// layerUsed reports whether a tag names a layer, a layer is built on it, or
// an instance, a build's too, was made from it. A listing that fails is no
// answer, and never read as "unused".
func (e *Engine) layerUsed(ctx context.Context, id string) (bool, error) {
	tags, err := e.Driver.Tags(ctx)
	if err != nil {
		return true, err
	}
	for _, t := range tags {
		if t.Layer == id {
			return true, nil
		}
	}
	layers, err := e.Driver.Layers(ctx)
	if err != nil {
		return true, err
	}
	for _, layer := range layers {
		if layer.Parent == id {
			return true, nil
		}
	}
	instances, err := e.Driver.Instances(ctx)
	if err != nil {
		return true, err
	}
	for _, inst := range instances {
		if inst.Layer == id {
			return true, nil
		}
	}
	return false, nil
}
