# 26-10-23-410 — Images are snapshots and stand alone; there is no build

- **Status**: Accepted (supersedes in part 26-10-15-851, its images made by
  `build`; 0002, its docker base layer installed and shifted once; and
  26-10-23-417, its layers as a build's cache)
- **Date**: 2026-10-23

## Context

An image is made two ways: `disco-vm build` runs a YAML spec of layers, each
a booted guest's steps committed from an orderly shutdown and kept under the
key of its inputs, and `disco-vm snapshot` commits a stopped instance
(26-10-15-882). A client that shapes a guest already does it the second way:
exec, copy, stop, snapshot (26-10-15-703). Since 26-10-23-417 a driver is the
store, and the build is what most of its contract is for: a layer's ID is a
cache key, a layer has a parent, `local` records each layer's parent
generation so that a lost parent rebuilds its children, `images -a` lists a
chain's middle, `rmi` walks up a chain, and an instance can be a build's
temporary one. Only hcs's disks need a parent to boot: a differencing VHDX
over its parent's. vz's APFS clones, fake's trees, boxd's snapshots, and
docker's committed images are whole.

A build is also how hcs, vz, and docker make their first image, through
`from.install`. boxd needs none: its images and snapshots are bases
(26-10-15-833).

## Decision

Remove the build. An image is a snapshot of a stopped instance, or an install
from media, and every image stands alone above the seam.

- **The seam has images, not layers.** `machine.Layer` is `machine.Image`
  (ID, guest OS, comment, created; no parent), and its ID is random.
  `Layer`, `Layers`, and `DeleteLayer` are `Image`, `Images`, and
  `DeleteImage`; `InstanceSpec.Layer` and `Instance.Layer` are `ImageID`;
  `Tag.Layer` is `Tag.Image`. `TemporaryPrefix`, `Instance.Temporary`, and
  `Base.Version`, which only a build read, go.
- **`rmi` deletes one image**, once nothing tags or runs it, and touches no
  other.
- **A driver whose disks chain keeps the chain below the seam.**
  `local.Hypervisor.Chains` says so (hcs). `local` then records an image's
  parent beside its disk, hands the hypervisor the whole chain as before, and
  refuses `DeleteImage` with `machine.ErrInUse` while another image's record,
  in any project, names it as its parent. `rmi` of its last tag leaves it
  untagged and says why. The per-layer generations go: nothing rebuilds a
  child.
- **`disco-vm install` makes a first image** on a driver that lists
  `Capabilities.Install` (hcs, vz, fake), from flags in place of
  `from.install`. boxd and docker refuse it.
- **docker's bases are registry images.** `Driver.Base(ref)` finds a base by
  name or ID, so a driver can have bases it cannot list; docker's is any image
  the daemon has or its registry answers for (`/distribution`; only a 401,
  403, or 404 means it has none), found without a pull. Create of an instance
  of one pulls it, adds systemd and the agent, shifts it, and commits that as
  the instance's own Docker image, which Destroy removes. The agent is
  `$DISCO_VM_AGENT`, or this binary on linux.
- **`build`, `pkg/build`, `images -a`, `docs/build-spec.md`, and
  `docs/examples/` go.**

## Alternatives rejected

- **Keep the build, as a client of snapshot.** A spec could run as exec,
  copy, stop, and snapshot, with no layers of its own. It would still be
  disco-vm deciding what an image holds, which 26-10-15-703 gives to a
  client, and a second way to make an image beside the one every client
  uses. Its cache is the only thing it adds, and a snapshot is not
  reproducible, so the cache cannot be kept honestly.
- **Drop the build and keep layers.** A snapshot's parent would stay on the
  seam, so `rmi` would still refuse an image others are built on, and
  `images` would still hide a chain's middle. Only hcs's disks need the
  parent, so it is hcs's to keep.
- **hcs flattens every snapshot into a disk of its own.** That copies a
  Windows guest's whole disk, tens of GB, on every snapshot.
- **hcs reads a disk's parent from the VHDX** (virtdisk's parent location)
  instead of a record. It was this ADR's first build, before 26-10-23-417,
  when nothing below the seam kept a record; `local` keeps one now, and a
  record needs no long-path comparison against the store's paths.
- **docker keeps an install that commits a registry image, readied, as an
  image.** It would be the one driver whose first image is neither a base
  nor an install from media. As a base, any image runs by name, as boxd's
  do.
- **Remove install too.** hcs and vz have no other way to a first image.

## Consequences

- A client shapes every image the same way: run, exec and copy, stop,
  snapshot. Nothing is cached; an image that is wanted again is kept.
- Removing an image never breaks another, above the seam. On hcs a snapshot
  keeps the disks under it until it goes, so a deep chain of snapshots costs
  the disk of every one, and `rmi` says which image holds one.
- Every docker instance of a registry image readies it again at create: 30 s
  to 2 minutes when the image has no systemd. A snapshot of the instance does
  not.
- Nothing from before is migrated, as 26-10-23-417 decided, and nothing
  needs to be: a local layer's record is read as an image's, its parent kept
  only on a hypervisor that chains and its generations ignored, and a local
  tag's or instance's `layer` is read as its image; docker's labels and
  boxd's names keep the keys they had.

## Deferred

- **docker readying cached per image digest and agent**, when the time a
  create takes from a registry image matters to a client.
- **hcs merging a snapshot's chain** (virtdisk can merge a child into its
  parent), when a chain's disk use matters.
