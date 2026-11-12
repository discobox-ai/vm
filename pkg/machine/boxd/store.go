package boxd

import (
	"context"
	"fmt"
	"slices"
	"strings"
	"time"

	"github.com/discobox-ai/vm/pkg/machine"
	"github.com/discobox-ai/vm/pkg/machine/boxd/internal/boxdapi"
)

// namePrefix starts the name of every machine and snapshot the driver makes,
// so that the caller's other machines and snapshots are never taken for the
// driver's. The pool follows it.
const namePrefix = "dvm-"

// unixTime reads one of boxd's times, counted in unit since the epoch: a
// machine's created_at is in seconds, and a snapshot's in milliseconds.
func unixTime(n int64, unit time.Duration) time.Time {
	if n <= 0 {
		return time.Time{}
	}
	return time.Unix(0, 0).Add(time.Duration(n) * unit)
}

// snapshots lists the caller's snapshots.
func (d *Driver) snapshots(ctx context.Context, api boxdapi.BoxdApiClient) ([]*boxdapi.SnapshotInfo, error) {
	listed, err := api.ListSnapshots(ctx, &boxdapi.ListSnapshotsRequest{})
	if err != nil {
		return nil, fmt.Errorf("boxd: list snapshots: %w", err)
	}
	return listed.GetSnapshots(), nil
}

// instanceName is the VM a machine or snapshot name is of, in the driver's
// pool: `dvm-<pool>--<name>`, where name is a VM name
// (machine.ValidateName). What disco-vm made before ADR 26-11-03-606 (an
// image's snapshot, named for its 64-digit ID, and a tag's, with "--" in its
// name) is not one, and is not read.
func (d *Driver) instanceName(name string) (string, bool) {
	rest, ok := strings.CutPrefix(name, d.prefix())
	if !ok || machine.ValidateName(rest) != nil {
		return "", false
	}
	return rest, true
}

// instance is a machine as a VM. boxd says which image a machine was made
// from, but not which snapshot, so a VM created from a committed one has no
// From.
func (d *Driver) instance(vm *boxdapi.GetVmResponse) machine.Instance {
	name, _ := d.instanceName(vm.GetName())
	return machine.Instance{
		ID:       vm.GetVmId(),
		Name:     name,
		Pool:     d.pool,
		GuestOS:  machine.Linux,
		CPUs:     int(vm.GetVcpu()),
		Memory:   vm.GetMemoryBytes(),
		Created:  unixTime(vm.GetCreatedAt(), time.Second),
		Isolated: vm.GetIsolated(),
		Networks: vm.GetNetworks(),
		Egress:   egressOf(vm.GetEgressAllow()),
	}
}

// committed is a ready snapshot as a committed VM, its ID the snapshot's.
// That it is committed is the caller's to record; the driver tells it from a
// machine by its kind (snapshotOf).
func (d *Driver) committed(snap *boxdapi.SnapshotInfo) machine.Instance {
	name, _ := d.instanceName(snap.GetName())
	return machine.Instance{
		ID:      snap.GetSnapshotId(),
		Name:    name,
		Pool:    d.pool,
		GuestOS: machine.Linux,
		CPUs:    int(snap.GetVcpu()),
		Memory:  snap.GetMemoryBytes(),
		Created: unixTime(snap.GetCreatedAt(), time.Millisecond),
	}
}

// snapshotOf is the committed VM's snapshot when inst is one: the ready
// snapshot of its name whose ID is inst's. It is nil for a machine.
func (d *Driver) snapshotOf(ctx context.Context, api boxdapi.BoxdApiClient, inst machine.Instance) (*boxdapi.SnapshotInfo, error) {
	got, err := api.GetSnapshot(ctx, &boxdapi.GetSnapshotRequest{Name: d.machineName(inst.Name)})
	if isNotFound(err) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("boxd: snapshot %s: %w", d.machineName(inst.Name), err)
	}
	snap := got.GetSnapshot()
	if snap.GetStatus() != "ready" || snap.GetSnapshotId() != inst.ID {
		return nil, nil
	}
	return snap, nil
}

// refuseCommitted is ErrCommitted when inst is a committed VM, which boxd
// keeps as a snapshot with no machine to run, and err otherwise: why a
// machine call on inst's ID failed.
func (d *Driver) refuseCommitted(ctx context.Context, api boxdapi.BoxdApiClient, inst machine.Instance, err error) error {
	if snap, _ := d.snapshotOf(ctx, api, inst); snap != nil {
		return fmt.Errorf("boxd: VM %s is a snapshot: %w", inst.Name, machine.ErrCommitted)
	}
	return err
}

// Instances lists the pool's VMs: its machines, then its ready snapshots
// as committed VMs. A snapshot that shares its name with a machine is a
// Commit that has not deleted the machine yet: it is listed, committed, and
// the machine is not, and Destroy removes both.
func (d *Driver) Instances(ctx context.Context) ([]machine.Instance, error) {
	api, err := d.api.API(ctx)
	if err != nil {
		return nil, err
	}
	listed, err := api.ListVms(ctx, &boxdapi.ListVmsRequest{})
	if err != nil {
		return nil, fmt.Errorf("boxd: list machines: %w", err)
	}
	snaps, err := d.snapshots(ctx, api)
	if err != nil {
		return nil, err
	}
	var out []machine.Instance
	committed := map[string]bool{}
	for _, snap := range snaps {
		if _, ok := d.instanceName(snap.GetName()); ok && snap.GetStatus() == "ready" {
			out = append(out, d.committed(snap))
			committed[snap.GetName()] = true
		}
	}
	for _, vm := range listed.GetVms() {
		if _, ok := d.instanceName(vm.GetName()); ok && vm.GetStatus() != "destroying" && !committed[vm.GetName()] {
			out = append(out, d.instance(vm))
		}
	}
	slices.SortFunc(out, func(a, b machine.Instance) int { return a.Created.Compare(b.Created) })
	return out, nil
}

// Instance is the pool's VM of the name, committed or not.
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
	return machine.Instance{}, fmt.Errorf("VM %q: %w", name, machine.ErrNotFound)
}
