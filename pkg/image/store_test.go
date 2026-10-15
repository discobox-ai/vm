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
	if err := s.Tag("base", "aaaaaaaaaaaa1"); err != nil {
		t.Fatal(err)
	}
	if err := s.Tag("app:v1", "cccccccccccc3"); err != nil {
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

	driver, _ := fake.New()
	// Removing app deletes its layer and the untagged middle one, and stops at
	// the tagged base.
	removed, err := s.Remove(context.Background(), "app:v1", driver, nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(removed) != 2 || !s.Has("aaaaaaaaaaaa1") || s.Has("bbbbbbbbbbbb2") {
		t.Fatalf("removed %v", removed)
	}
}

func TestRemoveKeepsLayersInUse(t *testing.T) {
	s := openStore(t, t.TempDir())
	commit(t, s, "aaaaaaaaaaaa1", "")
	_ = s.Tag("base", "aaaaaaaaaaaa1")
	driver, _ := fake.New()
	removed, err := s.Remove(context.Background(), "base", driver, func(string) bool { return true })
	if err != nil || len(removed) != 0 || !s.Has("aaaaaaaaaaaa1") {
		t.Fatalf("removed %v, %v", removed, err)
	}
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
		for _, path := range []string{base, filepath.Join(root, "tags.json")} {
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
