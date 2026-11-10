# pkg/engine

VMs and their lifecycle, above the driver seam, with no state of its own (see the root [DESIGN.md](../../DESIGN.md)). The engine is OS-neutral:
everything it does, it does through `machine.Driver` and `machine.Guest`, and
the fake driver tests all of it on every OS.

## No daemon: a running VM is its driver's

Every call reaches a running instance through its driver (`Driver.Attach`),
with the calling process's own credentials, and nothing of it outlives the
call. Who holds the VM is the driver's business:

- **boxd and docker:** the service (boxd's API, the Docker daemon) owns the
  VM, whatever process started it, so `Start` is `Boot`. See
  [docs/drivers/boxd.md](../../docs/drivers/boxd.md) and
  [docs/drivers/docker.md](../../docs/drivers/docker.md).
- **hcs, vz, and fake:** a Virtualization.framework VM dies with the process
  that created it, and an HCS VM belongs to vmcompute. A **shim per VM**
  ([pkg/machine/local](../machine/local/DESIGN.md)) makes both look the same. `Start` spawns
  `disco-vm shim <id>` detached (setsid, or DETACHED_PROCESS); the shim boots
  the VM, holds it for its whole life, and exits when the VM stops. It writes
  the instance's `shim.json` (pid, loopback address, bearer token) once its
  machine answers, and serves `/status`, `/stop`, `/kill`, the instance's URL
  and forwards, and `/guest`, which upgrades to the agent protocol and is
  answered from its machine's guest operations (`guest.Relay`). Attach
  reaches the VM there. A crash is contained to one VM.

A program that embeds the engine (discobox) starts its shims as a subcommand of
its own binary: `machine.Config.ShimCommand` names it, and that subcommand
makes the same driver and calls `local.ShimMain`. So an embedder ships one
binary, and its VMs outlive it.

## Every instance has one service, at one HTTP URL, and a private one

The driver, not the caller or a committed VM, picks where an instance's one
service listens in the guest (`Driver.Service`, `machine.Service`;
[ADR 0005](../../docs/adr/0005-every-instance-has-one-http-url-and-the-driver-picks-its-port.md)):
`vsock:7302` on vz and hcs (`guest.ServicePort`, listed in every hcs VM's
ServiceTable), `tcp:0.0.0.0:7302` on boxd and docker, and a loopback port
picked at Create on fake, whose guests share the host's. A committed VM
declares no service.

At every start, before it returns, the engine writes that address with the
guest's CopyTo to its `guest.ServiceFile` (`/var/lib/disco-vm/service`, or
`C:\ProgramData\disco-vm\service`), on disk so it outlives a reboot in the
guest. A program the caller copies in reads it, or calls
`guest.ListenService()`, and listens there.

`Engine.URL` and `disco-vm url INSTANCE` give the service's URL, always HTTP:

- **The driver's**, where it has one: boxd's `https://<machine>.boxd.sh`,
  pinned to 7302 at Create and the same across stop and start; Docker's
  `http://127.0.0.1:N`, a port the daemon publishes at each start.
- **The shim's** everywhere else: `http://127.0.0.1:N`, a loopback port the
  shim binds at each start and splices, connection by connection, into the
  hypervisor machine's `Dial(7302)` (`machine.Machine.Dial`). It is in the
  instance's `shim.json`, and there is none while the instance is stopped.

No URL takes a credential, so the service authenticates its callers.

`Engine.PrivateURL` and `disco-vm url --private INSTANCE` give where the
instance's peers, other instances of the driver on its networks, reach the
service (`Service.PrivateURL`; ADR 26-10-29-129), refused as `URL` is while
it is not running, and refused where the driver has none (vz, hcs):

- **A direct path** to the instance's address, where every port the guest
  listens on is reachable: boxd's `http://<machine>.boxd:7302` for a machine
  on networks, docker's container address on its network.
- **The URL**, where that is how peers reach it: boxd's on no networks, and
  fake's, whose guests share the host's loopback.

A caller tells a direct path by its differing from the URL.

Past the one service, `Engine.Dial` and `disco-vm dial INSTANCE PORT` open a
stream to the agent's port or the service's of a running instance (ADR
26-11-03-247), through the process that holds its machine, on a driver that
lists `Capabilities.Ports` (ADR 26-10-31-990): the shim's owner-only socket
on hcs, vz, libkrun, and fake. It is no
URL and needs the owner's access to the driver's directory; boxd and docker
refuse it. Where vz and hcs give no private URL, a guest reaches a peer's
port through a `Forward` to that instance instead
([pkg/machine](../machine/DESIGN.md#ports-not-guids)).

## There are only VMs, and a VM is made fresh or from a committed VM

disco-vm has one kind of thing, a VM, named in its pool, unique across
every VM, committed or not ([ADR
26-11-03-606](../../docs/adr/26-11-03-606-disco-vm-has-only-vms-and-a-vm-is-made-fresh-or-from-a-committed-vm.md)).
There are no images, image IDs, tags, or bases a caller names. A VM is made
three ways:

- **Fresh** (`Engine.Create` with no `From`; `create NAME`, `run NAME`),
  from the driver's base: its configuration (`machine.Config.Base`,
  `$DISCO_VM_BASE` on the command line), never a create's. A driver with no
  base (hcs, docker unconfigured) refuses it with `ErrUnsupported`.
- **From a committed VM** (`CreateOptions.From`; `create NAME --from VM`), a
  copy of its disk, on a driver that reports `Capabilities.Commit`. A
  running VM is refused as a `From`.
- **By installing** (`Engine.Install`, `install NAME`), on a driver that
  lists `Capabilities.Install` (hcs, vz, fake): an OS from media, with the
  agent, this binary, baked in, left committed.

`Engine.Commit` (`commit VM`) freezes a stopped VM under its name: its data
volume and cache clone go, and its disk is what VMs are created from. A
running VM is refused. On a driver that does not report
`Capabilities.Commit` (ADR 26-11-03-868), the engine refuses `Commit`, a
`From`, and an upgrade's `--from` with an error wrapping `machine.ErrUnsupported` that names the
driver, before it asks the driver, as it refuses `Isolated` and `Egress`; so
do `commit`, and `--from` on `create`, `run`, and `upgrade`. A caller there makes every
VM fresh, and `info` reports `"commit": false` up front. To change a committed VM, create a VM from it, change
that, and commit it under a new name; versions are names (`app-v1`,
`app-v2`), and a caller that wants a "current" one keeps its own pointer.

**That a VM is committed is the caller's to record.** Not every provider
can tell: Archil keeps a committed sandbox as a stopped one. So
`machine.Instance` carries no committed mark, `ls` shows a committed VM as
stopped, and the caller never starts one again. A driver refuses only what
its provider cannot do (docker's committed image and boxd's snapshot have no
machine to run: `machine.ErrCommitted`), or what would corrupt a disk (hcs
boots no VM another VM's disk differences over: `machine.ErrInUse`).

What a driver keeps for a committed VM:

- hcs, vz, fake: its record and disk, left where they are
- boxd: a snapshot `dvm-<pool>--<name>`, and the machine deleted
- docker: a Docker image `disco-vm-vm:<pool>--<name>`, and the container
  removed

`rm` removes a committed VM like any. On docker, boxd, vz, and fake a VM
created from it is a disk of its own, and outlives it. On hcs, whose copies
are differencing disks over it, the removal is refused with `ErrInUse` while
a VM created from it exists. Installation media a driver downloads itself
(`--media latest`) is not a VM; a local driver keeps it in `cache/<driver>/`
under its directory, so it is fetched once per host.

## A pool scopes everything, and is in every name

`--pool` (`$DISCO_VM_POOL`, `machine.Config.Pool`; `default` when not
given; ADR 26-11-04-412) names the pool a command works in. A pool is 1 to 32
lowercase letters, digits, and single hyphens. A driver is made for one
pool, and lists and makes only its VMs: a pool's `ls` and names are its own.

The driver puts the pool in the name of everything it makes
(`machine.ResourcePrefix`): boxd's machines and snapshots and docker's
containers are `dvm-<pool>--...`, docker's committed images
`disco-vm-vm:<pool>--...` and readied bases `disco-vm-base:<pool>--...`;
a local driver records it in each `instance.json`. `--` cannot occur
in a pool, so a name belongs to exactly one pool.

There are no orphans in disco-vm (ADR 26-10-23-417): everything under a
pool's names is that pool's, and is listed. A program that embeds the
engine and owns a set of its own (discobox's control plane) reaps against it,
as its pool agent reaps Docker: list the pool's VMs,
remove what it does not hold, treat a failed listing as no answer, and spare
what is younger than a grace period. A driver's own short-lived resources are listed as
VMs while they run (docker's `base-<random>` readying container), and a
readying takes minutes, so the grace period
is longer than that.

The project-to-pool cutover (ADR 26-11-04-412) removes `--project` and
`DISCO_VM_PROJECT`, along with the old Go names. No state migration is
provided: development resources from before the cutover must be removed
with the old binary. Provider names keep their `dvm-<pool>--<name>` shape;
local records/keys use `pool`, Docker labels use `ai.discobox.vm.pool`, and
Docker volume/cache directories use `pools`. Scope values and driver
lifecycle behavior otherwise stay the same. The engine counts no running
guests: a driver enforces `Capabilities.MaxRunning` across every pool
(`machine.ErrCapacity`).


## An instance's volumes, and a cache saved only on request

`create` and `run` give an instance a data volume (`--data SIZE`) and a
cache volume (`--cache KEY`, `--cache-size SIZE`), at
`machine.VolumePath` (ADR 26-11-02-180;
[pkg/machine](../machine/DESIGN.md#two-volumes-at-fixed-paths-and-a-cache-is-saved-only-on-request)).
`Engine.Create` refuses, with `ErrUnsupported`, a volume the driver does not
list in `Capabilities.Volumes`, and never makes a directory on the root disk
in its place; it refuses a negative size and a key that is not one
(`machine.ValidateCacheKey`), before it finds the `From`.

`Engine.SaveCache` (`cache save INSTANCE [KEY]`) publishes a stopped
instance's cache volume as a new generation of a key, the instance's own by
default, and keeps the instance; one not found stopped is refused, as
`Commit` refuses it. A save to the key the instance started from is
guarded by the generation it was cloned from (`Instance.Cache.Generation`),
so a run that began from an older cache never replaces a newer one; another
key is saved unguarded, and `SaveOptions.Force` (`--force`) drops the guard.
`Engine.Caches` (`cache ls`) and `Engine.DeleteCache` (`cache rm`) are the
driver's. A second guarded save from one instance is refused too, since the
key has moved to its own first save: force it, or save from a new instance.

`Engine.Upgrade` (`upgrade VM [--from COMMITTED]`) gives a stopped VM a new
root, a copy of a committed VM or fresh from the base, and keeps its name,
settings, data volume, and cache clone (ADR 26-11-03-782;
[pkg/machine](../machine/DESIGN.md#a-data-volume-is-its-vms-and-upgrade-gives-it-a-new-root)).
It refuses, before the driver, a driver that does not list `VolumeData`, a
`--from` on a driver that does not report `Commit`, a VM
not found stopped, and a `--from` that is the VM itself, another guest OS,
or running. A `--from` it does not find goes to the driver by name, which
finishes an interrupted upgrade without it where it can and refuses it
(`ErrNotFound`) where it cannot. The old root is gone for good: a caller that wants a
way back commits the VM first. This is how a caller rebuilds a VM and keeps
its data; `rm` is the only way the data goes, besides `commit`.

## disco-vm does not converge guests

Deciding what a committed VM holds is a client's, above disco-vm ([ADR
26-10-15-703](../../docs/adr/26-10-15-703-disco-vm-does-not-converge-guests.md)).
disco-vm offers what a client converges with: exec and copy
(`Engine.Guest`), `Engine.Stop`, and `Engine.Commit`. A converged VM is one
that was created, changed through exec and copy, stopped, and committed.
