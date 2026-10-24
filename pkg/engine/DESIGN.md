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
picked at Create on fake, whose guests share the host's. An image declares
no service.

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

## An image is a snapshot, and stands alone

An image is an immutable, committed disk state, which its driver keeps ([ADR
26-10-23-410](../../docs/adr/26-10-23-410-images-are-snapshots-and-stand-alone.md)).
There is no image build. An image is made two ways, each from a guest shut
down in order:

- **`snapshot`** (`Engine.Snapshot`, `disco-vm snapshot INSTANCE
  [NAME[:TAG]]`): a stopped instance's disk, committed (`Driver.Commit`). A
  snapshot consumes the instance, so an image is always cut the same way; to
  go on from one, create an instance of it (ADR 26-10-15-882).
- **`install`** (`Engine.Install`, `disco-vm install`), on a driver that
  lists `Capabilities.Install` (hcs, vz, fake): an OS from media, with the
  agent, this binary, baked in. A driver that does not has bases instead
  ([below](#an-instance-runs-straight-from-a-drivers-base)).

What a driver commits:

- hcs: a VHDX, a differencing one for a snapshot
- vz: an APFS-cloned bundle
- boxd: a boxd snapshot, `dvm-<project>--<image ID>`
- docker: a Docker image committed from the stopped guest,
  `disco-vm-layer:<project>--<image ID>`
- fake: a directory tree

An image's ID is random: nothing reproducible made it, and nothing is
cached. Above the seam every image stands alone: it has no parent, an
instance runs from one image, and removing one never touches another. A
driver whose disks chain below it keeps that to itself
([local](../machine/local/DESIGN.md)): hcs records each snapshot's parent and
refuses to delete a disk another image's differences over
(`machine.ErrInUse`).

Tags name images, and the driver keeps them too: a second snapshot on boxd,
`dvm-<project>--<image ID>--t-<ref>`, the ref's punctuation escaped; an image
tag `disco-vm/<project>/<ref>` on docker; a file on hcs, vz, and fake. A ref
is Docker's grammar (`engine.ValidateRef`). An image with no tag is listed
by `images` as `<none>`, and `rmi` takes its ID. `rmi` untags, then deletes
the image if nothing else tags it or was made from it, as `docker rmi` does;
a listing that fails is no answer, and never read as unused. An image the
driver keeps (`ErrInUse`) is left untagged, and `rmi` says why. Installation
media a driver downloads itself (`--media latest`) is not an image; a local
driver keeps it in `cache/<driver>/` under its directory, so it is fetched
once per host.

What the driver keeps can be deleted behind disco-vm's back (`boxd snap rm`,
`docker rmi`, an image's directory), and an image that is gone is not found.
A boxd snapshot and a Docker image are whole, so an image snapshotted from
one that was lost still boots. A local image whose recorded parent is gone
is not found either (`local.Driver.Image`); `images` still lists it, so
`rmi` removes it.

## A project scopes everything, and is in every name

`--project` (`$DISCO_VM_PROJECT`, `machine.Config.Project`; `default` when not
given; ADR 0004) names the project a command works in. A project is 1 to 32
lowercase letters, digits, and single hyphens. A driver is made for one
project, and lists and makes only its images, tags, and instances: a
project's `images`, `ls`, names, and tags are its own, and every image ID is
random, so two projects never share one.

The driver puts the project in the name of everything it makes
(`machine.ResourcePrefix`): boxd's machines and snapshots and docker's
containers are `dvm-<project>--...`, docker's images
`disco-vm-layer:<project>--...` and tags `disco-vm/<project>/...`; a local
driver records it in each `layer.json` and `instance.json`. `--` cannot occur
in a project, so a name belongs to exactly one project.

There are no orphans in disco-vm (ADR 26-10-23-417): everything under a
project's names is that project's, and is listed. A program that embeds the
engine and owns a set of its own (discobox's control plane) reaps against it,
as its pool agent reaps Docker: list the project's instances and images,
remove what it does not hold, treat a failed listing as no answer, and spare
what is younger than a grace period. A driver's own short-lived resources are listed as
instances while they run (docker's `base-<random>` readying container, boxd's
`save-<random>` machine), and a readying takes minutes, so the grace period
is longer than that.

## An instance runs straight from a driver's base

A driver may have what an instance can run from with no image: a base
(`machine.Base`; ADR 26-10-15-833). `Driver.Bases` lists what it can, and
`Driver.Base` finds one by name or ID, so a driver can have bases it cannot
list:

- **boxd**: its default image (`GetConfig`), and every ready snapshot in the
  account whose name does not start with `dvm-`, so nothing disco-vm made, in
  any project, is a base. Create makes the machine from the image
  (`CreateVm`) or the snapshot (`CreateVmFromSnapshot`), at the size asked
  for, and stops it.
- **docker**: any image the daemon has or its registry answers for
  (`/distribution`), found without a pull; it lists none. Create pulls it,
  adds systemd and the agent, shifts it into the guest's uids, and commits
  that as the instance's own Docker image, which Destroy removes; so every
  instance of a registry image readies it again (ADR 26-10-23-410), and a
  snapshot of one does not need to.
- hcs and vz have none; fake has the catalog a test gives it.

`images` lists what `Bases` lists on rows of its own, after the images, with
`-` for its ID. A ref goes to the images first, a tag or an image ID, then to
`Driver.Base` (`Engine.ResolveBase`), so a tag of the same name hides a base
and its ID still reaches it. `create` and `run` of a base make an instance
of the base instead of an image (`InstanceSpec.Base`), cold. `snapshot` of
such an instance is an image like any other. A base is not an image: `tag`
and `rmi` refuse it (`ErrBase`), since disco-vm never deletes what it did
not make.

## disco-vm does not converge guests

Deciding what an image holds is a client's, above
disco-vm ([ADR 26-10-15-703](../../docs/adr/26-10-15-703-disco-vm-does-not-converge-guests.md)).
disco-vm offers what a client converges with: exec and copy
(`Engine.Guest`), `Engine.Stop`, and `Engine.Snapshot`. A converged image is
an instance that was created, changed through exec and copy, stopped, and
snapshotted.
