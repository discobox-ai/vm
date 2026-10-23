# pkg/engine

Instances and their lifecycle, above the driver seam. The engine is OS-neutral:
everything it does, it does through `machine.Driver` and `machine.Guest`, and
the fake driver tests all of it on every OS.

## No daemon: one shim process per running VM

The two hypervisors disagree about who owns a running VM. A
Virtualization.framework VM dies with the process that created it, and an HCS
VM belongs to vmcompute. A **shim per VM** makes both look the same:

- `start` spawns `disco-vm shim <id>` detached (setsid, or DETACHED_PROCESS).
- The shim boots the VM, holds it for its whole life, and exits when the VM
  stops.
- The shim writes `instances/<id>/shim.json` (pid, loopback address, bearer
  token) once its machine answers, and serves `/status`, `/stop`, and
  `/guest`. `/guest` upgrades to the agent protocol, which the shim answers
  from its machine's guest operations (`guest.Relay`): `Engine.Guest`.
- Any process (a later CLI invocation, a discobox server) reaches any instance
  through the shim. A crash is contained to one VM.
- The shim exits when the VM stops.

A **remote driver** (`Capabilities.Remote`: boxd, docker) runs no shim: its
service (boxd's API, the Docker daemon) owns the VM, so each call attaches
(`machine.Attacher`) with its own credentials. See
[docs/drivers/boxd.md](../../docs/drivers/boxd.md) and
[docs/drivers/docker.md](../../docs/drivers/docker.md).

A program that embeds the engine (discobox) starts its shims as a subcommand of
its own binary: `Engine.ShimCommand` names it, and that subcommand calls
`Engine.ShimMain`. So an embedder ships one binary, and its VMs outlive it.

## Every instance has one service, at one HTTP URL

The driver, not the caller or the image, picks where an instance's one
service listens in the guest (`Driver.Service`, `machine.Service`;
[ADR 0005](../../docs/adr/0005-every-instance-has-one-http-url-and-the-driver-picks-its-port.md)):
`vsock:7302` on vz and hcs (`guest.ServicePort`, listed in every hcs VM's
ServiceTable), `tcp:0.0.0.0:7302` on boxd and docker, and a loopback port
picked at Prepare on fake, whose guests share the host's. A build spec
declares no service.

At every start, before it returns, the engine writes that address with the
guest's CopyTo to its `guest.ServiceFile` (`/var/lib/disco-vm/service`, or
`C:\ProgramData\disco-vm\service`), on disk so it outlives a reboot in the
guest. A program the caller copies in reads it, or calls
`guest.ListenService()`, and listens there.

`Engine.URL` and `disco-vm url INSTANCE` give the service's URL, always HTTP:

- **The driver's**, where it has one: boxd's `https://<machine>.boxd.sh`,
  pinned to 7302 at Prepare and the same across stop and start; Docker's
  `http://127.0.0.1:N`, a port the daemon publishes at each start.
- **The shim's** everywhere else: `http://127.0.0.1:N`, a loopback port the
  shim binds at each start and splices, connection by connection, into
  `machine.Dialer`'s `Dial(7302)`. It is in `shim.json`, and there is none
  while the instance is stopped.

No URL takes a credential, so the service authenticates its callers.

## A project scopes everything, and the driver's leftovers are listed

`--project` (`$DISCO_VM_PROJECT`, `Engine.Project`; `default` when not given; ADR 0004)
names the project a command works in. A project is 1 to 32 lowercase letters,
digits, and single hyphens. Every row has one: a project's `images`, `ls`,
names, and tags are its own, and an instance's name is unique within it.
Projects share a root's directories, so a project other than the default is
part of every layer's cache key, and two projects never share a layer ID. The
default project's keys are those from before projects, so its cache stays
valid. A shim is started with the engine's project.

The driver puts the project in the name of everything it makes outside the
store (`machine.ResourcePrefix`): boxd's machines and snapshots and docker's
containers are `dvm-<project>--...`, and docker's layer images are tagged
`disco-vm-layer:<project>--...`. `--` cannot occur in a project, so a name
belongs to exactly one project. A name from before projects, with no `--`,
is the default project's.

`ls` and `images` ask the driver every time for what it holds under the
project's names that the store has no record of (`Driver.Orphans`, given
`machine.Known`): a store that was deleted, a crash between making something
and recording it, another host's store, or something being made right now,
such as an install's machine. They are listed as `<orphan>` rows, stopped or
not, so nothing billed is out of sight. `rmi NAME` deletes an orphaned layer
and `rm NAME` an orphaned machine (`Engine.RemoveOrphan`,
`Driver.RemoveOrphan`), each only after asking the driver again that NAME is
an orphan of the project now, so nothing the store records and nothing of
another project's is deleted this way; a running machine takes `-f`. A
driver whose resources
live only in the store's directories (hcs, vz, fake) has none.

## An instance runs straight from a driver's base

A driver lists what it already has that an instance can run from with no
layer (`Driver.Bases`, `machine.Base`; ADR 26-10-15-833): boxd's default
image (`GetConfig`), and every ready snapshot in the account whose name does
not start with `dvm-`, so no project's layers or orphans are bases. The other
drivers have none. `images` lists each on a row of its own, after the store's
images, with `-` for its layer.

A ref goes to the store first, a tag or a layer ID, then to the driver's
bases by name or ID (`Engine.Base`), so a tag of the same name hides a base
and its ID still reaches it. `create` and `run` of a base make an instance
that records the base instead of a layer (`Instance.Base`,
`InstanceSpec.Base`, an empty `Chain`), cold, with nothing installed. A
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
