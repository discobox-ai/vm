package engine_test

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

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
		e, err := engine.Open(root, &fake.Driver{}, nil, "")
		if err != nil {
			t.Fatal(err)
		}
		inst, err := e.Get("web")
		if err != nil {
			t.Fatal(err)
		}
		if inst.ID != "0123456789ab" || inst.GuestOS != machine.Linux || inst.CPUs != 2 ||
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
	e, err := engine.Open(root, &fake.Driver{}, db, "")
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
	if ours != 4 {
		t.Errorf("tables = %v, want four named disco_vm_*", tables)
	}
	if _, err := os.Stat(filepath.Join(root, "disco-vm.db")); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("the engine made a database of its own: %v", err)
	}
}

// A database from before projects (tags keyed by ref alone, instance names
// unique on their own) opens as the default project's, and another project
// can then use the same names.
func TestOpenUpgradesADatabaseFromBeforeProjects(t *testing.T) {
	dir := t.TempDir()
	db, err := gormdb.Open(gormdb.Config{DSN: filepath.Join(dir, "old.db"), Logger: logger.Discard})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })
	type oldLayer struct {
		ID      string `gorm:"primaryKey"`
		Parent  string `gorm:"index"`
		Driver  string
		GuestOS string `gorm:"column:guest_os"`
		Created time.Time
	}
	type oldTag struct {
		Ref     string `gorm:"primaryKey"`
		LayerID string `gorm:"index"`
	}
	type oldInstance struct {
		ID      string `gorm:"primaryKey"`
		Name    string `gorm:"uniqueIndex"`
		Image   string
		Layer   string `gorm:"index"`
		Driver  string
		GuestOS string `gorm:"column:guest_os"`
		Mode    string
		Created time.Time
	}
	w := db.Write
	if err := w.Table("disco_vm_layers").AutoMigrate(&oldLayer{}); err != nil {
		t.Fatal(err)
	}
	if err := w.Table("disco_vm_tags").AutoMigrate(&oldTag{}); err != nil {
		t.Fatal(err)
	}
	if err := w.Table("disco_vm_instances").AutoMigrate(&oldInstance{}); err != nil {
		t.Fatal(err)
	}
	if !w.Migrator().HasIndex("disco_vm_instances", "idx_disco_vm_instances_name") {
		t.Fatal("the old schema's name index is not where the upgrade looks for it")
	}
	root := filepath.Join(dir, "root")
	layerDir := filepath.Join(root, "images", "layers", "aaaaaaaaaaaa1", "payload", "rootfs")
	if err := os.MkdirAll(layerDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(root, "instances", "0123456789ab", "machine"), 0o755); err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC()
	if err := w.Table("disco_vm_layers").Create(&oldLayer{ID: "aaaaaaaaaaaa1", Driver: "fake", GuestOS: "linux", Created: now}).Error; err != nil {
		t.Fatal(err)
	}
	if err := w.Table("disco_vm_tags").Create(&oldTag{Ref: "app:latest", LayerID: "aaaaaaaaaaaa1"}).Error; err != nil {
		t.Fatal(err)
	}
	if err := w.Table("disco_vm_instances").Create(&oldInstance{ID: "0123456789ab", Name: "web", Image: "app", Layer: "aaaaaaaaaaaa1", Driver: "fake", GuestOS: "linux", Mode: "cold", Created: now}).Error; err != nil {
		t.Fatal(err)
	}

	for range 2 {
		e, err := engine.Open(root, &fake.Driver{}, db, "")
		if err != nil {
			t.Fatal(err)
		}
		if id, err := e.Images.Resolve("app"); err != nil || id != "aaaaaaaaaaaa1" {
			t.Fatalf("the old tag resolves to %q, %v", id, err)
		}
		if inst, err := e.Get("web"); err != nil || inst.ID != "0123456789ab" || inst.Project != machine.DefaultProject {
			t.Fatalf("the old instance = %+v, %v", inst, err)
		}
		if err := e.Close(); err != nil {
			t.Fatal(err)
		}
	}
	if w.Migrator().HasTable("disco_vm_tags") {
		t.Fatal("the old tags table is still there")
	}

	// Another project sees none of it, and may use its names.
	other, err := engine.Open(root, &fake.Driver{}, db, "other")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = other.Close() })
	if _, err := other.Images.Resolve("app"); err == nil {
		t.Fatal("another project resolves the default's tag")
	}
	if _, err := other.Images.Resolve("aaaaaaaaaaaa1"); err == nil {
		t.Fatal("another project resolves the default's layer by ID")
	}
	if _, err := other.Get("web"); err == nil {
		t.Fatal("another project finds the default's instance")
	}
	if list, err := other.List(); err != nil || len(list) != 0 {
		t.Fatalf("another project lists %v, %v", list, err)
	}
	if err := db.Write.Table("disco_vm_instances").Create(map[string]any{"id": "fedcba987654", "project": "other", "name": "web"}).Error; err != nil {
		t.Fatalf("another project cannot name an instance web: %v", err)
	}
	if err := db.Write.Table("disco_vm_instances").Create(map[string]any{"id": "fedcba987655", "project": "other", "name": "web"}).Error; err == nil {
		t.Fatal("two instances in one project share a name")
	}
}

// A database from when images could be warmed loses the warm stages' table,
// and its instances, each with the clone mode it was made in, are kept and
// can be added to: the mode column stays, and nothing reads or writes it.
func TestOpenDropsWarmStages(t *testing.T) {
	dir := t.TempDir()
	db, err := gormdb.Open(gormdb.Config{DSN: filepath.Join(dir, "old.db"), Logger: logger.Discard})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })
	root := filepath.Join(dir, "root")
	e, err := engine.Open(root, &fake.Driver{}, db, "")
	if err != nil {
		t.Fatal(err)
	}
	if err := e.Close(); err != nil {
		t.Fatal(err)
	}
	type oldWarm struct {
		Layer   string `gorm:"primaryKey"`
		Project string
		Image   string
		Count   int
	}
	w := db.Write
	if err := w.Table("disco_vm_warm_stages").AutoMigrate(&oldWarm{}); err != nil {
		t.Fatal(err)
	}
	if err := w.Table("disco_vm_warm_stages").Create(&oldWarm{Layer: "aaaaaaaaaaaa1", Project: "default", Image: "app", Count: 1}).Error; err != nil {
		t.Fatal(err)
	}
	if err := w.Exec("ALTER TABLE disco_vm_instances ADD COLUMN mode text").Error; err != nil {
		t.Fatal(err)
	}
	if err := w.Table("disco_vm_instances").Create(map[string]any{"id": "0123456789ab", "project": "default", "name": "web", "layer": "aaaaaaaaaaaa1", "temporary": false, "mode": "resume"}).Error; err != nil {
		t.Fatal(err)
	}

	for range 2 {
		e, err := engine.Open(root, &fake.Driver{}, db, "")
		if err != nil {
			t.Fatal(err)
		}
		if inst, err := e.Get("web"); err != nil || inst.ID != "0123456789ab" {
			t.Fatalf("the instance made in resume mode = %+v, %v", inst, err)
		}
		if list, err := e.List(); err != nil || len(list) != 1 || list[0].Name != "web" {
			t.Fatalf("list = %v, %v", list, err)
		}
		if err := e.Close(); err != nil {
			t.Fatal(err)
		}
	}
	if w.Migrator().HasTable("disco_vm_warm_stages") {
		t.Fatal("the warm stages' table is still there")
	}
	if err := w.Create(&engine.Instance{ID: "fedcba987654", Project: "default", Name: "api", Layer: "aaaaaaaaaaaa1"}).Error; err != nil {
		t.Fatalf("an instance cannot be added beside the old mode column: %v", err)
	}
}

func TestOpenRefusesABadProject(t *testing.T) {
	for _, project := range []string{"Foo", "a--b", "-a", "a-", "a_b", strings.Repeat("a", 33)} {
		if _, err := engine.Open(t.TempDir(), &fake.Driver{}, nil, project); err == nil {
			t.Errorf("project %q was accepted", project)
		}
	}
}
