# 26-10-15-833 — An instance runs straight from a driver's base, and boxd's power state is boxd's

- **Status**: Accepted (supersedes in part 0001, its boxd shutdown)
- **Date**: 2026-10-15

## Context

An image comes only from the local store, and the store gets a base layer
only from a build's `from.install`. On boxd that means a `build` before
anything can `run`, although boxd already has machines to start from: its
default image (`GetConfig.default_image`), and the snapshots in the account
that disco-vm did not make (`boxd snap create`, or another tool's). Its
public API has no image catalog, so those two are all there is to list.

`images` already asks the driver on every call for what it holds outside the
store (`Driver.Orphans`): the project's own `dvm-<project>--` snapshots that
the store does not record. Those are disco-vm's leftovers, and `rmi` deletes
them; they are not bases.

A boxd layer is more than a snapshot today. Install puts a systemd shutdown
hook in the guest (ADR 0001), and the driver polls for its marker to learn
that a guest powered itself off, because boxd leaves a halted guest
`running`. A raw image or a hand-made snapshot has no hook, which is the one
reason a base would need installing as a layer before it could run. Nothing
of disco-vm's relies on a guest powering itself off: an orderly shutdown is
the engine's `Shutdown`, and the driver can do that through boxd's API.

## Decision

- **The seam gains a required method, `Driver.Bases(ctx) ([]machine.Base,
  error)`.** A base is something the driver can make a machine from with no
  layer: a name, an ID, a version, the guest OS, and when it was made. boxd
  returns its default image and every ready snapshot in the caller's context
  whose name does not start with `dvm-`, so no project's layers or orphans
  are bases. hcs, vz, and docker return none. fake returns the bases it is
  given, so the engine path is tested on every OS.
- **An instance runs from a base directly.** A reference the store does not
  have is looked up among the driver's bases, by name or ID, and the
  instance records the base instead of a layer (`InstanceSpec.Base`, an empty
  `Chain`). Nothing is installed and no layer is made. On boxd, Prepare only
  records the base; the first Boot makes the machine (`CreateVm` with the
  image, or `CreateVmFromSnapshot`), at the size it asks for, and pins its
  proxy. So a first `run` is one boot.
- **What builds on an instance builds on a base.** A build's `from.image` may
  name a base: its first layer is committed with no parent, and its cache key
  names the base's driver, ID, and version in place of a parent layer.
  `snapshot` of an instance of a base is a layer with no parent too.
- **A base is not a layer.** `images` lists bases on rows of their own, after
  the store's images, with `-` for the layer. `tag` of a base is refused,
  saying to `snapshot` an instance of it for a layer, and `rmi` of a base is
  refused: disco-vm never deletes what it did not make.
- **boxd's power state is boxd's.** The driver puts nothing in the guest: no
  agent and no shutdown hook. A machine's state is `GetVm`'s status, read as it
  is. `Shutdown` runs `sync` in the guest over Exec, then `StopVm`, since
  boxd does not say whether `StopVm` is a clean shutdown; a reboot is
  `RebootVm`. Kill is `StopVm`. A cold clone restores the layer's snapshot and
  stops it the same way. A guest that powers itself off from inside
  (`systemctl poweroff`) is not supported: boxd leaves it `running`, and so
  does disco-vm, until `disco-vm stop`.

## Alternatives rejected

- **Install a base as a layer on its first use.** It was this ADR's first
  draft: one ordinary layer that `tag` handles unchanged. But the
  install exists only to add the shutdown hook, and costs a boot, a snapshot,
  and a restore before the first `run` (two minutes on boxd, 2026-10-15). With
  no hook there is nothing to install.
- **Keep the shutdown hook.** It is what tells the driver that a guest
  halted itself, and nothing needs to know that: disco-vm stops a machine
  through boxd's API, and every image would otherwise carry disco-vm's file
  and every base would need it added.
- **Mask `poweroff.target` in the guest, so a power-off from inside fails
  loudly.** It puts something of disco-vm's back in the guest, which is the
  thing being removed.
- **Detect a halted guest by an Exec that cannot reach it.** A probe on every
  status, and a timeout is a guess.
- **`StopVm` alone.** Fastest, but if boxd's stop is a hard power-off, a
  commit or a snapshot can lose writes the guest had not flushed.
- **An optional interface (`machine.BaseLister`).** The engine needs every
  driver to answer, and the repository's rule is a required method with an
  empty answer.
- **List every snapshot in the account, `dvm-` ones included.** Those are
  disco-vm's layers, of this project or another's, and listing another
  project's would break the scoping of ADR 0004.

## Consequences

- On boxd, `disco-vm images` then `disco-vm run` with the default image's
  name, or a snapshot's name or ID, works with no build, in one boot.
- An instance has a layer or a base, and the engine, the build, the store's
  `LayerInUse`, and the CLI handle both. A row from before has a layer.
- Every cold `run` on boxd gets faster too: a cold clone no longer waits for
  the hook's marker.
- `images` makes one more boxd call (`GetConfig`) and lists snapshots twice,
  once for bases and once for orphans. Neither has paging.
- A base's name is resolved after the store's refs, so a local tag of the
  same name hides it; its ID still reaches it.
- An image built before this still has the hook, and the old agent if it is
  older still. Nothing reads either; they do no harm.
- `exec dev systemctl poweroff` leaves a dead guest that `ps` calls running;
  `disco-vm stop` stops it.
- `machinetest` checks that every base a driver lists names itself, and that
  an instance of the first boots; an account may hold many snapshots.
