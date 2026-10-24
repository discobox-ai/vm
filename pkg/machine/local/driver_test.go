package local_test

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"slices"
	"testing"

	"github.com/discobox-ai/vm/pkg/machine"
	"github.com/discobox-ai/vm/pkg/machine/fake"
	"github.com/discobox-ai/vm/pkg/machine/local"
)

// chained is the fake hypervisor as one whose disks chain, as hcs's do,
// recording the chain each Prepare was given.
type chained struct {
	*fake.Hypervisor
	prepared [][]string
}

func (*chained) Chains() bool { return true }

func (c *chained) Prepare(ctx context.Context, inst local.InstanceSpec) error {
	var ids []string
	for _, l := range inst.Chain {
		ids = append(ids, l.ID)
	}
	c.prepared = append(c.prepared, ids)
	return c.Hypervisor.Prepare(ctx, inst)
}

// A snapshot of an instance of an image, on a hypervisor that chains, is
// prepared over that image's disk, and the image is refused deletion while
// the snapshot needs it; an image whose parent was lost behind the driver's
// back is not found, and is still listed and deleted. On a hypervisor that
// does not chain, a parent is never needed.
func TestDeleteImageKeepsADiskAnotherNeeds(t *testing.T) {
	ctx := context.Background()
	guestOS := machine.OS(runtime.GOOS)
	root := t.TempDir()
	hv := &chained{Hypervisor: &fake.Hypervisor{}}
	d, err := local.New(machine.Config{Dir: root}, hv)
	if err != nil {
		t.Fatal(err)
	}
	snapshot := func(from, name, id string) machine.Image {
		t.Helper()
		inst, err := d.Create(ctx, machine.InstanceSpec{Name: name, ImageID: from, GuestOS: guestOS})
		if err != nil {
			t.Fatal(err)
		}
		img, err := d.Commit(ctx, inst, machine.Image{ID: id, GuestOS: guestOS})
		if err != nil {
			t.Fatal(err)
		}
		return img
	}
	base, err := d.Install(ctx, machine.InstallSpec{GuestOS: guestOS, Media: "latest"}, machine.Image{ID: "aaaa", GuestOS: guestOS})
	if err != nil {
		t.Fatal(err)
	}
	first := snapshot(base.ID, "one", "bbbb")
	second := snapshot(first.ID, "two", "cccc")
	if _, err := d.Create(ctx, machine.InstanceSpec{Name: "three", ImageID: second.ID, GuestOS: guestOS}); err != nil {
		t.Fatal(err)
	}
	if got := hv.prepared[len(hv.prepared)-1]; len(got) != 3 || got[0] != "aaaa" || got[1] != "bbbb" || got[2] != "cccc" {
		t.Fatalf("prepared over %v, want the whole chain, base first", got)
	}

	for _, id := range []string{base.ID, first.ID} {
		if err := d.DeleteImage(ctx, id); !errors.Is(err, machine.ErrInUse) {
			t.Fatalf("delete %s, which another image needs: %v, want ErrInUse", id, err)
		}
	}
	// Another project shares the directory and sees none of it.
	other, err := local.New(machine.Config{Dir: root, Project: "other"}, hv)
	if err != nil {
		t.Fatal(err)
	}
	if images, err := other.Images(ctx); err != nil || len(images) != 0 {
		t.Fatalf("another project's images = %+v, %v", images, err)
	}

	// A parent deleted behind the driver's back leaves its child unbootable:
	// not found, but listed, and deleted by rmi.
	if err := os.RemoveAll(filepath.Join(root, "images", "layers", first.ID)); err != nil {
		t.Fatal(err)
	}
	if _, err := d.Image(ctx, second.ID); !errors.Is(err, machine.ErrNotFound) {
		t.Fatalf("an image whose parent is gone: %v, want ErrNotFound", err)
	}
	images, err := d.Images(ctx)
	if err != nil || !slices.ContainsFunc(images, func(img machine.Image) bool { return img.ID == second.ID }) {
		t.Fatalf("images = %+v, %v; want the one whose parent is gone listed", images, err)
	}
	if err := d.DeleteImage(ctx, second.ID); err != nil {
		t.Fatal(err)
	}
	if err := d.DeleteImage(ctx, base.ID); err != nil {
		t.Fatalf("delete %s once nothing needs it: %v", base.ID, err)
	}
	if _, err := d.Image(ctx, base.ID); !errors.Is(err, machine.ErrNotFound) {
		t.Fatalf("a deleted image: %v", err)
	}

	// The fake hypervisor's own images are trees of their own.
	plain, err := local.New(machine.Config{Dir: t.TempDir()}, &fake.Hypervisor{})
	if err != nil {
		t.Fatal(err)
	}
	b, err := plain.Install(ctx, machine.InstallSpec{GuestOS: guestOS, Media: "latest"}, machine.Image{ID: "dddd", GuestOS: guestOS})
	if err != nil {
		t.Fatal(err)
	}
	inst, err := plain.Create(ctx, machine.InstanceSpec{Name: "four", ImageID: b.ID, GuestOS: guestOS})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := plain.Commit(ctx, inst, machine.Image{ID: "eeee", GuestOS: guestOS}); err != nil {
		t.Fatal(err)
	}
	if err := plain.DeleteImage(ctx, b.ID); err != nil {
		t.Fatalf("delete an image another was snapshotted from, on a hypervisor that does not chain: %v", err)
	}
}

// A tag and an instance written when images were layers name their image
// "layer", and still read: the tag as the image's, the instance as its image
// ID, which keeps rmi from deleting a disk the instance runs from.
func TestRecordsFromBeforeRead(t *testing.T) {
	ctx := context.Background()
	guestOS := machine.OS(runtime.GOOS)
	root := t.TempDir()
	d, err := local.New(machine.Config{Dir: root}, &fake.Hypervisor{})
	if err != nil {
		t.Fatal(err)
	}
	img, err := d.Install(ctx, machine.InstallSpec{GuestOS: guestOS, Media: "latest"}, machine.Image{ID: "aaaa", GuestOS: guestOS})
	if err != nil {
		t.Fatal(err)
	}
	inst, err := d.Create(ctx, machine.InstanceSpec{Name: "old", ImageID: img.ID, GuestOS: guestOS})
	if err != nil {
		t.Fatal(err)
	}
	tags := filepath.Join(root, "images", "tags", machine.DefaultProject)
	if err := os.MkdirAll(tags, 0o755); err != nil {
		t.Fatal(err)
	}
	// "app:latest" in hex, as the driver names a tag's file.
	if err := os.WriteFile(filepath.Join(tags, "6170703a6c6174657374.json"), []byte(`{"ref":"app:latest","layer":"aaaa"}`), 0o600); err != nil {
		t.Fatal(err)
	}
	record := filepath.Join(root, "instances", inst.ID, "instance.json")
	old := `{"id":"` + inst.ID + `","name":"old","image":"app","layer":"aaaa","guestOS":"` + string(guestOS) + `","project":"default"}`
	if err := os.WriteFile(record, []byte(old), 0o600); err != nil {
		t.Fatal(err)
	}
	if got, err := d.Tags(ctx); err != nil || len(got) != 1 || got[0].Image != "aaaa" {
		t.Fatalf("tags = %+v, %v", got, err)
	}
	if got, err := d.Instance(ctx, "old"); err != nil || got.ImageID != "aaaa" {
		t.Fatalf("instance = %+v, %v", got, err)
	}
}
