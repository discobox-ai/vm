package engine

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strings"

	"github.com/discobox-ai/vm/pkg/machine"
)

// ErrImageNotFound reports a reference that names no tag and no image.
var ErrImageNotFound = errors.New("not found")

// Entry is a tag and the image it names, or an untagged image.
type Entry struct {
	// Ref is the tag, or empty for an untagged image, named by its ID.
	Ref   string        `json:"ref"`
	Image machine.Image `json:"image"`
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

// Short is the 12-character form of an image ID people read.
func Short(id string) string {
	if len(id) > 12 {
		return id[:12]
	}
	return id
}

// Resolve is the image a reference names: name:tag, a bare name (meaning
// :latest), a full image ID, or an unambiguous image ID prefix of at least six
// characters.
func (e *Engine) Resolve(ctx context.Context, ref string) (machine.Image, error) {
	tags, err := e.Driver.Tags(ctx)
	if err != nil {
		return machine.Image{}, err
	}
	normalized := NormalizeRef(ref)
	for _, t := range tags {
		if t.Ref == normalized {
			img, err := e.Driver.Image(ctx, t.Image)
			if errors.Is(err, machine.ErrNotFound) {
				return machine.Image{}, fmt.Errorf("image %s names %s, which %s no longer has: %w", ref, Short(t.Image), e.Driver.Name(), ErrImageNotFound)
			}
			return img, err
		}
	}
	if len(ref) < 6 || strings.ContainsAny(ref, ":/") {
		return machine.Image{}, fmt.Errorf("image %q: %w", ref, ErrImageNotFound)
	}
	if img, err := e.Driver.Image(ctx, ref); err == nil || !errors.Is(err, machine.ErrNotFound) {
		return img, err
	}
	images, err := e.Driver.Images(ctx)
	if err != nil {
		return machine.Image{}, err
	}
	var match []machine.Image
	for _, img := range images {
		if strings.HasPrefix(img.ID, ref) {
			match = append(match, img)
		}
	}
	switch len(match) {
	case 1:
		return match[0], nil
	case 0:
		return machine.Image{}, fmt.Errorf("image %q: %w", ref, ErrImageNotFound)
	default:
		return machine.Image{}, fmt.Errorf("image %q matches %d images", ref, len(match))
	}
}

// Tag points ref at an image, moving it from any other.
func (e *Engine) Tag(ctx context.Context, ref, image string) error {
	if err := ValidateRef(ref); err != nil {
		return err
	}
	return e.Driver.Tag(ctx, NormalizeRef(ref), image)
}

// Images lists every tag, then every untagged image, newest first.
func (e *Engine) Images(ctx context.Context) ([]Entry, error) {
	tags, err := e.Driver.Tags(ctx)
	if err != nil {
		return nil, err
	}
	images, err := e.Driver.Images(ctx)
	if err != nil {
		return nil, err
	}
	byID := make(map[string]machine.Image, len(images))
	for _, img := range images {
		byID[img.ID] = img
	}
	named := map[string]bool{}
	entries := make([]Entry, 0, len(tags))
	for _, t := range tags {
		img, ok := byID[t.Image]
		if !ok {
			img = machine.Image{ID: t.Image}
		}
		named[t.Image] = true
		entries = append(entries, Entry{Ref: t.Ref, Image: img})
	}
	slices.SortFunc(entries, func(a, b Entry) int { return strings.Compare(a.Ref, b.Ref) })
	var untagged []Entry
	for _, img := range images {
		if !named[img.ID] {
			untagged = append(untagged, Entry{Image: img})
		}
	}
	slices.SortFunc(untagged, func(a, b Entry) int { return b.Image.Created.Compare(a.Image.Created) })
	return append(entries, untagged...), nil
}

// Removal is what RemoveImage did.
type Removal struct {
	// Untagged is the tag removed, or empty for an image named by its ID.
	Untagged string
	// Deleted is the image deleted, or empty when it is still tagged, in
	// use, or Kept.
	Deleted string
	// Kept is why the driver kept an image whose last tag was removed: its
	// disk is still another image's (machine.ErrInUse). The image stays,
	// untagged, until that one is removed.
	Kept error
}

// RemoveImage untags ref, then deletes the image if nothing else tags or
// runs it, as `docker rmi` does, and touches no other image. A ref that is an
// image ID removes an untagged image; a tagged one is removed by name.
func (e *Engine) RemoveImage(ctx context.Context, ref string) (Removal, error) {
	var r Removal
	tags, err := e.Driver.Tags(ctx)
	if err != nil {
		return r, err
	}
	normalized := NormalizeRef(ref)
	var id string
	for _, t := range tags {
		if t.Ref == normalized {
			id, r.Untagged = t.Image, normalized
		}
	}
	if r.Untagged != "" {
		if err := e.Driver.Untag(ctx, normalized); err != nil {
			return Removal{}, err
		}
	} else {
		img, err := e.Resolve(ctx, ref)
		if err != nil {
			return r, err
		}
		id = img.ID
	}
	used, err := e.imageUsed(ctx, id)
	if err != nil {
		return r, err
	}
	if used {
		if r.Untagged != "" {
			return r, nil
		}
		return r, fmt.Errorf("image %s is tagged or in use; remove what names it first", Short(id))
	}
	err = e.Driver.DeleteImage(ctx, id)
	switch {
	case errors.Is(err, machine.ErrInUse) && r.Untagged != "":
		r.Kept = err
		return r, nil
	case err != nil:
		return r, err
	}
	r.Deleted = id
	return r, nil
}

// imageUsed reports whether a tag names an image, or an instance was made
// from it. A listing that fails is no answer, and never read as "unused".
func (e *Engine) imageUsed(ctx context.Context, id string) (bool, error) {
	tags, err := e.Driver.Tags(ctx)
	if err != nil {
		return true, err
	}
	for _, t := range tags {
		if t.Image == id {
			return true, nil
		}
	}
	instances, err := e.Driver.Instances(ctx)
	if err != nil {
		return true, err
	}
	for _, inst := range instances {
		if inst.ImageID == id {
			return true, nil
		}
	}
	return false, nil
}
