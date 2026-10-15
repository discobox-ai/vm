package engine_test

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/discobox-ai/x/gormdb"
	"gorm.io/gorm/logger"

	"github.com/discobox-ai/vm/pkg/engine"
	"github.com/discobox-ai/vm/pkg/machine"
	"github.com/discobox-ai/vm/pkg/machine/fake"
)

// An instance an older disco-vm recorded in instance.json is a row after the
// first open, and the file is gone.
func TestOpenImportsInstanceFiles(t *testing.T) {
	root := t.TempDir()
	path := filepath.Join(root, "instances", "0123456789ab", "instance.json")
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	record := `{"id":"0123456789ab","name":"web","image":"app","layer":"aaaaaaaaaaaa1","driver":"fake","guestOS":"linux",` +
		`"mode":"cold","cpus":2,"memory":1073741824,"created":"2026-10-01T12:00:00Z","forwards":[{"port":9000,"socket":"/run/x.sock"}],"service":8080}`
	if err := os.WriteFile(path, []byte(record), 0o600); err != nil {
		t.Fatal(err)
	}
	for range 2 {
		e, err := engine.Open(root, &fake.Driver{}, nil)
		if err != nil {
			t.Fatal(err)
		}
		inst, err := e.Get("web")
		if err != nil {
			t.Fatal(err)
		}
		if inst.ID != "0123456789ab" || inst.GuestOS != machine.Linux || inst.Mode != machine.Cold || inst.CPUs != 2 ||
			inst.Memory != 1<<30 || len(inst.Forwards) != 1 || inst.Forwards[0].Socket != "/run/x.sock" {
			t.Fatalf("instance = %+v", inst)
		}
		if byPrefix, err := e.Get("0123"); err != nil || byPrefix.Name != "web" {
			t.Fatalf("get by prefix = %+v, %v", byPrefix, err)
		}
		if _, err := os.Stat(path); !errors.Is(err, os.ErrNotExist) {
			t.Fatalf("instance.json is still there: %v", err)
		}
		if err := e.Close(); err != nil {
			t.Fatal(err)
		}
	}
}

// A program that embeds the engine gives it the database its own tables are
// in. Every table the engine makes there is named disco_vm_*, and the
// database stays open when the engine closes.
func TestOpenInACallersDatabase(t *testing.T) {
	dir := t.TempDir()
	db, err := gormdb.Open(gormdb.Config{DSN: filepath.Join(dir, "embedder.db"), Logger: logger.Discard})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })
	// The names discobox's own models use for the same things.
	type sandbox struct{ ID string }
	if err := db.Write.Table("instances").AutoMigrate(&sandbox{}); err != nil {
		t.Fatal(err)
	}
	if err := db.Write.Table("layers").AutoMigrate(&sandbox{}); err != nil {
		t.Fatal(err)
	}
	root := filepath.Join(dir, "root")
	e, err := engine.Open(root, &fake.Driver{}, db)
	if err != nil {
		t.Fatal(err)
	}
	if err := e.Close(); err != nil {
		t.Fatal(err)
	}
	tables, err := db.Write.Migrator().GetTables()
	if err != nil {
		t.Fatal(err)
	}
	ours := 0
	for _, table := range tables {
		switch {
		case strings.HasPrefix(table, "disco_vm_"):
			ours++
		case table != "instances" && table != "layers":
			t.Errorf("the engine made table %q, which is not named disco_vm_*", table)
		}
	}
	if ours != 5 {
		t.Errorf("tables = %v, want five named disco_vm_*", tables)
	}
	if _, err := os.Stat(filepath.Join(root, "disco-vm.db")); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("the engine made a database of its own: %v", err)
	}
}
