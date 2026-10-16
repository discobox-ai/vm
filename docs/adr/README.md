# Architecture Decision Records

Nygard-style ADRs: the decision, its context, and the alternatives rejected.
Follow `template.md` and the `repostd` skill.

- Write one only when a plausible alternative was rejected for a non-obvious
  reason, or something was deferred with a condition for revisiting it.
- Land it as `Proposed` before implementation; flipping it to `Accepted` is
  the decision gate.
- Amend while nothing has shipped against it; supersede after.
- ADRs are history. `DESIGN.md` is current state.

## Index

<!-- repostd:adr-index -->
| ADR | Title | Status |
|---|---|---|
| [0001](0001-a-driver-runs-guest-operations-and-exposes-one-service-endpoint.md) | A driver runs guest operations itself and exposes one service endpoint | Accepted (§1's boxd shutdown superseded by 26-10-15-833) |
| [0002](0002-docker-guests-make-their-own-user-namespace.md) | Docker guests make their own user namespace, and the base layer is shifted once | Proposed |
| [0003](0003-a-client-converges-a-guest-to-state-files.md) | A client converges a guest to state files, through exec and copy | Superseded by [26-10-15-703](26-10-15-703-disco-vm-does-not-converge-guests.md) |
| [0004](0004-a-project-scopes-every-resource-and-its-name.md) | A project scopes every resource, and is in the name of what a driver makes | Accepted |
| [0005](0005-every-instance-has-one-http-url-and-the-driver-picks-its-port.md) | Every instance has one HTTP URL, and the driver picks where its service listens | Proposed |
| [26-10-15-703](26-10-15-703-disco-vm-does-not-converge-guests.md) | disco-vm does not converge guests; a client above it does, through exec, copy, and snapshot | Accepted |
| [26-10-15-833](26-10-15-833-an-instance-runs-straight-from-a-drivers-base.md) | An instance runs straight from a driver's base, and boxd's power state is boxd's | Accepted (supersedes in part 0001, its boxd shutdown) |
| [26-10-15-851](26-10-15-851-images-are-made-by-build-and-snapshot-and-clones-are-cold.md) | Images are made by build and snapshot, and every clone boots cold | Accepted |
| [26-10-15-882](26-10-15-882-a-snapshot-consumes-its-instance.md) | A snapshot commits a stopped instance as an image, and consumes the instance | Accepted |
<!-- /repostd:adr-index -->
