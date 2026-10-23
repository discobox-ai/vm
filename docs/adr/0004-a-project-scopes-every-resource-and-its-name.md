# 0004 — A project scopes every resource, and is in the name of what a driver makes

- **Status**: Accepted (orphan listing superseded by 26-10-23-417)
- **Date**: 2026-10-15

## Context

discobox has projects, and disco-vm had none: one root's images and
instances, and every `dvm-` machine and snapshot in a boxd account, belonged
to one flat namespace. A remote driver's resources outlive the store that
records them. A deleted root, a crash between making a machine and recording
it, or a second host's root leaves machines and snapshots that nothing lists
and that are billed. Listing them needs a way to tell this store's
resources, and other stores' resources, from the account's other contents.

## Decision

Every engine works in one project: `--project`, `$DISCO_VM_PROJECT`, or
`Engine.Project`, `default` when none is named. A project is 1 to 32
lowercase letters, digits, and single hyphens.

- **Rows carry the project.** Layers, tags, instances, and warm stages each
  have a `project` column, and every read filters on it. Tags are keyed by
  project and ref, and an instance's name is unique within its project.
- **The project is in every layer's cache key**, except the default's, so
  projects never share a layer ID or a layer directory, and the default
  project's cache from before projects stays valid.
- **The driver names what it makes after the project**:
  `dvm-<project>--...` for boxd's machines and snapshots and docker's
  containers, `disco-vm-layer:<project>--...` for docker's images. A name
  under the driver's prefix with no `--` is from before projects, and the
  default project's.
- **`ls` and `images` list orphans**: what the driver holds under the
  project's names that no row of the project accounts for
  (`Driver.Orphans`). The driver is asked on every listing.

## Alternatives rejected

- **A root, or a database, per project.** Simple for the CLI, but an
  embedder keeps one database for all its projects (`engine.Open` takes the
  embedder's), and every project would then need a database of its own.
- **Layer IDs shared across projects, with the store keyed by project and
  ID.** Two projects building the same spec would share the layer's
  directory, and the driver's snapshot or image, so removing one project's
  image would delete the other's layer. Putting the project in the key keeps
  every layer owned by one project.
- **The project in a label or tag rather than the name.** A boxd snapshot has
  no labels and cannot be renamed, so its name is the only place to put it.
  A name also shows the project in `boxd ls` and `docker ps`.
- **A plain `-` after the project.** Project `a` would then claim project
  `a-b`'s names. `--` cannot occur in a project, so the split is exact.
- **Listing only from the store's own records.** That listing is exactly
  what cannot show a resource the store lost.

## Consequences

- An existing root is the default project's. Opening it upgrades the
  database once: `disco_vm_tags` is copied into `disco_vm_image_tags` and
  dropped, and the unique index on an instance's name alone is dropped.
- Machine names grow by the project and `--`. At 32 characters a boxd
  machine's name is 59, still a DNS label in its proxy's domain.
- A boxd tag snapshot made before projects is named differently from what a
  tag now makes, so it is listed as an orphan, and `rmi` with its name
  removes it.
- Every `ls` and `images` on boxd makes two more API calls, and lists
  nothing when the service cannot be reached; a warning says so.
- Something being made right now (an install's machine, a commit's
  snapshot) is listed as an orphan until it is recorded or released.
