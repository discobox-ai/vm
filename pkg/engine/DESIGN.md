# pkg/engine

Images, instances, and their lifecycle, above the driver seam, with no state
of its own (see the root [DESIGN.md](../../DESIGN.md)). The engine is OS-neutral:
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

## Every instance has one service, at one HTTP URL

The driver, not the caller or the image, picks where an instance's one
service listens in the guest (`Driver.Service`, `machine.Service`;
[ADR 0005](../../docs/adr/0005-every-instance-has-one-http-url-and-the-driver-picks-its-port.md)):
`vsock:7302` on vz and hcs (`guest.ServicePort`, listed in every hcs VM's
ServiceTable), `tcp:0.0.0.0:7302` on boxd and docker, and a loopback port
picked at Create on fake, whose guests share the host's. A build spec
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
  hypervisor machine's `Dial(7302)` (`local.Machine`). It is in the
  instance's `shim.json`, and there is none while the instance is stopped.

No URL takes a credential, so the service authenticates its callers.

## The driver's layers are the build cache

A layer is an immutable, committed disk state, which its driver keeps:

- hcs: a differencing VHDX
- vz: an APFS-cloned bundle
- boxd: a boxd snapshot, `dvm-<project>--<layer ID>`
- docker: a Docker image committed from the stopped guest,
  `disco-vm-layer:<project>--<layer ID>`
- fake: a directory tree

A layer's **ID is its cache key**. The key is a hash of the parent ID, driver,
guest OS, and the fully resolved steps, including a content digest of every
copied file. A build asks the driver for each key (`Driver.Layer`); one it has
is never rebuilt, and `--no-cache` salts the keys instead of overwriting. The
one exception is a snapshot (`Engine.Snapshot`, `disco-vm snapshot INSTANCE
[NAME[:TAG]]`): a stopped instance's disk committed as a layer on its image's
layer, under a random ID, since nothing reproducible made it. A snapshot
consumes the instance, as a build step does, so a layer is always cut the
same way, from a stopped guest that is then destroyed; to go on from one,
create an instance of it (ADR 26-10-15-882).

Tags name layers, and the driver keeps them too: a second snapshot on boxd,
`dvm-<project>--<layer ID>--t-<ref>`, the ref's punctuation escaped; an image
tag `disco-vm/<project>/<ref>` on docker; a file on hcs, vz, and fake. A ref
is Docker's grammar (`engine.ValidateRef`). A layer with no tag and no child
is an untagged image: `images` lists it as `<none>` (`images -a` lists every
untagged layer, a chain's intermediate layers too) and `rmi` takes its ID.
`rmi` untags, then deletes layers that nothing else tags, parents, or was
made from, as `docker rmi` does; a listing that fails is no answer, and never
read as unused. A layer of the same key committed by two builds at once is
identical by construction, and the driver keeps either. Installation media a
driver downloads itself (`media: latest`) is not a layer; a local driver keeps
it in `cache/<driver>/` under its directory, so it is fetched once per host.

What the driver keeps can be deleted behind disco-vm's back (`boxd snap rm`,
`docker rmi`, a layer's directory), and a layer that is gone is a cache miss:
the build makes it again under the same key. A boxd snapshot and a Docker
image are whole, so a layer built on one that was lost still boots and is
still used. A local layer whose disk differences over its parent's (hcs) is
not: each records its parent's generation, a random token made when the
parent was published, and one whose parent is gone or was made again since is
not found either (`local.Driver.Layer`), so it is built again too.

## A project scopes everything, and is in every name

`--project` (`$DISCO_VM_PROJECT`, `machine.Config.Project`; `default` when not
given; ADR 0004) names the project a command works in. A project is 1 to 32
lowercase letters, digits, and single hyphens. A driver is made for one
project, and lists and makes only its layers, tags, and instances: a
project's `images`, `ls`, names, and tags are its own. A project other than
the default is part of every layer's cache key, and two projects never share
a layer ID. The default project's keys are those from before projects, so its
cache stays valid.

The driver puts the project in the name of everything it makes
(`machine.ResourcePrefix`): boxd's machines and snapshots and docker's
containers are `dvm-<project>--...`, docker's layer images
`disco-vm-layer:<project>--...` and tags `disco-vm/<project>/...`; a local
driver records it in each `layer.json` and `instance.json`. `--` cannot occur
in a project, so a name belongs to exactly one project.

There are no orphans in disco-vm (ADR 26-10-23-417): everything under a
project's names is that project's, and is listed. A program that embeds the
engine and owns a set of its own (discobox's control plane) reaps against it,
as its pool agent reaps Docker: list the project's instances and images,
remove what it does not hold, treat a failed listing as no answer, and spare
what is younger than a grace period.

## An instance runs straight from a driver's base

A driver lists what it already has that an instance can run from with no
layer (`Driver.Bases`, `machine.Base`; ADR 26-10-15-833): boxd's default
image (`GetConfig`), and every ready snapshot in the account whose name does
not start with `dvm-`, so nothing disco-vm made, in any project, is a base.
The other drivers have none. `images` lists each on a row of its own, after
the images, with `-` for its layer.

A ref goes to the images first, a tag or a layer ID, then to the driver's
bases by name or ID (`Engine.Base`), so a tag of the same name hides a base
and its ID still reaches it. `create` and `run` of a base make an instance of
the base instead of a layer (`InstanceSpec.Base`), cold, with nothing
installed: on boxd its machine is made at create, from the image
(`CreateVm`) or the snapshot (`CreateVmFromSnapshot`), at the size asked
for, and stopped. A
build's `from.image` may name one: its first layer is committed with no
parent, and its key names the base's driver, ID, and version where a parent
layer's ID would be, so a snapshot captured again builds again. `snapshot` of
an instance of a base is a layer with no parent too. A base is not an image:
`tag` and `rmi` refuse it (`ErrBase`), since it has no layer and
disco-vm never deletes what it did not make.

## disco-vm does not converge guests

Deciding what an image holds beyond a build spec is a client's, above
disco-vm ([ADR 26-10-15-703](../../docs/adr/26-10-15-703-disco-vm-does-not-converge-guests.md)).
disco-vm offers what a client converges with: exec and copy
(`Engine.Guest`), `Engine.Stop`, and `Engine.Snapshot`. A converged image is
an instance that was created, changed through exec and copy, stopped, and
snapshotted.
