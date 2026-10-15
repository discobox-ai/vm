package image

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/discobox-ai/vm/internal/statedb"
	"github.com/discobox-ai/vm/pkg/machine"
	"github.com/discobox-ai/vm/pkg/machine/fake"
)

// openStore opens a store at root with a database of its own beside it.
func openStore(t *testing.T, root string) *Store {
	t.Helper()
	db, err := statedb.Open(root)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })
	s, err := Open(root, db)
	if err != nil {
		t.Fatal(err)
	}
	return s
}

func commit(t *testing.T, s *Store, id, parent string) {
	t.Helper()
	p, err := s.Begin(Layer{ID: id, Parent: parent, Driver: "fake", GuestOS: machine.Linux})
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(p.MachineLayer().Dir+"/data", []byte(id), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := p.Commit(context.Background(), nil); err != nil {
		t.Fatal(err)
	}
}

func TestStoreTagsChainsAndRemove(t *testing.T) {
	s := openStore(t, t.TempDir())
	commit(t, s, "aaaaaaaaaaaa1", "")
	commit(t, s, "bbbbbbbbbbbb2", "aaaaaaaaaaaa1")
	commit(t, s, "cccccccccccc3", "bbbbbbbbbbbb2")
	driver, _ := fake.New()
	ctx := context.Background()
	if err := s.Tag(ctx, "base", "aaaaaaaaaaaa1", driver); err != nil {
		t.Fatal(err)
	}
	if err := s.Tag(ctx, "app:v1", "cccccccccccc3", driver); err != nil {
		t.Fatal(err)
	}

	if id, err := s.Resolve("base"); err != nil || id != "aaaaaaaaaaaa1" {
		t.Fatalf("resolve base = %q, %v", id, err)
	}
	if id, err := s.Resolve("bbbbbb"); err != nil || id != "bbbbbbbbbbbb2" {
		t.Fatalf("resolve prefix = %q, %v", id, err)
	}
	if _, err := s.Resolve("nope"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("resolve unknown: %v", err)
	}
	chain, err := s.Chain("cccccccccccc3")
	if err != nil || len(chain) != 3 || chain[0].ID != "aaaaaaaaaaaa1" {
		t.Fatalf("chain = %+v, %v", chain, err)
	}

	// Removing app deletes its layer and the untagged middle one, and stops at
	// the tagged base.
	untagged, removed, err := s.Remove(context.Background(), "app:v1", driver, nil)
	if err != nil {
		t.Fatal(err)
	}
	if untagged != "app:v1" || len(removed) != 2 || !s.Has("aaaaaaaaaaaa1") || s.Has("bbbbbbbbbbbb2") {
		t.Fatalf("untagged %q, removed %v", untagged, removed)
	}
}

func TestUntaggedImages(t *testing.T) {
	s := openStore(t, t.TempDir())
	commit(t, s, "aaaaaaaaaaaa1", "")
	commit(t, s, "bbbbbbbbbbbb2", "aaaaaaaaaaaa1")
	commit(t, s, "cccccccccccc3", "aaaaaaaaaaaa1")
	driver, _ := fake.New()
	_ = s.Tag(context.Background(), "app", "bbbbbbbbbbbb2", driver)

	// The untagged head is an image; the shared parent is not.
	images, err := s.Images()
	if err != nil || len(images) != 2 || images[0].Ref != "app:latest" || images[1].Ref != "" || images[1].Layer.ID != "cccccccccccc3" {
		t.Fatalf("images = %+v, %v", images, err)
	}
	// By ID, only an untagged head goes.
	for _, id := range []string{"aaaaaaaaaaaa1", "bbbbbbbbbbbb2"} {
		if _, _, err := s.Remove(context.Background(), id, driver, nil); err == nil || !s.Has(id) {
			t.Fatalf("remove %s by ID: %v", id, err)
		}
	}
	untagged, removed, err := s.Remove(context.Background(), "cccccc", driver, nil)
	if err != nil || untagged != "" || len(removed) != 1 || s.Has("cccccccccccc3") || !s.Has("aaaaaaaaaaaa1") {
		t.Fatalf("untagged %q, removed %v, %v", untagged, removed, err)
	}
	if _, _, err := s.Remove(context.Background(), "cccccc", driver, nil); !errors.Is(err, ErrNotFound) {
		t.Fatalf("remove twice: %v", err)
	}
}

func TestRemoveKeepsLayersInUse(t *testing.T) {
	s := openStore(t, t.TempDir())
	commit(t, s, "aaaaaaaaaaaa1", "")
	driver, _ := fake.New()
	_ = s.Tag(context.Background(), "base", "aaaaaaaaaaaa1", driver)
	_, removed, err := s.Remove(context.Background(), "base", driver, func(string) bool { return true })
	if err != nil || len(removed) != 0 || !s.Has("aaaaaaaaaaaa1") {
		t.Fatalf("removed %v, %v", removed, err)
	}
}

// tagRecorder is the fake driver, recording what the store asks it to tag.
type tagRecorder struct {
	*fake.Driver
	tags map[string]string
	fail error
	lost map[string]bool
}

func (r *tagRecorder) LayerExists(_ context.Context, layer machine.Layer) (bool, error) {
	return !r.lost[layer.ID], r.fail
}

// TestReplacedLayer checks that a layer its driver no longer holds is not
// intact, stays in the store for the layers built on it, and is answered for
// by the layer recorded in its place for as long as that one exists.
func TestReplacedLayer(t *testing.T) {
	s := openStore(t, t.TempDir())
	commit(t, s, "aaaaaaaaaaaa1", "")
	commit(t, s, "bbbbbbbbbbbb2", "aaaaaaaaaaaa1")
	commit(t, s, "cccccccccccc3", "bbbbbbbbbbbb2")
	base, _ := fake.New()
	driver := &tagRecorder{Driver: base, tags: map[string]string{}, lost: map[string]bool{"bbbbbbbbbbbb2": true}}
	ctx := context.Background()

	for id, want := range map[string]bool{"aaaaaaaaaaaa1": true, "bbbbbbbbbbbb2": false, "cccccccccccc3": true, "dddddddddddd4": false} {
		if intact, err := s.Intact(ctx, id, driver); err != nil || intact != want {
			t.Fatalf("intact %s = %v, %v", id, intact, err)
		}
	}
	// Nothing is dropped on the driver's word: the child's chain is whole.
	if chain, err := s.Chain("cccccccccccc3"); err != nil || len(chain) != 3 {
		t.Fatalf("chain = %+v, %v", chain, err)
	}
	current := func(id, want string) {
		t.Helper()
		if got, err := s.Current(id); err != nil || got != want {
			t.Fatalf("current %s = %q, %v, want %s", id, got, err, want)
		}
	}
	current("bbbbbbbbbbbb2", "bbbbbbbbbbbb2")
	commit(t, s, "eeeeeeeeeeee5", "aaaaaaaaaaaa1")
	if err := s.Replace("bbbbbbbbbbbb2", "eeeeeeeeeeee5"); err != nil {
		t.Fatal(err)
	}
	current("bbbbbbbbbbbb2", "eeeeeeeeeeee5")
	current("cccccccccccc3", "cccccccccccc3")
	// A replacement that is removed is no longer the answer.
	if _, _, err := s.Remove(ctx, "eeeeeeeeeeee5", driver, nil); err != nil {
		t.Fatal(err)
	}
	current("bbbbbbbbbbbb2", "bbbbbbbbbbbb2")
	// A driver that cannot answer is an error, not a lost layer.
	driver.fail = errors.New("unreachable")
	if _, err := s.Intact(ctx, "aaaaaaaaaaaa1", driver); err == nil {
		t.Fatal("intact with an unreachable driver")
	}
}

func (r *tagRecorder) TagLayer(_ context.Context, layer machine.Layer, ref string) error {
	if r.fail != nil {
		return r.fail
	}
	r.tags[layer.ID+" "+ref] = layer.Dir
	return nil
}

func (r *tagRecorder) UntagLayer(_ context.Context, layer machine.Layer, ref string) error {
	if r.fail != nil {
		return r.fail
	}
	delete(r.tags, layer.ID+" "+ref)
	return nil
}

func TestTagsReachTheDriver(t *testing.T) {
	s := openStore(t, t.TempDir())
	commit(t, s, "aaaaaaaaaaaa1", "")
	commit(t, s, "bbbbbbbbbbbb2", "")
	base, _ := fake.New()
	driver := &tagRecorder{Driver: base, tags: map[string]string{}}
	ctx := context.Background()
	has := func(keys ...string) {
		t.Helper()
		if len(driver.tags) != len(keys) {
			t.Fatalf("driver holds %v, want %v", driver.tags, keys)
		}
		for _, key := range keys {
			if dir, ok := driver.tags[key]; !ok || dir == "" {
				t.Fatalf("driver holds %v, want %v", driver.tags, keys)
			}
		}
	}

	if err := s.Tag(ctx, "app", "aaaaaaaaaaaa1", driver); err != nil {
		t.Fatal(err)
	}
	has("aaaaaaaaaaaa1 app:latest")
	// A tag that moves is dropped from the layer it left.
	if err := s.Tag(ctx, "app", "bbbbbbbbbbbb2", driver); err != nil {
		t.Fatal(err)
	}
	has("bbbbbbbbbbbb2 app:latest")

	// A tag the driver could not record is not made, and one it could not
	// drop stays, to be removed again.
	driver.fail = errors.New("unreachable")
	if err := s.Tag(ctx, "other", "aaaaaaaaaaaa1", driver); err == nil {
		t.Fatal("tagged without the driver")
	}
	if _, err := s.Resolve("other"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("resolve other: %v", err)
	}
	if _, _, err := s.Remove(ctx, "app", driver, nil); err == nil {
		t.Fatal("untagged without the driver")
	}
	if id, err := s.Resolve("app"); err != nil || id != "bbbbbbbbbbbb2" {
		t.Fatalf("resolve app = %q, %v", id, err)
	}
	driver.fail = nil
	if _, _, err := s.Remove(ctx, "app", driver, nil); err != nil {
		t.Fatal(err)
	}
	has()
}

func TestRefs(t *testing.T) {
	for in, want := range map[string]string{
		"a":                "a:latest",
		"a:1":              "a:1",
		"org/a":            "org/a:latest",
		"host:5000/a":      "host:5000/a:latest",
		"host:5000/a:tag1": "host:5000/a:tag1",
	} {
		if got := NormalizeRef(in); got != want {
			t.Errorf("NormalizeRef(%q) = %q, want %q", in, got, want)
		}
	}
	if ValidateRef("Bad Name") == nil {
		t.Error("accepted an invalid ref")
	}
}

func TestOpenImportsJSONFiles(t *testing.T) {
	root := t.TempDir()
	write := func(path, content string) {
		t.Helper()
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	base := filepath.Join(root, "layers", "aaaaaaaaaaaa1", "layer.json")
	write(base, `{"id":"aaaaaaaaaaaa1","driver":"fake","guestOS":"linux","created":"2026-10-01T12:00:00Z","comment":"install linux"}`)
	write(filepath.Join(root, "layers", "bbbbbbbbbbbb2", "layer.json"),
		`{"id":"bbbbbbbbbbbb2","parent":"aaaaaaaaaaaa1","driver":"fake","guestOS":"linux","created":"2026-10-02T12:00:00Z","service":{"port":8080}}`)
	write(filepath.Join(root, "tags.json"), `{"app:latest":"bbbbbbbbbbbb2"}`)
	write(filepath.Join(root, "replaced.json"), `{"cccccccccccc3":"bbbbbbbbbbbb2"}`)

	// Twice: the second open finds the rows and no files.
	for range 2 {
		s := openStore(t, root)
		layer, err := s.Layer("aaaaaaaaaaaa1")
		if err != nil || layer.Comment != "install linux" || layer.GuestOS != machine.Linux || !layer.Created.Equal(time.Date(2026, 10, 1, 12, 0, 0, 0, time.UTC)) {
			t.Fatalf("base = %+v, %v", layer, err)
		}
		if id, err := s.Resolve("app"); err != nil || id != "bbbbbbbbbbbb2" {
			t.Fatalf("resolve app = %q, %v", id, err)
		}
		if id, err := s.Current("cccccccccccc3"); err != nil || id != "bbbbbbbbbbbb2" {
			t.Fatalf("current = %q, %v", id, err)
		}
		for _, path := range []string{base, filepath.Join(root, "tags.json"), filepath.Join(root, "replaced.json")} {
			if _, err := os.Stat(path); !errors.Is(err, os.ErrNotExist) {
				t.Fatalf("%s is still there: %v", path, err)
			}
		}
	}
}

func TestCommitClearsAStrayDirectory(t *testing.T) {
	s := openStore(t, t.TempDir())
	// What a crash between the rename and the row leaves.
	stray := filepath.Join(s.Root, "layers", "aaaaaaaaaaaa1", "payload")
	if err := os.MkdirAll(stray, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(stray, "old"), nil, 0o600); err != nil {
		t.Fatal(err)
	}
	if s.Has("aaaaaaaaaaaa1") {
		t.Fatal("a directory with no row is a layer")
	}
	commit(t, s, "aaaaaaaaaaaa1", "")
	if _, err := os.Stat(filepath.Join(stray, "old")); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("the stray payload survived: %v", err)
	}
	if data, err := os.ReadFile(filepath.Join(stray, "data")); err != nil || string(data) != "aaaaaaaaaaaa1" {
		t.Fatalf("payload = %q, %v", data, err)
	}
}
