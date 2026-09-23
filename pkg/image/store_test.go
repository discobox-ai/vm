package image

import (
	"context"
	"errors"
	"os"
	"testing"

	"github.com/discobox-ai/vm/pkg/machine"
	"github.com/discobox-ai/vm/pkg/machine/fake"
)

func commit(t *testing.T, s *Store, id, parent string) {
	t.Helper()
	p, err := s.Begin(Layer{ID: id, Parent: parent, Driver: "fake", GuestOS: machine.Linux})
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(p.MachineLayer().Dir+"/data", []byte(id), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := p.Commit(); err != nil {
		t.Fatal(err)
	}
}

func TestStoreTagsChainsAndRemove(t *testing.T) {
	s, err := Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
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
	s, _ := Open(t.TempDir())
	commit(t, s, "aaaaaaaaaaaa1", "")
	_ = s.Tag("base", "aaaaaaaaaaaa1")
	driver, _ := fake.New()
	removed, err := s.Remove(context.Background(), "base", driver, func(id string) bool { return true })
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
