package boxd

import (
	"context"
	"encoding/hex"
	"fmt"
	"slices"
	"strings"
	"time"

	"github.com/discobox-ai/vm/pkg/machine"
	"github.com/discobox-ai/vm/pkg/machine/boxd/internal/boxdapi"
)

// namePrefix starts the name of every machine and snapshot the driver makes,
// so that the caller's other machines and snapshots are never taken for the
// driver's. The project follows it.
const namePrefix = "dvm-"

// tagMark parts a tag's snapshot's image from its ref:
// dvm-<project>--<image id>--t-<ref>.
const tagMark = "--t-"

// imageID is the image a snapshot name is of, or "" for any other name: a
// tag's, another project's, or one from before ADR 26-10-23-417, which is
// dvm-<project>--<image id prefix>-<random>.
func (d *Driver) imageID(name string) string {
	id, ok := strings.CutPrefix(name, d.prefix())
	if !ok || !isHex(id) {
		return ""
	}
	return id
}

func isHex(s string) bool {
	if s == "" {
		return false
	}
	for _, r := range s {
		if (r < '0' || r > '9') && (r < 'a' || r > 'f') {
			return false
		}
	}
	return true
}

// tagName is a tag's snapshot of an image.
func (d *Driver) tagName(image, ref string) string {
	return d.imageName(image) + tagMark + escapeRef(ref)
}

// parseTag is the tag a snapshot name is, if it is one of the project's.
func (d *Driver) parseTag(name string) (machine.Tag, bool) {
	rest, ok := strings.CutPrefix(name, d.prefix())
	if !ok {
		return machine.Tag{}, false
	}
	image, escaped, ok := strings.Cut(rest, tagMark)
	if !ok || !isHex(image) {
		return machine.Tag{}, false
	}
	ref, ok := unescapeRef(escaped)
	return machine.Tag{Ref: ref, Image: image}, ok
}

// escapeRef spells a ref in what a snapshot name allows, and can be read
// back: lowercase letters and digits stand for themselves, and every other
// byte is '-' and its two hex digits.
func escapeRef(ref string) string {
	var b strings.Builder
	for i := range len(ref) {
		c := ref[i]
		if c >= 'a' && c <= 'z' || c >= '0' && c <= '9' {
			b.WriteByte(c)
		} else {
			fmt.Fprintf(&b, "-%02x", c)
		}
	}
	return b.String()
}

func unescapeRef(s string) (string, bool) {
	var b strings.Builder
	for i := 0; i < len(s); i++ {
		if s[i] != '-' {
			b.WriteByte(s[i])
			continue
		}
		if i+2 >= len(s) {
			return "", false
		}
		c, err := hex.DecodeString(s[i+1 : i+3])
		if err != nil {
			return "", false
		}
		b.Write(c)
		i += 2
	}
	return b.String(), true
}

// unixTime reads one of boxd's times, counted in unit since the epoch: a
// machine's created_at is in seconds, and a snapshot's in milliseconds.
func unixTime(n int64, unit time.Duration) time.Time {
	if n <= 0 {
		return time.Time{}
	}
	return time.Unix(0, 0).Add(time.Duration(n) * unit)
}

// imageOf is a ready snapshot as an image: boxd keeps no comment.
func imageOf(id string, snap *boxdapi.SnapshotInfo) machine.Image {
	return machine.Image{ID: id, GuestOS: machine.Linux, Created: unixTime(snap.GetCreatedAt(), time.Millisecond)}
}

// Image is the image's snapshot, once it is ready.
func (d *Driver) Image(ctx context.Context, id string) (machine.Image, error) {
	if !isHex(id) {
		return machine.Image{}, fmt.Errorf("boxd: image %s: %w", id, machine.ErrNotFound)
	}
	api, err := d.api.API()
	if err != nil {
		return machine.Image{}, err
	}
	got, err := api.GetSnapshot(ctx, &boxdapi.GetSnapshotRequest{Name: d.imageName(id)})
	if isNotFound(err) || (err == nil && got.GetSnapshot().GetStatus() != "ready") {
		return machine.Image{}, fmt.Errorf("boxd: image %s: %w", id, machine.ErrNotFound)
	}
	if err != nil {
		return machine.Image{}, fmt.Errorf("boxd: snapshot %s: %w", d.imageName(id), err)
	}
	return imageOf(id, got.GetSnapshot()), nil
}

// snapshots lists the caller's snapshots.
func (d *Driver) snapshots(ctx context.Context) ([]*boxdapi.SnapshotInfo, error) {
	api, err := d.api.API()
	if err != nil {
		return nil, err
	}
	listed, err := api.ListSnapshots(ctx, &boxdapi.ListSnapshotsRequest{})
	if err != nil {
		return nil, fmt.Errorf("boxd: list snapshots: %w", err)
	}
	return listed.GetSnapshots(), nil
}

// Images lists the project's ready image snapshots.
func (d *Driver) Images(ctx context.Context) ([]machine.Image, error) {
	snaps, err := d.snapshots(ctx)
	if err != nil {
		return nil, err
	}
	var out []machine.Image
	for _, snap := range snaps {
		if id := d.imageID(snap.GetName()); id != "" && snap.GetStatus() == "ready" {
			out = append(out, imageOf(id, snap))
		}
	}
	return out, nil
}

// Tags lists the project's ready tag snapshots.
func (d *Driver) Tags(ctx context.Context) ([]machine.Tag, error) {
	snaps, err := d.snapshots(ctx)
	if err != nil {
		return nil, err
	}
	var out []machine.Tag
	for _, snap := range snaps {
		if t, ok := d.parseTag(snap.GetName()); ok && snap.GetStatus() == "ready" {
			out = append(out, t)
		}
	}
	slices.SortFunc(out, func(a, b machine.Tag) int { return strings.Compare(a.Ref, b.Ref) })
	return out, nil
}

// Tag saves a second snapshot of the image, named for the image and the tag,
// then deletes the snapshots of the tag on any other image. boxd has no way to
// name or label a snapshot after the fact, and snapshots only a running
// machine, so the image's snapshot is restored into a machine and that machine
// is snapshotted. Nothing restores from a tag's snapshot.
func (d *Driver) Tag(ctx context.Context, ref, image string) error {
	if _, err := d.Image(ctx, image); err != nil {
		return err
	}
	snaps, err := d.snapshots(ctx)
	if err != nil {
		return err
	}
	name := d.tagName(image, ref)
	have := slices.ContainsFunc(snaps, func(s *boxdapi.SnapshotInfo) bool { return s.GetName() == name && s.GetStatus() == "ready" })
	if !have {
		if err := d.saveAs(ctx, d.imageName(image), name); err != nil {
			return fmt.Errorf("boxd: tag %s: %w", ref, err)
		}
	}
	for _, snap := range snaps {
		if t, ok := d.parseTag(snap.GetName()); ok && t.Ref == ref && snap.GetName() != name {
			if err := d.deleteSnapshot(ctx, snap.GetName()); err != nil {
				return err
			}
		}
	}
	return nil
}

// saveAs saves the snapshot from again under the name to, through a machine
// restored from it and destroyed after.
func (d *Driver) saveAs(ctx context.Context, from, to string) error {
	api, err := d.api.API()
	if err != nil {
		return err
	}
	created, err := api.CreateVmFromSnapshot(ctx, &boxdapi.CreateVmFromSnapshotRequest{
		Snapshot: from, Name: d.transientName("save"),
	})
	if err != nil {
		return fmt.Errorf("restore %s: %w", from, err)
	}
	vm := created.GetVmId()
	defer d.destroyVM(vm)
	if _, err := waitVM(ctx, api, vm, "running"); err != nil {
		return err
	}
	saved, err := api.CreateSnapshot(ctx, &boxdapi.CreateSnapshotRequest{Vm: vm, Name: to})
	if err != nil {
		return fmt.Errorf("snapshot %s: %w", to, err)
	}
	if _, err := waitSnapshot(ctx, api, to, saved.GetVersion()); err != nil {
		_ = d.deleteSnapshot(context.Background(), to)
		return err
	}
	return nil
}

// Untag deletes every snapshot of the tag.
func (d *Driver) Untag(ctx context.Context, ref string) error {
	snaps, err := d.snapshots(ctx)
	if err != nil {
		return err
	}
	for _, snap := range snaps {
		if t, ok := d.parseTag(snap.GetName()); ok && t.Ref == ref {
			if err := d.deleteSnapshot(ctx, snap.GetName()); err != nil {
				return err
			}
		}
	}
	return nil
}

// instanceName is the instance a machine name is of, in the driver's project:
// `dvm-<project>--<name>`. A machine of the driver's own (transientName) is
// one too, so that none is out of sight.
func (d *Driver) instanceName(vmName string) (string, bool) {
	return strings.CutPrefix(vmName, d.prefix())
}

// instance is a machine as an instance. boxd says which image a machine was
// made from, but not which snapshot, so an instance of an image has none.
func (d *Driver) instance(vm *boxdapi.GetVmResponse) machine.Instance {
	name, _ := d.instanceName(vm.GetName())
	return machine.Instance{
		ID:      vm.GetVmId(),
		Name:    name,
		Image:   vm.GetImageRef(),
		GuestOS: machine.Linux,
		CPUs:    int(vm.GetVcpu()),
		Memory:  vm.GetMemoryBytes(),
		Created: unixTime(vm.GetCreatedAt(), time.Second),
	}
}

// Instances lists the project's machines.
func (d *Driver) Instances(ctx context.Context) ([]machine.Instance, error) {
	api, err := d.api.API()
	if err != nil {
		return nil, err
	}
	listed, err := api.ListVms(ctx, &boxdapi.ListVmsRequest{})
	if err != nil {
		return nil, fmt.Errorf("boxd: list machines: %w", err)
	}
	var out []machine.Instance
	for _, vm := range listed.GetVms() {
		if _, ok := d.instanceName(vm.GetName()); ok && vm.GetStatus() != "destroying" {
			out = append(out, d.instance(vm))
		}
	}
	slices.SortFunc(out, func(a, b machine.Instance) int { return a.Created.Compare(b.Created) })
	return out, nil
}

// Instance is the project's machine of the name.
func (d *Driver) Instance(ctx context.Context, name string) (machine.Instance, error) {
	all, err := d.Instances(ctx)
	if err != nil {
		return machine.Instance{}, err
	}
	for _, inst := range all {
		if inst.Name == name {
			return inst, nil
		}
	}
	return machine.Instance{}, fmt.Errorf("instance %q: %w", name, machine.ErrNotFound)
}
