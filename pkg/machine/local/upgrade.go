package local

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"

	"github.com/discobox-ai/vm/pkg/machine"
)

// An upgrade gives a VM a new disk beside its old one, in its own directory
// (ADR 26-11-03-782):
//
//	instances/<id>/machine.next/  the new disk, which Prepare makes with no volumes
//	instances/<id>/next.ready     written once that disk is whole
//	instances/<id>/machine.old/   the old disk, once the new one is in place
//
// Its record names the root it is given (Upgrading) before any disk is
// touched, and drops it last. Each step can be taken again: a new disk not
// yet whole is made again, volumes are moved by a hypervisor that keeps what
// has moved (MoveVolumes), and the disks are swapped by two renames that a
// second call finishes. The VM keeps its ID, so it is the same directory and
// the same name's claim throughout, and nothing else is ever listed.
const (
	nextDir   = machineDir + ".next"
	oldDir    = machineDir + ".old"
	nextReady = "next.ready"
)

// upgradeTarget is the root an upgrade gives a VM: a copy of the committed
// VM FromID, or a fresh one from the base where FromID is empty.
type upgradeTarget struct {
	From   string `json:"from,omitempty"`
	FromID string `json:"fromID,omitempty"`
}

// whole refuses a VM caught between two roots by an interrupted upgrade.
func (rec instanceRecord) whole() error {
	if rec.Upgrading == nil {
		return nil
	}
	return fmt.Errorf("instance %s: an upgrade of it is unfinished; run upgrade again%s to finish it, or remove it", rec.Name, rec.Upgrading.flag())
}

// flag is how the command line names the target, for an error.
func (t upgradeTarget) flag() string {
	if t.From == "" {
		return ""
	}
	return " --from " + t.From
}

// upgradeSpec is the hypervisor's view of the VM's disk in dir (nextDir or
// oldDir) with no volumes: what Prepare makes and Destroy removes there.
// chain is the new disk's, for Prepare.
func (d *Driver) upgradeSpec(rec instanceRecord, dir string, chain []Layer) InstanceSpec {
	return InstanceSpec{
		ID: rec.ID, Dir: filepath.Join(d.instanceDir(rec.ID), dir), GuestOS: rec.GuestOS,
		CacheDir: d.cacheDir(), Egress: rec.Egress, Chain: chain,
	}
}

// Upgrade gives the stopped VM a new disk, a copy of the committed VM from or
// a fresh one from the hypervisor's base, moves its volumes onto it, and
// removes its old disk. An upgrade interrupted at any step is finished by
// calling it again with the same from.
func (d *Driver) Upgrade(ctx context.Context, inst machine.Instance, from *machine.Instance) (machine.Instance, error) {
	if err := d.authorizeInstance(ctx, inst); err != nil {
		return machine.Instance{}, err
	}
	if !d.hv.Capabilities().Attaches(machine.VolumeData) {
		return machine.Instance{}, fmt.Errorf("%s: an upgrade keeps a data volume, and %s attaches none: %w", d.hv.Name(), d.hv.Name(), machine.ErrUnsupported)
	}
	rec, err := d.readProject(inst.ID)
	if err != nil {
		return machine.Instance{}, err
	}
	if shim, err := d.openShim(rec.ID); err == nil && shim.status(ctx) == nil {
		return machine.Instance{}, fmt.Errorf("instance %s is running; stop it to upgrade it", rec.Name)
	}
	fromName := ""
	if from != nil {
		fromName = from.Name
	}
	var target upgradeTarget
	switch {
	case rec.Upgrading != nil && rec.Upgrading.From == fromName:
		// Finishing an upgrade: the committed VM it is from is read only if
		// its new disk is still to be made, so one removed since does not
		// keep it from finishing.
		target = *rec.Upgrading
	case from == nil:
		guestOS, ok := d.hv.Base()
		if !ok {
			return machine.Instance{}, fmt.Errorf("%s has no base to give a VM a fresh root from; upgrade it from a committed VM: %w", d.hv.Name(), machine.ErrUnsupported)
		}
		if guestOS != rec.GuestOS {
			return machine.Instance{}, fmt.Errorf("instance %s is a %s guest, and %s's base is %s: its data volume is %s's", rec.Name, rec.GuestOS, d.hv.Name(), guestOS, rec.GuestOS)
		}
	default:
		src, err := d.readProject(from.ID)
		if errors.Is(err, machine.ErrNotFound) || (err == nil && src.Name != from.Name) {
			err = fmt.Errorf("instance %s: %w", from.Name, machine.ErrNotFound)
		}
		if err != nil {
			return machine.Instance{}, err
		}
		if src.ID == rec.ID {
			return machine.Instance{}, fmt.Errorf("instance %s cannot be upgraded from itself", rec.Name)
		}
		if shim, err := d.openShim(src.ID); err == nil && shim.status(ctx) == nil {
			return machine.Instance{}, fmt.Errorf("instance %s is running; stop and commit it to upgrade from it", src.Name)
		}
		if src.GuestOS != rec.GuestOS {
			return machine.Instance{}, fmt.Errorf("instance %s is a %s guest, and %s a %s one: its data volume is %s's", rec.Name, rec.GuestOS, src.Name, src.GuestOS, rec.GuestOS)
		}
		target = upgradeTarget{From: src.Name, FromID: src.ID}
	}
	if rec.Upgrading != nil && *rec.Upgrading != target {
		// Until the new disk is whole the old one and its volumes are
		// untouched, so another root may take the recorded one's place
		// (its committed VM may be gone); after, only it finishes.
		if _, err := os.Stat(filepath.Join(d.instanceDir(rec.ID), nextReady)); !errors.Is(err, fs.ErrNotExist) {
			return machine.Instance{}, rec.whole()
		}
		rec.Upgrading = nil
	}
	if rec.Upgrading == nil {
		rec.Upgrading = &target
		if err := d.writeRecord(rec); err != nil {
			return machine.Instance{}, err
		}
	}
	if err := d.upgrade(ctx, rec, target); err != nil {
		return machine.Instance{}, fmt.Errorf("upgrade %s: %w", rec.Name, err)
	}
	rec.From, rec.FromID, rec.Upgrading = target.From, target.FromID, nil
	if err := d.writeRecord(rec); err != nil {
		return machine.Instance{}, err
	}
	return rec.Instance, nil
}

// upgrade takes each step of the upgrade not yet taken: make the new disk,
// move the volumes onto it, swap it in, and remove the old one.
func (d *Driver) upgrade(ctx context.Context, rec instanceRecord, target upgradeTarget) error {
	dir := d.instanceDir(rec.ID)
	exists := func(name string) bool {
		_, err := os.Lstat(filepath.Join(dir, name))
		return err == nil
	}
	next := d.upgradeSpec(rec, nextDir, nil)
	next.Data, next.Cache = rec.Data, rec.Cache
	// next.ready is written once the new disk is whole and removed last, so
	// with it and no new disk the swap is done.
	if ready := exists(nextReady); !ready || exists(nextDir) {
		if !ready {
			var chain []Layer
			if target.FromID != "" {
				var err error
				if chain, err = d.chain(target.FromID); err != nil {
					return err
				}
			}
			// A new disk an interrupted upgrade began holds no volume yet.
			bare := d.upgradeSpec(rec, nextDir, chain)
			if err := d.hv.Destroy(ctx, bare); err != nil {
				return err
			}
			if err := d.hv.Prepare(ctx, bare); err != nil {
				return err
			}
			if err := os.WriteFile(filepath.Join(dir, nextReady), nil, 0o600); err != nil {
				return err
			}
		}
		if exists(machineDir) {
			old, err := d.spec(rec)
			if err != nil {
				// A chain removed behind the driver's back: the old disk's
				// volumes are moved all the same.
				old = InstanceSpec{ID: rec.ID, Dir: d.machineDir(rec.ID), GuestOS: rec.GuestOS}
			}
			old.Data, old.Cache = rec.Data, rec.Cache
			if err := d.hv.MoveVolumes(ctx, old, next); err != nil {
				return err
			}
			if err := os.Rename(d.machineDir(rec.ID), filepath.Join(dir, oldDir)); err != nil {
				return err
			}
		}
		if err := os.Rename(filepath.Join(dir, nextDir), d.machineDir(rec.ID)); err != nil {
			return err
		}
	}
	if exists(oldDir) {
		if err := d.hv.Destroy(ctx, d.upgradeSpec(rec, oldDir, nil)); err != nil {
			return err
		}
		if err := os.RemoveAll(filepath.Join(dir, oldDir)); err != nil {
			return err
		}
	}
	if err := os.Remove(filepath.Join(dir, nextReady)); err != nil && !errors.Is(err, fs.ErrNotExist) {
		return err
	}
	if !exists(machineDir) {
		return errors.New("its disk is gone")
	}
	return nil
}
