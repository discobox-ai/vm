# 26-11-03-606 — disco-vm has only VMs, and a VM is made fresh or from a committed VM

- **Status**: Accepted (supersedes 26-10-15-882; and in part 26-10-23-410,
  its images and tags, and 26-10-15-833, an instance run from a base the
  caller names)
- **Date**: 2026-11-03
- **Supersedes**: [26-10-15-882](26-10-15-882-a-snapshot-consumes-its-instance.md)

## Context

disco-vm has two kinds of thing today:

- **Instances:** machines that run.
- **Images:** what instances are made from.

Images bring tags, `rmi`, and image IDs. A snapshot consumes its instance
and makes an image under a new ID (26-10-15-882, 26-10-23-410). An instance
can also run straight from a driver's base, which the caller names by
reference and `images` lists (26-10-15-833).

Every driver keeps both kinds, and the mapping between them, in its
provider. That costs most where the provider has least:

- **boxd** keeps a tag as a second snapshot.
- **Archil** can't relabel or rename a sandbox, so turning an instance into
  an image needs records kept somewhere else: empty disks named for them,
  whose names Archil does not keep unique (ADR 26-11-03-676, unshipped).
- **docker** keeps images, tags, and readied bases apart by label and tag.

What callers do with all this is narrower. discobox creates a sandbox,
shapes it, freezes it, and creates more like it. It names what it froze,
and keeps its own pointer to the one it uses now. It never needs a tag or
an image ID from disco-vm. Nor does it need to choose among a provider's
bases: what a fresh machine starts from is a fact about the provider and
how it is configured.

## Decision

### 1. There are only VMs

disco-vm has one kind of thing, a **VM**, named in its project
(`machine.ValidateName`, unique across every VM, committed or not). A VM is
made in one of three ways:

- **Fresh:** `create NAME` (and `run NAME`), from the driver's base.
- **From a committed VM:** `create NAME --from VM`, a copy of that VM's
  disk.
- **By installing:** `install NAME`, on a driver that installs from media
  (§3).

There are no images, image IDs, tags, `tag`, `rmi`, or `images`.

### 2. Commit freezes a VM, under its name

`commit VM` freezes a stopped VM. A committed VM:

- keeps its name, and `ls -a` lists it;
- can be created from, and removed;
- is never started again, committed again, or exec'd into;
- holds only its disk. Its data volume and cache clone go at commit, as
  ADR 26-11-02-180 has them leave an image.

**That a VM is committed is the caller's to record.** Archil keeps a
committed sandbox as a stopped one, so a driver cannot always tell, and the
seam reports no committed mark. The caller (discobox, or the person at the
command line) knows which VMs it committed and never starts one again. A
driver refuses a committed VM only where its provider cannot do what was
asked: docker's committed image and boxd's snapshot have no machine to run
(`machine.ErrCommitted`), and hcs boots no VM another VM's disk differences
over, since the boot would write under it (`machine.ErrInUse`). Archil
refuses nothing, and keeps no record that it should.

To change a committed VM, create a VM from it, change that, and commit it
under a new name. There are no tags, so versions are names (`app-v1`,
`app-v2`). A caller that wants a "current" one keeps its own pointer.

**Removing a committed VM.** On a driver whose VMs stand alone (docker,
boxd), removing one takes nothing from the VMs created from it. On a driver
whose copies chain below the seam, the removal is refused with
`machine.ErrInUse` while a VM created from it exists:

- hcs: a differencing disk over the committed VM's;
- Archil: a fork, which pins its source.

### 3. What a fresh VM starts from is the driver's, not the caller's

A fresh VM starts from the driver's **base**. The base is the driver's own
configuration, or its provider's default, and the caller never names it:

- **docker:** a configured image;
- **Archil:** a configured OCI image, or Archil's default;
- **boxd:** its default image;
- **vz:** its Linux guest image;
- **fake:** the catalog a test gives it.

A driver may need to build a VM from an OCI image, a provider snapshot, or
an install; how it does that is below the seam. There is no `Bases`,
`Base`, or listing of bases. A driver with no base (hcs, and vz for macOS)
refuses a fresh VM. Its VMs come from an install or from a committed VM.

**Install** (`install NAME --media ...`, on a driver that lists
`Capabilities.Install`) runs the install and leaves a committed VM of that
name. It is the one way besides commit that a committed VM comes to be.

### 4. The seam

`machine.Driver` keeps what it has for VMs: `Create`, `Instance`,
`Instances`, `Destroy`, `Boot`, `Start`, `Attach`, `Commit`, `Service`,
keys, and caches. It loses `Image`, `Images`, `DeleteImage`, `Tags`, `Tag`,
`Untag`, `Bases`, and `Base`. The VM-shaped types change as follows:

- **`InstanceSpec`** loses `Image`, `ImageID`, and `Base`. It gains `From`,
  the committed VM a copy is made from; nil is fresh.
- **`machine.Instance`** loses `Image`, `ImageID`, and `Base`. It gains
  `From` (the committed VM it came from, where the provider says), and no
  committed mark (§2).
- **`machine.Config`** gains `Base`, the driver's base in its own terms
  (§3); the command line reads it from `$DISCO_VM_BASE`, never a flag of a
  create.
- **`Commit(ctx, inst)`** freezes the stopped VM in place and returns it.
- **`Install(ctx, spec, name)`** returns the committed VM it made.
- **`Boot` and `Start`** refuse a committed VM only where §2 says,
  with `ErrCommitted` or `ErrInUse`.
- **`Destroy`** removes a committed VM too, or refuses it with `ErrInUse`
  (§2).

The engine and the command line follow:

- **The engine** drops `Resolve`, `Images`, `RemoveImage`, `Tag`,
  `ResolveBase`, and `Snapshot`, and gains `Commit`.
- **The command line** drops `images`, `rmi`, `tag`, and `snapshot`, and
  gains `commit`. `create` and `run` take `--from`; `install` takes a name;
  `ls -a` lists committed VMs among the stopped ones.

### 5. Where each driver keeps a committed VM

Every driver keeps it in its provider, as before (ADR 26-10-23-417):

| driver | a committed VM | a VM from it |
|---|---|---|
| local (hcs, vz, fake) | the VM's record and its disk, left where they are, with its volumes gone | hcs: a differencing disk over it; vz: an APFS clone; fake: a copy |
| docker | the container committed as a Docker image named and labeled for the VM, and the container removed | a container of that image |
| boxd | a snapshot named for the VM, and the machine deleted | a machine restored from the snapshot |
| archil | the stopped sandbox, `dvm-<project>--<name>`, with its mounts dropped, and nothing recording that it is committed (§2) | a fork of the sandbox |

## Alternatives rejected

- **Keep images, and fix each driver's records.** Archil's records stay a
  separate set of disks whose names Archil does not keep unique. Every
  driver keeps a mapping from instance to image that no caller asked for.
- **Commit as a checkpoint the VM runs on from.** A committed VM that runs
  again would need its snapshot kept apart from it. An Archil fork's
  source can't move on, so the snapshot would be a hidden copy, which is
  the records problem again. Freezing is the same on every provider.
- **A committed mark the driver keeps.** Where a provider has no kind of
  its own for a committed VM (Archil), the mark would be a record kept
  beside it: an empty disk named for it, whose names Archil does not keep
  unique, which is the records problem again. The caller already knows
  what it committed.
- **Tags as aliases of committed VMs.** A tag is a record that moves, which
  every driver must keep somewhere it can change. Names are enough: a caller
  that wants a moving pointer keeps it.
- **A base the caller names** (`create NAME --base debian:trixie`). What a
  fresh machine starts from is the provider's and its configuration's. A
  caller that wants a particular start commits one and creates from it.
- **Install as the driver's base, filled on first use.** It hides a
  20-minute macOS install in a `create`, and leaves no name for what it
  made.

## Consequences

- **Every driver and the engine change, with the conformance suite and
  `internal/e2e`.** The suite's images become committed VMs: commit, create
  from one, and a copy of a copy outliving the first's removal (or refused
  with `ErrInUse`). No suite starts a committed VM, since the caller never
  does.
- **The archil driver lands on this model.** A VM is a sandbox named for it,
  so Archil keeps its name unique. A commit changes no sandbox's name, and
  leaves no record at all. ADR 26-11-03-676 is amended
  before it ships.
- **Nothing from before is read** (as ADR 26-10-23-417): images, tags, and
  readied bases a driver made under the old model are not listed. Remove
  them with `docker`, boxd's console, Archil's API, or the disco-vm that
  made them.
- **discobox** keeps committed VMs where it kept images, and records which
  VMs it committed. A pool names the
  committed VM its sandboxes are created from, and its warm run is
  `create --from`, the warm script, `stop`, and `commit`.
