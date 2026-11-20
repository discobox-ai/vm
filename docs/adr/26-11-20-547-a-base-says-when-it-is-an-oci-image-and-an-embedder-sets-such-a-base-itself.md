# 26-11-20-547 — A base says when it is an OCI image, and an embedder sets such a base itself

- **Status**: Proposed
- **Date**: 2026-11-20

## Context

A fresh VM starts from the driver's base, `machine.Config.Base`, which every
driver that takes one describes in `Definition.Base` (ADR 26-11-12-461).
What a base names differs by driver: an OCI image reference Archil or a
Docker daemon pulls (archil, docker), boxd's own image name, a Linux guest
image whose kernel, initrd and root are pulled as artifacts (libkrun, vz),
a name (fake). Nothing in the definition says which, so an embedder can
fill in a base only by knowing drivers by name.

discobox needs systemd in an Archil guest (discobox-ai/discobox#1055), which
the driver will start itself (vm#193). An Archil sandbox with no base runs
Archil's default image, `node:24-bookworm`, which has none. vm#194 asked
for disco-vm to build and publish a Debian image with systemd, dbus and
nftables and to pin it as the archil driver's default. discobox already
publishes such an image, `ghcr.io/discobox-ai/discobox-base`: public,
amd64 and arm64, Debian trixie with systemd, systemd-sysv, dbus and
nftables (and Docker). On Archil itself (2026-11-20), a fresh VM of
`discobox-base:v0.18.1` booted, and systemd, started as PID 1 of its own
PID and mount namespace under Archil's supervisor, reached `running` with
no failed unit, dbus on the bus, `nft` working, and came back after a stop
and a start.

## Decision

1. **A base that names an OCI image says so.** `ConfigField.Format` is
   `machine.OCIImage` (`"format": "oci-image"`) on `Definition.Base` of a
   driver whose base is an OCI image reference: archil and docker. A base
   only the driver knows the form of (libkrun's and vz's guest image, boxd's
   image name, fake's name) has none. `Format` is set on no configuration
   field, and an OCI image base is a string; `Register` refuses either.
2. **An embedder sets such a base itself.** An embedder that holds an image
   of a base's format sets `Config.Base` to it on every driver declaring
   that format, unless its own configuration names another. The driver's
   default, or its provider's, is then the command line's alone. discobox
   sets its pinned `discobox-base`.
3. **The image an embedder sets is one its provider can pull with no
   credential.** archil's provider pulls the base from Archil's cloud, with
   none of the embedder's credentials, dialer or trust roots, so an `oci-image`
   base an embedder sets on every such driver is public. A private, mirrored
   or local image is for a driver the embedder configures by name.
4. **The archil driver's default stays Archil's image.** disco-vm builds and
   pins no Archil base of its own.

## Alternatives rejected

- **disco-vm publishes `disco-vm-archil-base:vN` and pins it as archil's
  default** (vm#194 as filed). A second image with the same four packages
  as `discobox-base`, released on its own line, would be pinned in two
  places: discobox's choice of the guest it runs would be disco-vm's
  release to make. The owner chose discobox's image, set by discobox
  (2026-11-20).
- **discobox keys the base on the driver's name.** It would have to learn
  each new driver that takes an OCI image, and would set an image on a
  driver whose base is not one (libkrun's is a registry reference too, but
  to a guest image's artifacts).
- **A new `ConfigField.Type`, `oci-image`.** `Type` is the JSON type of a
  value, which discobox's form reads to render the field; the base is still a
  string.
- **Fill in only where the driver declares no default.** An empty
  `Default` cannot tell a driver with no base (docker refuses a fresh VM)
  from one whose provider has its own (archil's Archil image, boxd's
  default image), so the rule would reach boxd, whose base is no OCI image,
  and it would turn on a default the embedder does not want anyway: the
  embedder sets its guest wherever the format allows, whatever the
  provider's own default is.

## Consequences

- discobox reads `Definition.Base.Format` and pins `discobox-base` by
  digest; that is discobox's change, outside this repository.
- A new driver whose base is an OCI image declares `Format`, and
  `TestBaseFormat` holds every driver's to a list.
- disco-vm's command line is unchanged: `$DISCO_VM_BASE` or the driver's
  default.
