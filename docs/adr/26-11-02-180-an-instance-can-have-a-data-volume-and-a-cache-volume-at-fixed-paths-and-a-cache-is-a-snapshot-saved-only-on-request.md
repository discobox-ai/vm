# 26-11-02-180 — An instance can have a data volume and a cache volume at fixed paths, and a cache is a snapshot saved only on request

- **Status**: Accepted
- **Date**: 2026-11-02

## Context

An instance has one disk, its root, and an image is a snapshot of it
(26-10-23-410). discobox gives every sandbox two more directories:

- **`/.discobox/data`**: the sandbox's own state, the harness's memories,
  transcripts and settings. It must outlive the sandbox's stops, and must
  never end up in an image cut from it.
- **`/.discobox/cache`**: package and build caches, shared by a pool today
  as a bind mount of one host directory per pool
  (`/var/lib/discobox/cache/projects/<project>/pools/<pool>/cache`). Every
  live sandbox of the pool writes to it.

disco-vm has no way to give an instance either. A provider that sells
storage better than its root disk cannot be used for them. Archil
(archil.com) is one: its sandboxes are Firecracker microVMs whose extra
disks are mounted by the provider, at a path the caller chooses, from a
`mounts` list at create or start. We checked this by hand on 2026-11-02:
two disks mounted at `/.discobox/data` and `/.discobox/cache`.

A cache that every live machine writes is also a hole. Any sandbox, running
whatever code it was handed, can plant a file that every later sandbox of the
pool runs or reads. discobox wants the cache warmed by a trusted run instead.
If a repository has `.discobox/warm`, discobox runs a fresh "warm" instance
for each new commit on the HEAD branch, on an interval. That instance runs
the script, and its cache becomes the one the next sandboxes start from.

Providers differ in what they can do with a second disk:

- **Mounted read-write by many machines at once**: Archil's `conditional`
  mounts, a Docker volume, a host directory over virtiofs on vz.
- **Cloned copy-on-write**: an APFS `clonefile` on vz, an Archil branch.
- **Neither**: boxd's API attaches no disk but the root, which is a fixed
  100 GiB.

Archil's fork branches only the sandbox's root disk; the fork mounts the same
extra disks as its source (checked by hand, 2026-11-02). Archil can clone a
disk only through its branch API, `POST /api/disks/{id}/branches`, which its
SDK has and its public API reference does not. A branch is mounted by
passing its `filesystem_id` as a mount's `disk_id`. A checkpoint can be taken
only through a mount, and a single branch cannot be deleted, only its whole
root disk. All of this was checked by hand on 2026-11-02, and we have asked
Archil to support it.

## Decision

### 1. Two volumes, a data volume and a cache volume, each a capability

`machine.InstanceSpec` and `engine.CreateOptions` gain `Data` and `Cache`.
`machine.Instance` reports them as the provider has them.

```go
type DataVolume  struct{ Bytes int64 }
type CacheVolume struct {
	Key        string // the cache it starts from
	Bytes      int64
	Generation string // reported: the generation it was cloned from; empty for none
}

type Volume string
const (
	VolumeData  Volume = "data"
	VolumeCache Volume = "cache"
)
```

- **These two volumes are the whole contract.** There is no list of volumes,
  and a caller does not choose a mount point.
- **A driver lists what it attaches** in `Capabilities.Volumes`. The engine
  refuses, with `ErrUnsupported`, a spec that asks for a volume the driver
  does not list. It never falls back to a directory on the root disk: a
  caller that wants one makes it.
- **`Bytes` is the volume's size.** Zero is the driver's default. A provider
  that sizes storage, such as a disk image or a block device, holds it as a
  cap. A provider whose storage grows on demand, such as Archil or a Docker
  volume, takes it as a hint, and `Instance` reports what the provider
  reports.

### 2. Every driver mounts them at the same path for each guest OS

`machine.VolumePath(os, volume)` is the path:

| guest OS | data | cache |
|---|---|---|
| linux | `/.discobox/data` | `/.discobox/cache` |
| darwin | `/.discobox/data` | `/.discobox/cache` |
| windows | `C:\.discobox\data` | `C:\.discobox\cache` |

- **The driver mounts both** before `Boot` or `Start` returns, so a
  process the caller runs finds them in place.
- **Where the guest image does the mounting**, it mounts by a name the
  driver gives the disk, and makes the filesystem if the disk is blank. On
  vz's Linux guest, that is a virtio-blk device identifier and
  `x-systemd.makefs,nofail`.
- **An instance with no volume** has no mount there. The path is whatever
  the image holds.
- **On darwin**, `/.discobox` is a `synthetic.conf` entry baked in at
  install, because the system volume is read-only.
- **On windows**, a volume is an NTFS folder mount point.

### 3. A data volume is the instance's

It is made at Create, and kept across stop and start. Destroy removes it.
**It is never part of an image:** Commit captures the root disk alone, and
the data volume goes with the instance Commit consumes.

### 4. A cache is a chain of snapshots, cloned at Create and saved only on request

A cache key names a project's cache, as a tag names an image. Keys are
validated as instance names are, and each project has its own
(0004). A key points at its current **generation**, an immutable
snapshot of a cache volume, which nothing mounts writable.

- **At Create,** an instance whose spec names a key gets a private,
  writable clone of the key's current generation. If the key has none, it
  gets an empty volume of `Bytes`. A clone smaller than `Bytes` grows to it.
  `Instance.Cache.Generation` says which generation was cloned.
- **The clone is the instance's own.** It is kept across stop and start, is
  never refreshed from the key, and no other machine mounts it. Destroy and
  Commit remove it.
- **Only the caller publishes.** `SaveCache` snapshots a stopped instance's
  cache volume as a new generation and moves a key to it. The instance and
  its clone are kept. Nothing the guest does saves a cache, and nothing
  saves one on stop.
- **The caller names the key at save time.** It may differ from the key
  the instance started from, so a run can start from `go-main` and save to
  another key, or save nothing.
- **A save is guarded.** Given the generation the caller expects the key to
  hold, `SaveCache` refuses with `ErrCacheMoved`, and moves nothing, if the
  key has moved since. A warm run for an older commit then cannot replace
  a newer one's cache.
- **An old generation is removed** when the key moves off it, unless a
  driver's clones still chain on it.

`machine.Driver` gains three required methods. A driver that does not list
`VolumeCache` lists no caches, and returns `ErrUnsupported` from the other
two.

```go
type Cache struct {
	Key        string
	Generation string
	Bytes      int64
	Saved      time.Time
}

type CacheSave struct {
	Key string
	// Guarded refuses the save unless Key holds IfGeneration, where an
	// empty IfGeneration is no generation at all.
	Guarded      bool
	IfGeneration string
}

// Caches lists the project's cache keys.
Caches(ctx context.Context) ([]Cache, error)
// SaveCache snapshots stopped inst's cache volume as a new generation and
// points save.Key at it. A guarded save whose key does not hold the
// generation it expects is ErrCacheMoved, and nothing moves.
SaveCache(ctx context.Context, inst Instance, save CacheSave) (Cache, error)
// DeleteCache drops key and the generations nothing else needs. A key
// already gone is not an error.
DeleteCache(ctx context.Context, key string) error
```

`Engine.SaveCache` refuses an instance that is not stopped, as
`Engine.Snapshot` does. It guards a save to the key the instance started
from by the generation it was cloned from, unless `SaveOptions.Force` is
set.

The command line is:

- `run --data SIZE` and `run --cache KEY [--cache-size SIZE]`;
- `disco-vm cache ls | save INSTANCE [KEY] | rm KEY`.

`cache save` saves to the key the instance started from unless another is
named. It is guarded by the generation the instance was cloned from when it
saves to that same key; `--force` saves unguarded.

### 5. What each driver does

| driver | data | cache |
|---|---|---|
| fake | a directory in the guest's root, at its path, emptied out of the image by Commit | a directory per generation, copied into the guest's root at Create and out of it at save |
| docker | a named volume, labeled with the project and instance | a named volume per generation, and an empty pointer volume naming it, made once the generation is whole (labels never change); a key's current generation is its newest pointer's. A cache is copied between volumes through the daemon's archive API, by a container made and never started |
| vz | not yet (Deferred) | not yet (Deferred) |
| hcs | not yet | not yet |
| boxd | none: its API attaches no disk | none |
| archil (planned) | a disk per instance, mounted exclusively by the provider at the path | **not yet** (Deferred) |

## Alternatives rejected

- **A list of volumes, each with a path, a size, and a sharing mode.**
  Every guest OS would still need agreed paths for discobox to find its
  volumes. Every driver would still need a meaning for every sharing mode.
  Two kinds cover what discobox does, and each driver can say exactly which
  of them it holds.
- **A cache mounted live by every instance of its key, read-write.** It
  needs storage that many machines write at once, which most drivers lack.
  It also lets any instance poison every other instance's cache. A
  sandbox's root user is not trusted, so the cache it writes cannot be
  either.
- **A cache saved when its instance stops.** Every guest, untrusted ones
  included, would publish to the next instance. Choosing which run to trust
  is the caller's call, not the guest's.
- **A cache cloned at every start, not at Create.** A long-lived instance
  would lose what it built at every boot, or need a merge.
- **A cache carried in the image, on the root disk.** On Archil, where a
  fork branches only the root disk, this is free. But it ties a cache to one
  image, so an instance could not take image X with cache K. It also breaks
  "an image is the root disk alone" for every driver, because of one.
- **A data volume captured into an image, or a Commit refused while one
  exists.** An image is a template others run, and the data volume holds
  one instance's private state.
- **Falling back to a directory on the root disk when a driver lacks a
  volume.** The caller could not tell a volume from a directory that dies
  with every snapshot. "Reported, never faked" holds as it does for
  `Isolated` and `Egress`.

## Consequences

- Every driver implements `Caches`, `SaveCache` and `DeleteCache`, and
  reports `Data` and `Cache` on its instances. fake and docker hold both
  volumes, so the contract is proven on every OS and in CI.
- The conformance suite gains cases:
  - a data volume kept across stop and start, gone after Destroy, and
    absent from a snapshot;
  - a cache cloned from its key's generation;
  - a write in one instance seen by neither a live peer nor the key until
    it is saved;
  - a save seen by the next instance of the key;
  - a guarded save refused once the key has moved;
  - another project and another key seeing nothing.
- docker's cache clone and save copy the volume, O(its size).
- disco-vm's Linux guest image gains the mount units, and macOS and Windows
  guests their mount points, before vz and hcs list a volume (Deferred).
- discobox's warm run is `run --cache KEY`, `.discobox/warm`, `stop`,
  `cache save`, then `snapshot` or `rm`. Its sandboxes run with
  `--cache KEY` and never save.

## Deferred

- **Archil's cache volume.** An Archil driver lists `VolumeData` alone.
  Archil can clone a disk as a branch of a checkpoint and mount it by the
  branch's `filesystem_id`, but only through an API it has not published,
  and with no way to delete one branch. Revisit when Archil supports the
  branch API and per-branch deletion: a generation would be a checkpoint, a
  clone a branch, and a save a checkpoint of the instance's branch.
- **vz's volumes.** On Linux guests, a data volume is a second disk image
  attached as a virtio-blk device, and a cache generation is a disk image
  cloned with `clonefile` at Create and at save. The guest image mounts each
  by its device identifier. Revisit on a Mac, where it can be run: this
  change is built and verified where vz cannot run. macOS guests follow when
  a caller needs them.
- **hcs's volumes:** revisit when a caller needs them on Windows guests.
- **Resizing a volume after Create:** revisit when a caller needs to grow
  one.
