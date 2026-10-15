package boxd

import (
	"context"
	"fmt"
	"path/filepath"
	"slices"
	"time"

	"github.com/discobox-ai/vm/pkg/machine"
	"github.com/discobox-ai/vm/pkg/machine/boxd/internal/boxdapi"
)

// namePrefix starts the name of every machine and snapshot the driver makes
// (vmName, snapshot, tagSnapshot), so that the caller's other machines and
// snapshots are never taken for orphans. The project follows it.
const namePrefix = "dvm-"

// Orphans lists the machines and snapshots in the caller's boxd context named
// for known's project that known does not name: no instance's vm.json, no
// layer's layer.json, no stage's stage.json, and no tag's snapshot name.
func (d *Driver) Orphans(ctx context.Context, known machine.Known) ([]machine.Orphan, error) {
	project := known.Project
	if project == "" {
		project = machine.DefaultProject
	}
	vms, snapshots := map[string]bool{}, map[string]bool{}
	for _, inst := range known.Instances {
		var ref vmRef
		if readJSON(filepath.Join(inst.Dir, vmFile), &ref) == nil {
			vms[ref.ID], vms[ref.Name] = true, true
		}
	}
	for _, layer := range known.Layers {
		var snap snapshotRef
		if readJSON(filepath.Join(layer.Dir, layerFile), &snap) == nil {
			snapshots[snap.Name] = true
		}
	}
	for _, tag := range known.Tags {
		snapshots[tagSnapshot(tag.Layer, tag.Ref)] = true
	}
	for _, dir := range known.Stages {
		var snap snapshotRef
		if readJSON(filepath.Join(dir, stageFile), &snap) == nil {
			snapshots[snap.Name] = true
		}
	}

	api, err := d.api.API()
	if err != nil {
		return nil, err
	}
	listedVMs, err := api.ListVms(ctx, &boxdapi.ListVmsRequest{})
	if err != nil {
		return nil, fmt.Errorf("boxd: list machines: %w", err)
	}
	listedSnapshots, err := api.ListSnapshots(ctx, &boxdapi.ListSnapshotsRequest{})
	if err != nil {
		return nil, fmt.Errorf("boxd: list snapshots: %w", err)
	}
	var out []machine.Orphan
	for _, vm := range listedVMs.GetVms() {
		if machine.ResourceProject(namePrefix, vm.GetName()) != project || vms[vm.GetVmId()] || vms[vm.GetName()] {
			continue
		}
		out = append(out, machine.Orphan{
			Kind: machine.OrphanMachine, Name: vm.GetName(), ID: vm.GetVmId(),
			Status: vm.GetStatus(), Created: unixTime(vm.GetCreatedAt(), time.Second),
		})
	}
	for _, snap := range listedSnapshots.GetSnapshots() {
		if machine.ResourceProject(namePrefix, snap.GetName()) != project || snapshots[snap.GetName()] {
			continue
		}
		out = append(out, machine.Orphan{
			Kind: machine.OrphanLayer, Name: snap.GetName(), ID: snap.GetSnapshotId(),
			Status: snap.GetStatus(), Created: unixTime(snap.GetCreatedAt(), time.Millisecond),
		})
	}
	slices.SortFunc(out, func(a, b machine.Orphan) int { return a.Created.Compare(b.Created) })
	return out, nil
}

// RemoveOrphan destroys an orphaned machine, or deletes an orphaned snapshot.
func (d *Driver) RemoveOrphan(ctx context.Context, orphan machine.Orphan) error {
	switch orphan.Kind {
	case machine.OrphanMachine:
		api, err := d.api.API()
		if err != nil {
			return err
		}
		if _, err := api.DestroyVm(ctx, &boxdapi.DestroyVmRequest{VmId: orphan.ID}); err != nil && !isNotFound(err) {
			return fmt.Errorf("boxd: destroy %s: %w", orphan.Name, err)
		}
		return nil
	case machine.OrphanLayer:
		return d.deleteSnapshot(ctx, orphan.Name)
	default:
		return fmt.Errorf("boxd: orphan %s is a %q", orphan.Name, orphan.Kind)
	}
}

// unixTime reads one of boxd's times, counted in unit since the epoch: a
// machine's created_at is in seconds, and a snapshot's in milliseconds.
func unixTime(n int64, unit time.Duration) time.Time {
	if n <= 0 {
		return time.Time{}
	}
	return time.Unix(0, 0).Add(time.Duration(n) * unit)
}
