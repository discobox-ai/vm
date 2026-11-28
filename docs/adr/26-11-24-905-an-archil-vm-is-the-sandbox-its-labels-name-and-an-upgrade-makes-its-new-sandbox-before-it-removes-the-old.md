# 26-11-24-905 — An archil VM is the sandbox its labels name, and an upgrade makes its new sandbox before it removes the old

- **Status**: Proposed (supersedes in part 26-11-03-676, its VM as the
  sandbox named for it; §6's VM created from a committed one being stopped
  and started with its data disk mounted, superseded by
  [26-11-28-860](26-11-28-860-a-new-archil-vm-keeps-its-data-on-its-root-unless-its-driver-is-configured-for-data-disks-and-a-vm-with-one-keeps-it.md))
- **Date**: 2026-11-24

## Context

ADR 26-11-03-676 made an archil VM "a sandbox named for it",
`dvm-<pool>--<name>`, which Archil keeps unique. The driver has since
labeled every sandbox it makes (`ai.discobox.vm.pool`, `.instance`,
`.from`) but kept the name as the VM, since a sandbox made before labels had
none and Archil sets labels only at create. Holding the name as the
identity is what makes the driver hard to follow:

- **An upgrade deletes the VM before it remakes it.** Archil can't rename a
  sandbox, so the new root takes the name only once the old sandbox is gone.
  An upgrade that stops in between leaves the VM as its data disk alone, so
  a data disk with no sandbox is a VM: listed with no size and the strictest
  fence, refused at Start, finished by another Upgrade. One with no data disk
  is simply gone, the one exception to ADR 26-11-03-782 §3. A new root that
  fails once it boots fails only after the old is deleted.
- **Migrations for VMs that no longer exist.** Every Start, Commit, Service,
  and copy unexposes 7302 for a VM made before ports were left unexposed; a
  listing reads names under the pool's prefix, labeled or not, and skips an
  earlier driver's `dvm-<pool>-<key>`.
- **A disk's ownership is guessed.** Archil answers a disk create with the
  disk of the name, so a create that makes its data disk first (ADR
  26-11-23-458 §7) could not tell its own disk from another's.

Checked on Archil itself (2026-11-24):

- **Sandboxes:** 117 live in us-west-2 and 6 in us-east-1. Every one is
  labeled with its pool and instance, and named `dvm-<pool>--<name>`.
  None exposes a port, and no data disk lacks its sandbox. eu-west-1 was
  not readable with the keys given.
- **Labels:** a sandbox takes labels at create and fork only. PATCH and PUT
  answer 405, and there is no labels endpoint. Repeated `label=k=v` filters
  on a listing are ANDed.
- **Disks:** a disk takes no labels. A `labels` field is dropped, and the
  `label` filter is ignored. A disk create answers 201 when it made the
  disk, and 200 with the existing disk of the name.
- **Forks:** a fork takes `name`, `labels`, and `checkpoint`. It ignores
  `mounts`, `network`, and `vcpu_count`, and boots with its source's
  mounts.
- **Deleting a disk:** Archil deletes a disk a running sandbox mounts. A
  stopped sandbox whose disk is gone then fails to start unless the start
  names its mounts again.

## Decision

### 1. A VM is the live sandbox its labels name

A VM of pool P named N is the live sandbox labeled `ai.discobox.vm.pool=P`
and `ai.discobox.vm.instance=N`. A pool is listed with
`label=ai.discobox.vm.pool=P`, and a VM looked up with both labels. A
sandbox's name is never read for what it is. A sandbox with neither label is
not a VM.

### 2. Names are for Archil's refusal and for people

- **Create** names the sandbox `dvm-<pool>--<name>`, as every existing
  VM's is: Archil refuses a second, so a race between two creates of a name
  is mostly refused by Archil, and the console reads as before.
- **An upgrade's new sandbox** is named
  `dvm-<pool>--<name, cut to fit>--<4 hex>`, made again with other hex where
  Archil refuses it. A VM name has no `--`, so it is never another VM's
  sandbox name.

### 3. One sandbox is the VM, whatever Archil holds

A sandbox labeled `ai.discobox.vm.replaces=<sandbox ID>` is an upgrade's new
root. While the sandbox it replaces is live, it is not the VM. Of the rest,
if more than one carries a VM's labels (two creates of a name that both
passed their checks, one after an upgrade freed `dvm-<pool>--<name>`), the
oldest is the VM, by creation time, then ID. A create that finds an older one
once its own is made removes its own and is `ErrExists`. Every listing,
lookup, and `Destroy` applies this one rule.

### 4. An upgrade makes the new root, then removes the old

`Upgrade` checks what it can, then makes the new sandbox (fresh, or a fork of
`from`) at the VM's size, fence, and ingress, labeled for the VM and
`replaces` the old, installs and settles systemd as a create does, and stops
it. Only then does it revoke the old sandbox's tokens and delete it. A
failure before the delete removes the new sandbox and leaves the VM as it
was, `ErrInUse` included (the old has forks). An upgrade that stops in
between leaves a replacement nothing lists. The next `Upgrade` or `Destroy`
of the VM removes it.

A data disk is never a VM, and there is no mid-upgrade state: a VM with no
data disk is no longer lost by a failed upgrade.

### 5. A create's data disk is made for it

The data disk keeps its name, `dvm-<pool>--<name>--data` (disks take no
labels). A create makes it only once it has found no sandbox of the VM but
its own, so a disk Archil answers 200 for, rather than 201, is left from a
VM removed midway: it is deleted and made again, so no VM sees another's
data. A create that loses its name to another (§3) leaves the disk: it may
be the winner's.

`Destroy` still deletes the sandbox before the data disk. Archil shows a
sandbox's forks only by refusing its delete, and a destroy refused with
`ErrInUse` must leave the VM, data and all. One stopped between the two
leaves a disk nothing lists, which the next create of the name replaces.

### 6. ADR 26-11-23-458 §7, settled

- **A fresh VM asked to start** has its data disk made first and mounted
  in its sandbox's create: one start. It uses only a disk Archil made for
  it (201): one already there may be a racing create's, mounted and
  written, so it is left, and the VM is made as §5 says and started once
  more with the disk mounted.
- **A VM created from a committed one, asked to start with a data volume,**
  is stopped and started once with it mounted, since Archil's fork takes no
  mounts.
- **Commit still starts a sandbox with mounts once with none,** and stops
  it, since a fork boots with its source's mounts.

### 7. What existing VMs need

Nothing. Every sandbox found in us-east-1 and us-west-2 already carries both
labels, under the name §2 gives a create. Removed with this decision:

- listing by name prefix, and the skip of an earlier driver's names;
- unexposing 7302 at every Start, Commit, Service, and copy;
- a data disk with no sandbox listed, refused, or upgraded as a VM.

A port is still never exposed, and a fork's tokens are still revoked.

## Alternatives rejected

- **Keep the name as the VM, and simplify around it.** The upgrade's
  delete-before-make, and so the mid-upgrade VM and the data disk read as a
  VM, are what the name forces; Archil can't rename a sandbox, so no
  simplification removes them.
- **A random name for every sandbox.** It drops Archil's atomic refusal for
  the common create, which §3's rule would then resolve every time, and the
  console's readable names, for nothing §1 needs.
- **Label the data disk, and find it by label.** Archil's disks take no
  labels.
- **Give a fork its mounts, saving a copy's restart and Commit's.**
  Archil's fork ignores `mounts`.
- **Keep the migrations.** No VM in the regions read needs them, and each
  runs on every Start, Commit, and Service of every VM.

## Consequences

- An interrupted or failed upgrade leaves the VM as it was. The archil
  exception to ADR 26-11-03-782 §3 is gone.
- A VM made by an earlier driver in a state none of the regions read
  showed is unsupported, and is not migrated: a sandbox made before labels
  is no longer listed (and billed until removed in Archil); a VM caught
  mid-upgrade, its data disk alone, is no longer listed, and the next create
  of its name makes that disk again; and a VM whose 7302 an earlier driver
  exposed stays exposed, since nothing unexposes it any more. None was seen
  in us-east-1 or us-west-2 (2026-11-24); eu-west-1, and accounts other
  than the one read, were not checked. The owner chose this: existing VMs
  are supported, and carry the labels the driver needs.
- A listing reads labels, and `Instances` applies §3's rule to what it reads.
- The fake API answers as Archil was seen to: a disk create's 201 and 200,
  a fork's ignored mounts, and a disk deleted while mounted.
- `docs/drivers/archil.md`, `pkg/machine/archil/DESIGN.md` and `REVIEW.md`,
  and `DESIGN.md`'s proof table change with the implementation.
