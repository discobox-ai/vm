# 26-10-23-417 — The engine is stateless, and a driver keeps its state in its provider

- **Status**: Accepted (supersedes in part 0004, its orphan listing; its layers as a build's cache superseded by 26-10-23-410)
- **Date**: 2026-10-23

## Context

disco-vm keeps two records of everything it makes. The provider holds the
thing itself: a boxd snapshot or machine, a Docker image or container, a VHDX
or an APFS bundle. And the engine holds a row for it in its own database
(`disco-vm.db`, or the embedder's): layers with their parent, guest OS, and
comment; tags; the layers built in place of lost ones; instances with their
name, image, size, and forwards. Files under the engine's root (`layers/`,
`instances/`, `shim.json`) tie the two together.

Most of the seam exists to keep those two records in step. `LayerExists` and
the replaced-layers table cope with a layer the provider lost behind the
store's back. `Known`, `Orphans`, and `RemoveOrphan` cope with the opposite,
something the provider holds that the store lost: a deleted root, a crash
between making and recording, another host's root. JSON import and
`AutoMigrate` upgrades exist because the rows are durable state of their own.
The two records still disagree whenever two hosts share a boxd account or a
Docker daemon: each sees the other's work as orphans, and neither shares the
other's build cache.

discobox already runs Docker this way, with no store of its own. Its pool
agent labels every sandbox container (`discobox.project_id`,
`discobox.pool_id`, `discobox.sandbox_id`), and `ListSandboxes` and
`GetSandbox` are a label-filtered `ContainerList`. An orphan there is not
something a local store lacks. It is something the provider holds that the
owner above no longer claims: `SyncKnownPools` removes containers of pools
the control plane did not name, `WatchSandboxVolumes` reaps trees the control
plane does not hold, and `imagereap` reclaims an image that carries the
reclaim label, that no container refers to, and that arrived longer ago than
a retention window. A failed lookup is never an empty set, and something just
made is spared by a grace period or a lock.

The providers' metadata is not equal. Docker has labels on images and
containers, fixed at creation. boxd has no labels on machines or snapshots:
only a name, unique per owner, and what `GetVm` and `SnapshotInfo` report
(status, size, `vcpu`, `memory_bytes`, `created_at`, a machine's
`image_ref`). A machine's name is a DNS label in its proxy's domain, 63
characters. hcs and vz have no service: the provider is the host's disk.

## Decision

**The engine keeps no state.** It holds no database, no root, and no files.
Everything disco-vm knows about a layer, a tag, or an instance is read from
the driver, which reads it from its provider every time.
`engine.Open(driver, project)` takes no root and no database.

- **A driver is the store.** The seam is the provider's resources, listed and
  read by project:
  - **Layers** by ID, which is still the cache key: commit under a key, get
    by key, list, delete. A build's cache lookup is a get; a key the provider
    lacks is a miss, and is built.
  - **Tags**, set, moved, listed, and removed through the driver. Docker's are
    image tags; boxd's are tag snapshots, as today.
  - **Instances** by name: create with a spec, get, list, destroy. The driver
    makes a name unique within a project the way its provider makes names
    unique (a container's name, a machine's name), so two commands racing to
    create one name have one winner without a lock of the engine's.
- **Metadata lives on the provider's resources, in the provider's way.**
  - **docker:** labels, set when an image is committed or a container
    created: project, layer ID, parent, guest OS, comment; instance name,
    image, size, forwards.
  - **boxd:** the name, and what the API reports. A layer's snapshot is
    `dvm-<project>--<layer id>`, an instance's machine
    `dvm-<project>--<name>`, which limits a project and a name to 57
    characters together. A machine's size and creation are `GetVm`'s, a
    snapshot's `SnapshotInfo`'s. A layer's parent and comment are not kept:
    a boxd snapshot is a whole capture and boots without its parent, so
    `images -a` lists a boxd layer with no parent and `rmi` deletes one
    whatever was built on it. boxd's `image_ref` of a restored machine names
    the image under the snapshot, not the snapshot (2026-10-23), so a boxd
    instance has no layer either; a restored machine outlives its snapshot,
    so `rmi` cannot break one.
  - **hcs, vz, fake:** the host's disk is the provider. The driver keeps a
    self-describing directory of its own (its configuration, as
    `/var/lib/docker` is Docker's), with each layer's and instance's metadata
    in a JSON file beside its disk, written under a temporary name and
    renamed into place. It takes a lock file where two processes could race.
- **No local state on a remote provider.** boxd and docker keep no file on
  the host. Any process, on any host, with the provider's credentials, sees
  and operates the same layers, tags, and instances.
- **A running VM is the driver's.** Every driver implements `Attach`. hcs and
  vz still need a process to hold a VM, so the shim moves below the seam, into
  a package those drivers share; its `shim.json` and log live in the driver's
  directory, and the driver's `Attach` reaches the VM through it.
  `Capabilities.Remote`, `machine.Dialer` above the seam, and the engine's
  shim go. An embedder still names its own binary's shim subcommand, now
  through the driver's configuration.
- **There are no orphans in disco-vm.** Everything under a project's names is
  that project's, and is listed. `Known`, `Orphans`, `RemoveOrphan`,
  `LayerExists`, and the replaced-layers table leave the seam and the engine.
  An embedder that owns a set of its own (discobox's control plane) reaps
  against it, as its pool agent reaps Docker: list the project's resources,
  remove what it does not hold, treat a failed listing as no answer, and
  spare what is younger than a grace period. An unreferenced, untagged layer
  older than a retention window is reclaimable, as `imagereap` judges an
  image; `rmi` and a future `prune` remove it, never a listing.
- **Existing state is not migrated.** disco-vm is pre-alpha: a root's
  `disco-vm.db`, an embedder's `disco_vm_*` tables, and what they described
  (layers and instances under old names, labels, or records) are not read.
  Remove them, or keep using the disco-vm that made them.

## Alternatives rejected

- **Keep the database and make it a cache of the provider.** Every listing
  already asks the provider (`Orphans`, `Bases`), so the cache saves nothing,
  and a cache that can be wrong needs the reconciliation this removes.
- **Keep detecting orphans, against what the driver lists.** With no store,
  an orphan is only defined against an owner's set, and disco-vm has none of
  its own. discobox has one, and already reaps that way.
- **Encode a boxd layer's parent and comment in its name.** A snapshot
  cannot be renamed, the name already carries the project and a 64-character
  key, and nothing on boxd needs the parent to boot.
- **Write a boxd layer's metadata into the guest's disk.** It can be read
  only by booting a machine from the snapshot.
- **Keep the state of hcs and vz in the engine.** It would leave one driver
  family stateless and the other not, and the engine would keep its database
  and its migrations for them alone.
- **Keep the shim in the engine.** It is how two hypervisors hold a VM, which
  is a driver's business; with it in the engine, the engine keeps
  `shim.json` and a root.
- **Migrate each driver's state once, through a required `Driver.Import`.**
  It was this ADR's first draft, and was built: Docker images relabeled
  through a commit, boxd snapshots saved again under new names, local records
  written beside the disks. disco-vm is pre-alpha, its users can rebuild, and
  the import carried more code and failure modes (a provider that does not
  answer, two processes migrating at once) than the state it saved is worth.

## Consequences

- disco-vm's database, `pkg/image`'s rows, JSON import, `gormdb`, and the
  orphan design are deleted. A root or a provider's resources from before
  this are not read. `pkg/image` becomes the build's reading of the
  driver's layers and tags, or folds into `pkg/build` and `pkg/engine`.
- A boxd account or a Docker daemon is one build cache and one set of
  instances for every host and every embedder that shares it.
- `ls`, `images`, and every build's cache lookup are provider calls. That is
  one list per command, as `Orphans` and `Bases` already make; a build does
  one get per layer.
- On boxd an instance's name is at most 57 characters minus its project's,
  `images -a` shows no parents, and `rmi` of a layer does not refuse because
  of what was built on it.
- `engine.Open` loses two arguments, and the shim's configuration moves to
  the driver: discobox's provider changes with this.
- The seam changes for every driver at once, with `machinetest` and the fake
  driver, so `internal/e2e` keeps proving the lifecycle on every OS.

## Deferred

- **Labels on boxd machines and snapshots.** With them, boxd keeps a layer's
  parent and comment as Docker does, and its names stop carrying
  metadata. Revisit when boxd's API offers them.
- **boxd's limits on a snapshot's name** are not documented. Names of 110
  characters worked (2026-10-23), so a layer's
  `dvm-<project>--<64-character key>` fits; a tag's, which adds its escaped
  ref, may not for a long ref, and boxd refuses it then. Revisit if one is
  refused.
- **`prune`**, removing reclaimable layers by age, waits until someone asks
  for it; `rmi` covers the CLI until then.
