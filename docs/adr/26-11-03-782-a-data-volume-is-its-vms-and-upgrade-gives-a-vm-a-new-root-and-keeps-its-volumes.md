# 26-11-03-782 — A data volume is its VM's, and Upgrade gives a VM a new root and keeps its volumes

- **Status**: Accepted (supersedes in part 26-11-02-180, its §5 docker row)
- **Date**: 2026-11-03

Accepted by the owner by merging #39, 2026-11-03.

## Context

ADR 26-11-02-180 gives a VM a data volume at `/.discobox/data`. Its §3 makes
the volume the VM's own: made at Create, kept across stop and start, and
removed by Destroy. `main` does that (`33294b1`, `247f378`):

- `pkg/machine/volume.go` documents `VolumeData` as "removed by Destroy";
- `Driver.Destroy` (`pkg/machine/machine.go`) removes "a VM and everything
  made for it, its volumes included";
- fake keeps the volume as a directory in the VM's root
  (`pkg/machine/fake/fake.go`, `volume`);
- docker keeps it as a named volume labeled with the project and the VM's
  name (`pkg/machine/docker/volumes.go`, `makeVolumes`);
- boxd, vz and hcs list no volumes and refuse one;
- the conformance suite asserts that a new VM under a destroyed VM's name
  sees none of its data (`pkg/machine/machinetest/machinetest.go`, the
  data volume case).

Issue #23 asked for the opposite: volumes that outlive their VM, made,
listed and removed on their own, attached by name to any one VM at a time,
so discobox's pool can rebuild a sandbox by destroying its machine and
creating a new one with the same volumes. The owner settled it on the
issue: **a data volume is attached to the VM it was created for. Its only
purpose is to let that VM be upgraded with a new root. It is removed when
the VM is.**

So the volume stays as 180 has it. What `main` lacks is the upgrade: no
call gives a VM a new root. A caller's only ways to a new root are
`Destroy` and `Create`, which lose the data volume, or a new VM under
another name, which starts with an empty one. discobox's pool ships the
no-volume rebuild (copy its bootstrap and pins back into the same machine
and restart it) and waits on this for the other mode.

ADR 26-11-03-606 makes a VM's root come from one of two places: the
driver's base (a fresh VM), or a committed VM (`InstanceSpec.From`). ADR
26-11-03-868 (Proposed) makes the second a capability, `Commit`.

docker keeps every volume in Docker's own volume store (180 §5): a VM's
data and cache clone, and each saved cache generation with its pointer.
The cache is copied between them through the daemon's archive API, a tar
stream that passes through the driver (`pkg/machine/docker/volumes.go`,
`copyCache`). discobox's own docker driver keeps a sandbox's directories
as host bind mounts under `/var/lib/discobox` instead, such as a pool's
cache at `/var/lib/discobox/cache/projects/<project>/pools/<pool>/cache`
(180, Context). The owner asked, reviewing this ADR, that disco-vm's
docker driver do the same: no Docker volumes, and data and cache in host
directories under that tree.

## Decision

### 1. A data volume is its VM's, and goes only with it

ADR 26-11-02-180 §3 stands. A data volume is made at Create, for that VM.
It is never listed, named, found, attached or removed apart from its VM.
`Destroy` (`disco-vm rm`, which is what a caller's purge of the machine
calls) removes it with the VM, and nothing else does except `Commit`,
which leaves a committed VM with no volume (606 §2).

### 2. Upgrade gives a stopped VM a new root, and keeps the rest of it

`machine.Driver` gains a required method:

```go
// Upgrade gives stopped VM inst a new root disk: a copy of the committed
// VM from, or, where from is nil, a fresh one from the driver's base. The
// VM keeps its name, its data volume and its cache clone as they are, and
// what its spec set (CPUs, Memory, Forwards, Isolated, Networks, Egress).
// Its old root is removed. It returns the VM as the provider now has it.
Upgrade(ctx context.Context, inst Instance, from *Instance) (Instance, error)
```

- **The VM is the same VM.** It keeps its name, and a caller that held it
  holds it still: an `Instance` from before the upgrade names it, though
  its `ID`, `From` and `Created` may change, as the provider reports them.
- **Its volumes are kept, untouched.** The new root mounts the same data
  volume and cache clone at their `VolumePath`s. The cache clone is the
  VM's own (180 §4) and is not re-cloned from its key: a caller that wants
  the key's newest generation in a VM makes a new VM.
- **Its old root is gone.** Nothing of it is kept, and it cannot be
  restored. A caller that wants to go back commits the VM first and
  upgrades to that.
- **Its service is a driver's, as before.** The service's listen address
  and URL follow the rules they follow across a stop and start
  (`Driver.Service`, `pkg/machine/machine.go`), so a URL that outlives a
  stop outlives an upgrade.

`Upgrade` refuses, and changes nothing:

- a running VM, as `Commit` does;
- a `from` whose guest OS is not the VM's: a data volume is formatted and
  read by its guest OS;
- a `from` the provider no longer has (`ErrNotFound`), or that is running;
- a fresh root on a driver with no base (`ErrUnsupported`, as `Create`
  refuses one);
- a non-nil `from` on a driver without `Capabilities.Commit` (868, once
  accepted);
- any upgrade on a driver that does not list `VolumeData` in
  `Capabilities.Volumes` (`ErrUnsupported`): a data volume and its upgrade
  are one capability, since neither is any use without the other.

A VM with no data volume, on a driver that lists `VolumeData`, may be
upgraded too. It keeps its name and settings, and has nothing else to keep.

### 3. An interrupted upgrade is finished by calling it again

A provider may not swap a root in one step: docker removes a container and
makes another, and Archil deletes a sandbox and makes another of the name.
Whatever step a driver is stopped at:

- the VM is still listed under its name, by `Instance` and `Instances`,
  with its data volume;
- `Upgrade` again, with the same `from`, finishes it, and `Destroy`
  removes it with its volume;
- no data volume is ever left that no listed VM owns, so a lost upgrade, as
  a lost create (ADR 0004), is found by its name.

How a driver reports a VM caught between its roots (stopped, with an
`Instance` that `Boot` refuses until `Upgrade` finishes, or otherwise) is
left to implementation, and the conformance suite fixes it once chosen.

### 4. The engine and the command line

- `Engine.Upgrade(ctx, name, UpgradeOptions{From string})` refuses a
  running VM and the cases of §2 before it reaches the driver.
- `disco-vm upgrade NAME [--from VM]`.

### 5. What each driver does

| driver | data volume | upgrade |
|---|---|---|
| fake | a directory in its root (as today) | a new root copied from the base or the committed VM, with the data and cache directories moved into it from the old one, which is then removed |
| docker | a host directory bind-mounted at its path (§6) | a new container from the base image or the committed VM's image, bind-mounting the VM's same directories and carrying its labels; the old container removed |
| vz, hcs | none yet (180, Deferred) | `ErrUnsupported` until they list `VolumeData`; then the VM's root disk replaced in its directory |
| boxd | none: its API attaches no disk but the root | `ErrUnsupported` |
| archil (planned) | a disk per VM, named for it | the sandbox deleted and made again under its name, from the base or a fork of the committed VM, mounting the VM's disk |

How docker orders the removal of the old container and the creation of the
new one under the same container name, so that §3 holds, is left to
implementation. A directory is not removed with a container, so the VM's
data outlives the old container whatever the order.

### 6. docker keeps volumes and caches in host directories, laid out as discobox's

docker uses no Docker volumes. This replaces the docker row of 180 §5,
which it supersedes in part once accepted. Every volume and every cache
generation is a directory on the daemon's host (under Docker Desktop, its
VM), bind-mounted where the guest sees it. The directories sit under
`/var/lib/discobox`, laid out as discobox's own docker driver lays out its
tree:

| what | directory |
|---|---|
| a VM's data volume | `/var/lib/discobox/data/projects/<project>/vms/<name>` |
| a VM's cache clone | `/var/lib/discobox/cache/projects/<project>/vms/<name>` |
| a cache key's saved generations | `/var/lib/discobox/cache/projects/<project>/keys/<key>/<generation>` |

`<project>` is the driver's project (ADR 0004). discobox's
`cache/projects/<project>/pools/<pool>/cache` sits beside these, and
disco-vm never reads, writes or removes it.

- **The root is a driver setting** whose default is `/var/lib/discobox`.
  How a caller sets another root is left to implementation.
- **The daemon may be remote**, so the driver never touches the
  directories itself. A short-lived container that bind-mounts the root
  makes, copies and removes them, as a copier container mounts a
  generation today. A cache is cloned and saved by a copy on the daemon's
  host, not streamed through the driver.
- **A generation becomes current by an atomic rename** once it is whole,
  so no VM ever clones a partial one. The guard of a guarded save, and
  which generation a key holds, keep 180 §4's rules. How the current
  generation is recorded is left to implementation.
- **A directory is the VM's, and goes with it** (§1). `Destroy` removes the
  VM's directories, then its container, so a `Destroy` that fails leaves
  the VM listed and a second `Destroy` finds them. A directory a lost
  create left must never be taken by a later VM of the name. Whether that
  is done by a random end recorded on the container, as the named volumes
  have today, or by emptying a directory that no container holds, is left
  to implementation.
- **Files keep the guest's uids**, shifted (`16777216+N` on the host, ADR
  0002). docker-init gives the guest's root each mount point at every
  start, as it does today (`pkg/machine/docker/init_linux.go`,
  `giveVolumes`). A host process reading a VM's data sees those uids.
- **`Bytes` stays a hint.** A host directory has no quota, as the local
  volume driver had none.

A VM made before this change, whose container mounts named volumes, keeps
them: it boots, and `Destroy` removes them, as today. `Upgrade` copies
such a VM's data volume and cache clone into its directories, mounts those
in the new container, and removes the named volumes once the new
container is made. Cache generations saved as named volumes are not read,
so their keys start empty. A cache is a cache, so losing them costs only a
warm run. `docker volume rm` removes them by hand, as 606 left the old
image tags.

## Alternatives rejected

- **docker keeps named Docker volumes** (180 §5, and this ADR as first
  proposed). A volume lives in Docker's store, named by the driver, where
  neither discobox nor an operator on the host finds a sandbox's data by
  its project and name. discobox's docker driver already keeps that data
  under `/var/lib/discobox`, and a disco-vm VM's data belongs in the same
  tree. Every cache copy is also a tar stream through the driver, where a
  host directory is copied in place.
- **A subtree of disco-vm's own** (`/var/lib/discobox/vm/...`), apart from
  discobox's directories. Nothing would collide, but discobox's operators
  would find a sandbox's data in two layouts. The owner chose discobox's
  layout.
- **Bind-mount discobox's pool cache, shared by every VM of the pool.**
  180 rejected a cache every live VM writes, since any sandbox could plant
  a file that later ones run.

- **Volumes that outlive their VM** (the issue as filed): a data volume
  made, listed and removed on its own, attached to one VM at a time, and
  kept by `Destroy`. The volume's only use is to carry one machine's state
  onto a new root. A volume apart from its VM is a second kind of thing,
  with its own names, listings and lifecycle, in every driver, which 606
  removed images to avoid. A caller would also have to remove it itself, and
  one it forgets is data nobody owns.
- **Destroy keeps the data volume, and a Create of the same name adopts
  it.** "Destroy" would no longer mean the VM is gone, a caller that wanted
  it gone would need another call, and a new VM reusing a name would
  silently inherit a stranger's data.
- **Create with a data volume taken from another VM**
  (`DataVolume{From: "old"}`), then Destroy the old one. Two VMs, two names,
  and a window where the volume is attached to both or neither. The caller
  wants the same VM with a new root, and should say so.
- **Upgrade re-clones the cache from its key.** Tempting, since an upgrade
  often follows a new commit that a warm run cached. But the clone is the
  VM's own (180 §4), and an upgrade that drops it would make the cache the
  one volume an upgrade does not keep. A VM that wants the key's newest
  generation is a new VM.
- **Upgrade changes the VM's settings too** (CPUs, Memory, Networks,
  Egress). Not needed to keep a VM's data, and each needs its own rules on
  every driver; see Deferred.
- **A capability of its own** (`Capabilities.Upgrade`). A driver with data
  volumes and no upgrade has a volume with no purpose, and one with an
  upgrade and no data volumes offers only a Destroy and Create under the
  same name. Tying it to `VolumeData` keeps one fact for the caller to read.

## Consequences

- `machine.Driver` gains `Upgrade`, and every driver implements it: fake and
  docker in full, boxd, vz and hcs as `ErrUnsupported`. How
  `local.Driver` shares the step between its hypervisors is left to
  implementation.
- The conformance suite gains cases: a VM upgraded fresh and from a
  committed VM keeps what it wrote to its data volume and its cache clone,
  loses what it wrote to its root, keeps its name and settings, and is
  refused while running and across guest OSes. The case that a new VM under
  a destroyed one's name sees no data stays. A driver that does not list
  `VolumeData` refuses `Upgrade` and changes nothing.
- An interrupted upgrade is tested on fake and docker by failing the driver
  between its steps, then calling `Upgrade` and `Destroy` again.
- `internal/e2e` gains `upgrade`.
- docker's volumes and caches move from named volumes to host directories
  (§6). Its `volumes.go`, and `docs/drivers/docker.md`'s volumes and caches
  rows, are rewritten. A VM made before keeps its named volumes until it is
  upgraded or destroyed. On acceptance, 180's docker row is marked
  superseded in part by this ADR.
- The docker driver needs a writable `/var/lib/discobox` (or the root it
  is given) on the daemon's host.
- discobox's pool rebuilds a sandbox with a data volume by stopping its
  machine and calling `upgrade --from` the committed VM its pool now uses,
  and keeps the no-volume rebuild for drivers that do not list
  `VolumeData`. It removes the machine, and its data, only when it purges
  the sandbox.
- Issue #23's "Done when" changes: an upgraded VM, not a new VM, sees the
  files its data volume held.

## Deferred

- **Changing a VM's settings at upgrade** (CPUs, Memory, Forwards,
  Networks, Egress). Revisit when a caller's rebuild needs a setting the
  VM was not created with.
- **Upgrade on vz and hcs.** Revisit with their data volumes (180,
  Deferred).
