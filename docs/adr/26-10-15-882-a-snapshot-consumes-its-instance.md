# 26-10-15-882 — A snapshot commits a stopped instance as an image, and consumes the instance

- **Status**: Accepted
- **Date**: 2026-10-15

## Context

Images came only from a build spec: each layer is the disk of a temporary
instance that ran the layer's steps, shut down in order, and was committed.
A client that shapes a guest some other way, through exec and copy, had no
way to keep the result as an image.

Every driver already commits a stopped instance (`Driver.Commit`), and the
contract says the instance is destroyed afterwards, so a driver may move
rather than copy. The drivers rely on it. hcs and vz move the instance's disk
into the layer, fake moves its root, and boxd boots the stopped machine to
snapshot it, since boxd snapshots only a running machine. Only docker leaves
the instance as it was.

## Decision

`Engine.Snapshot` and `disco-vm snapshot INSTANCE [NAME[:TAG]]` commit a
stopped instance through the same `Engine.Commit` a build uses.

- **The instance must be stopped.** A running one is refused, and so is an
  invalid tag, before anything is touched, so a refused snapshot leaves the
  instance as it was. The layer is cut from an orderly shutdown, as a build's
  is.
- **The instance is consumed.** On success it is destroyed and forgotten. To
  go on from a snapshot, create an instance of it, and snapshot that in turn.
- **The layer sits on the instance's image layer, under a random ID.** Nothing
  reproducible made it, so it is never a cache hit. It is otherwise a layer
  like any other: tagged, listed, kept while something is built on it or runs
  it, and removed by `rmi`.

## Alternatives rejected

- **Keep the instance.** It is what `docker commit` does, but it is a second
  `Commit` behavior, one that every driver would have to prove. hcs and vz
  would have to copy a disk they now move, boxd would leave behind a machine
  it booted for the snapshot, and an instance that is still usable after a
  commit is not something `machinetest` checks today. Consuming the instance
  keeps one behavior that every build already exercises, and an instance of
  the snapshot gives back what keeping it would have.
- **Snapshot a running instance.** boxd can, and the others cannot without
  pausing the guest, and a disk taken from a running guest may have writes
  it has not flushed. A build already refuses to cache such a disk for the
  same reason.
- **Give the layer a content ID.** A build's layer ID hashes its inputs. A
  snapshot's input is whatever a client did to a guest, which disco-vm does
  not see, so a hash of it would claim a reproducibility it does not have.

## Consequences

- Clients that shape guests outside a build spec (converging state files,
  for one) make images with nothing more than `create`, exec, copy, `stop`,
  and `snapshot`.
- A snapshot costs the instance. Keeping working means a new instance of the
  snapshot, which is a clone of the image and so as fast as `run`.
- A driver's `Commit` is now reached from user commands as well as builds, on
  instances of any clone mode. One that cannot commit a mode refuses it, as
  hcs refuses a fork clone, and the instance is kept.

## Deferred

Keeping the instance after a snapshot. Revisit when a caller needs it and
every driver's `Commit` can leave its instance usable, proven in
`machinetest`.
