# 26-11-28-860 — A new archil VM keeps its data on its root unless its driver is configured for data disks, and a VM with one keeps it

- **Status**: Accepted (supersedes in part 26-11-03-782 §2, its upgrade
  refused on any driver that does not list `VolumeData`, and its archil row
  in §5; and 26-11-24-905 §6, a VM created from a committed one being stopped
  and started with its data disk mounted)
- **Date**: 2026-11-28

Accepted by the owner by merging #241, 2026-11-28.

## Context

ADR 26-11-03-782 gives a VM a data volume whose only purpose is to let the VM
be upgraded to a new root. On archil that volume is a disk of its own,
`dvm-<pool>--<name>--data`, which the driver makes at `Create` and mounts at
every start (`pkg/machine/archil/archil.go`, `Create` and `Start`).
`Capabilities` lists `VolumeData` unconditionally (`archil.go`,
`Capabilities`). discobox's pool asks for a data volume whenever a driver
lists one (`pool-agent/sandboxruntime/machine.go`, `ensureInstance`), and
gives a machine a new root only when it keeps its data there
(`keepsDataOnVolume`).

Archil sets a sandbox's disks only when it is created or started. A fork
takes no mounts (ADR 26-11-24-905, Context: checked against Archil 2026-11-24).
Archil's own reference confirms it (docs.archil.com, Fork Sandbox and
Start Sandbox, read 2026-11-27): a fork's body is `name` and `checkpoint`
alone, and `mounts` on start "requires an inactive sandbox". A fork of a
stopped source always cold-boots, and there is no way to make one stopped.
So a VM created from a committed one with a data volume boots twice:
the fork boots without the disk, is stopped, and is started again with it
(ADR 26-11-24-905 §6). That is every discobox box made from a snapshot.

A fork branches the source's root disk: each fork gets its own writable copy
at once (a checkpoint about 15 ms, a branch about 35 ms, docs.archil.com,
Branches & Checkpoints). A VM whose data is on its root therefore needs no
second boot. A copy of a committed VM is one fork; a fresh VM is one
create.

What a data disk buys is §2 of ADR 26-11-03-782: an upgrade gives the VM a new
root and keeps its data. On archil a new root is a new sandbox, and the old
one, root disk included, is deleted (ADR 26-11-24-905 §4). A VM whose data is on
its root can't be given one without losing that data.

discobox's pool already rebuilds a machine without a data volume in place: a
new bootstrap on the same disk, and a restart (pool-agent `DESIGN.md`, "the
machine does not keep its data on its data volume"). boxd, which has no
data disk, is rebuilt that way today. A bundle upgrade never gives a new
root on any driver: it rewrites what the pool places in the machine
(`agentlayer.Archive`, the box's `pins.json`) and restarts it.

The owner chose, 2026-11-28: archil VMs keep their data on the root by
default, so a box boots once. A data disk stays an option. Existing VMs that
have one keep it, and keep being upgraded with it.

## Decision

### 1. The archil driver has a `dataDisk` setting, off by default

The archil driver's configuration (`Config`, ADR 26-11-11-215) gains a
boolean field:

| key | type | default | meaning |
|---|---|---|---|
| `dataDisk` | boolean | false | New VMs get a data disk, mounted at `VolumePath(Linux, VolumeData)` |

- **Off (the default):** `Capabilities.Volumes` lists no volume, so a
  caller asks for none (`InstanceSpec.Data` is refused by the engine's
  `checkVolumes`, as on boxd). A new VM keeps everything on its root disk,
  which a fork branches.
- **On:** the driver behaves as today. `Capabilities.Volumes` lists
  `VolumeData`, and a VM made with one has its disk.

The setting decides only what a new VM is made with. It never adds or
removes a disk on a VM that exists.

### 2. A VM that has a data disk keeps it, whatever the setting

Whether a VM has a data disk is the VM's, read from Archil, as today: the
disk of its name (`dataDisk`), reported as `Instance.Data`. With `dataDisk`
off, the driver still:

- mounts a VM's data disk at every start;
- gives such a VM a new root with its disk (`Upgrade`, ADR 26-11-24-905 §4);
- removes the disk with the VM (`Destroy`).

So a VM made before this, or while the setting was on, runs, upgrades and is
destroyed exactly as it does today. Turning the setting off changes no VM
that exists, and turning it on changes none either.

### 3. Upgrade is gated on the VM, not on the driver

ADR 26-11-03-782 §2 refuses any upgrade on a driver that does not list
`VolumeData`. That would refuse an upgrade to every existing archil VM once
the setting is off. It becomes:

- `Engine.Upgrade` refuses (`ErrUnsupported`) a VM that has no data volume
  on a driver that does not list `VolumeData`. A VM that has one
  (`Instance.Data` set) may be upgraded whatever the driver lists now.
- The archil driver refuses (`ErrUnsupported`) to upgrade a VM that has no
  data disk, whatever the setting: its root is all it has, and a new root
  would delete it. This narrows ADR 26-11-03-782 §2's "a VM with no data volume, on a
  driver that lists `VolumeData`, may be upgraded too" for archil. Other
  drivers keep that rule.

A caller that gets `ErrUnsupported` rebuilds the VM in place, as the
engine's error already tells it to ("rebuild the VM in place instead",
`pkg/engine/engine.go`, `Upgrade`).

### 4. A VM from a committed one, with no data disk, boots once

A VM created from a committed one and asked to start, with no data volume,
is the fork alone: it boots once, and is never stopped to be started again.
ADR 26-11-24-905 §6's stop and start remain only for a copy asked for a data
volume (the setting on). A fresh VM asked to start with no data volume was
already one start (ADR 26-11-23-458 §2), and stays one.

### 5. What a caller sees

| | `dataDisk` off (default) | `dataDisk` on |
|---|---|---|
| `Capabilities.Volumes` | none | `VolumeData` |
| a new VM | data on its root | a data disk, as today |
| a copy of a committed VM | one boot | fork, stop, start with its disk |
| an existing VM with a data disk | mounted, upgraded, destroyed with it | the same |
| `Upgrade` of a VM with no data disk | `ErrUnsupported` | `ErrUnsupported` |

The conformance suite runs archil both ways.

## Alternatives rejected

- **Leave disco-vm as it is, and have discobox ask for no data volume on
  archil.** That works today with no change here, but it puts a driver's
  storage layout in its caller, which is the boundary disco-vm owns
  (discobox keeps per-driver logic out of its pool). It also leaves
  `Upgrade` willing to delete an archil VM's root data if any caller asks.
- **Off means the driver ignores `InstanceSpec.Data`.** A capability that
  is listed and then not honoured makes a caller believe its data is on a
  volume it doesn't have, and a later upgrade would delete it.
- **Move existing VMs' data onto their roots.** It needs a copy from the
  disk into each running VM's root, then the disk's removal, for a VM that
  works as it is. The owner chose to leave them.
- **A data disk created later, on the first upgrade.** An upgrade is when
  the data is needed somewhere other than the root, and by then it's on the
  root, which the upgrade deletes.
- **Make the old root the new root's data disk.** Archil's root disk is
  internal: it can't be mounted in another sandbox ("A sandbox's internal
  root disk cannot be used here", Create Sandbox, `mounts`), and a sandbox
  can't be created on an existing disk.
- **Ask Archil to take `mounts` on a fork.** It would make a copy with a
  data disk one boot too. It is not in our hands, and doesn't remove the
  second disk per VM. Worth asking for, independently.

## Consequences

- A discobox box on archil made from a snapshot boots once, as a fresh one
  does. Its root is a branch of the snapshot's, so the box has its own
  copy, isolated, at once.
- A new archil VM can't be given a new root. discobox's pool rebuilds it in
  place, as it does on boxd: a spec change or a project-layer change keeps
  its root and its base image, and a newer base reaches only new VMs. Bundle
  upgrades are unchanged.
- discobox's pool must read a machine's own data volume, not the driver's
  capability, to keep giving existing archil VMs new roots:
  `keepsDataOnVolume` (`pool-agent/sandboxruntime/machine.go`) checks
  `record.DataOnVolume` and `inst.Data` and drops its
  `Capabilities().Attaches(VolumeData)` term. Without that change, existing
  VMs are rebuilt in place instead, which keeps their data (the disk is
  still mounted) but not a new root. That change is discobox's, in its own
  issue.
- `engine.Upgrade`'s gate, `Create`'s data-disk paths, and the conformance
  suite change with the implementation, as do `docs/drivers/archil.md`,
  `pkg/machine/archil/DESIGN.md`, and `DESIGN.md`'s proof table.
- Archil's root disk is the VM's whole storage, so its limits are the VM's.
  Archil documents no size limit for a root that differs from a disk's; the
  implementation checks it.

## Deferred

- **The default for an existing provider whose VMs all have data disks.**
  It is the same off: new VMs in such a pool get none, and the pool holds
  both kinds until the old ones are deleted. Revisit if a pool's mixed
  machines turn out to matter to an operator.
