package guest

import (
	"os"
	"path/filepath"
	"testing"
)

func TestDarwinPath(t *testing.T) {
	etc := t.TempDir()
	if err := os.WriteFile(filepath.Join(etc, "paths"), []byte("/usr/local/bin\n/usr/bin\n/bin\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(etc, "paths.d"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(etc, "paths.d", "homebrew"), []byte("/opt/homebrew/bin\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	got := darwinPath(etc, "/usr/bin:/bin:/usr/sbin:/sbin")
	want := "/usr/local/bin:/usr/bin:/bin:/opt/homebrew/bin:/usr/sbin:/sbin"
	if got != want {
		t.Fatalf("PATH = %s, want %s", got, want)
	}
}
