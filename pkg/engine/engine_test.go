package engine_test

import (
	"context"
	"errors"
	"fmt"
	"runtime"
	"strings"
	"testing"

	"github.com/discobox-ai/vm/pkg/engine"
	"github.com/discobox-ai/vm/pkg/machine"
	"github.com/discobox-ai/vm/pkg/machine/fake"
	"github.com/discobox-ai/vm/pkg/machine/local"
)

// fakeDriver is the fake driver for a project, keeping its state in root.
func fakeDriver(t *testing.T, root, project string) machine.Driver {
	t.Helper()
	d, err := local.New(machine.Config{Dir: root, Project: project}, &fake.Hypervisor{})
	if err != nil {
		t.Fatal(err)
	}
	return d
}

// keeper is a driver that refuses to delete the images in needed, as hcs
// refuses a disk another image's differences over.
type keeper struct {
	machine.Driver
	needed map[string]bool
}

func (k keeper) DeleteImage(ctx context.Context, id string) error {
	if k.needed[id] {
		return fmt.Errorf("image %s: %w", id, machine.ErrInUse)
	}
	return k.Driver.DeleteImage(ctx, id)
}

// An image is installed and tagged, listed, removed only once nothing tags
// or runs it, and kept, untagged, while its driver says another image needs
// its disk.
func TestInstallListAndRemoveImages(t *testing.T) {
	ctx := context.Background()
	d := keeper{Driver: fakeDriver(t, t.TempDir(), ""), needed: map[string]bool{}}
	e, err := engine.Open(d, "")
	if err != nil {
		t.Fatal(err)
	}
	img, err := e.Install(ctx, engine.InstallOptions{}, "base", "base:v1")
	if err != nil || len(img.ID) != 64 || img.GuestOS != machine.OS(runtime.GOOS) {
		t.Fatalf("install = %+v, %v", img, err)
	}
	entries, err := e.Images(ctx)
	if err != nil || len(entries) != 2 || entries[0].Ref != "base:latest" || entries[1].Ref != "base:v1" || entries[0].Image.ID != img.ID {
		t.Fatalf("images = %+v, %v", entries, err)
	}
	inst, err := e.Create(ctx, "base", engine.CreateOptions{Name: "one"})
	if err != nil || inst.ImageID != img.ID {
		t.Fatalf("create = %+v, %v", inst, err)
	}
	if r, err := e.RemoveImage(ctx, "base:v1"); err != nil || r.Untagged != "base:v1" || r.Deleted != "" {
		t.Fatalf("rmi of one of two tags = %+v, %v", r, err)
	}
	if _, err := e.RemoveImage(ctx, img.ID); err == nil {
		t.Fatal("rmi by ID of a tagged image")
	}
	if r, err := e.RemoveImage(ctx, "base"); err != nil || r.Untagged != "base:latest" || r.Deleted != "" {
		t.Fatalf("rmi of the last tag of an image in use = %+v, %v", r, err)
	}
	if err := e.Remove(ctx, inst, true); err != nil {
		t.Fatal(err)
	}
	d.needed[img.ID] = true
	if _, err := e.RemoveImage(ctx, img.ID[:8]); !errors.Is(err, machine.ErrInUse) {
		t.Fatalf("rmi by ID of an image the driver needs: %v, want ErrInUse", err)
	}
	if err := e.Tag(ctx, "again", img.ID); err != nil {
		t.Fatal(err)
	}
	if r, err := e.RemoveImage(ctx, "again"); err != nil || r.Deleted != "" || !errors.Is(r.Kept, machine.ErrInUse) {
		t.Fatalf("rmi of the last tag of an image the driver needs = %+v, %v", r, err)
	}
	delete(d.needed, img.ID)
	if r, err := e.RemoveImage(ctx, img.ID); err != nil || r.Deleted != img.ID {
		t.Fatalf("rmi = %+v, %v", r, err)
	}
	if entries, err := e.Images(ctx); err != nil || len(entries) != 0 {
		t.Fatalf("images after rmi = %+v, %v", entries, err)
	}
}

func TestOpenRefusesABadProject(t *testing.T) {
	for _, project := range []string{"Foo", "a--b", "-a", "a-", "a_b", strings.Repeat("a", 33)} {
		if _, err := engine.Open(fakeDriver(t, t.TempDir(), ""), project); err == nil {
			t.Errorf("project %q was accepted", project)
		}
	}
}

// A fence is refused at once by an engine whose driver cannot fence, and
// nothing is made.
func TestEgressNeedsADriverThatFences(t *testing.T) {
	ctx := context.Background()
	e, err := engine.Open(fakeDriver(t, t.TempDir(), ""), "")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := e.Install(ctx, engine.InstallOptions{}, "base"); err != nil {
		t.Fatal(err)
	}
	if _, err := e.Create(ctx, "base", engine.CreateOptions{Name: "fenced", Egress: &machine.Egress{}}); !errors.Is(err, machine.ErrUnsupported) {
		t.Fatalf("a fence on the fake driver: %v, want ErrUnsupported", err)
	}
	if list, err := e.List(ctx); err != nil || len(list) != 0 {
		t.Fatalf("instances after a refused fence: %+v, %v", list, err)
	}
}
