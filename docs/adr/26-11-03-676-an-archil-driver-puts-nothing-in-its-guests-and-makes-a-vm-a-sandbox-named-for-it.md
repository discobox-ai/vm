# 26-11-03-676 — An archil driver puts nothing in its guests, and makes a VM a sandbox named for it

- **Status**: Proposed
- **Date**: 2026-11-03

## Context

Archil (archil.com) sells Firecracker microVM sandboxes behind a REST API,
beside disks that the provider mounts in them. Every finding below was
checked by hand against Archil's API on 2026-11-02, from an
`aws-us-east-1` account.

- **A sandbox runs an OCI image under Archil's own init.** PID 1 is
  `/opt/archil/init`. Archil does not run the image's entrypoint (an
  instance of `nginx:latest` boots with no nginx), and there is no systemd
  and no `/etc/machine-id`. Sandboxes are amd64 only.
- **Lifecycle:**
  - Create to running takes 4.8 s. A stop takes 2.4 s and checkpoints the
    root disk; a cold start takes 2.4 s.
  - A fork of a stopped sandbox branches its root disk and boots the copy
    at once, with the source's mounts, in 2.4 s. A fork can't set mounts
    and can't skip the boot; `start` replaces the mounts of a stopped
    sandbox.
  - A sandbox with forks can't be deleted (`409 sandbox_has_forks`).
  - A session lasts at most 24 hours, then Archil pauses it. The idle
    timer can be turned off (`idle_ttl_seconds: 0`).
- **A sandbox has no labels, and its name never changes.**
  - There is no rename and no update. `env` is fixed at create and is seen
    by every process.
  - Sandbox names are unique, and a duplicate is refused
    (`sandbox_name_conflict`).
  - Disk names are unique too, but creating a disk with a name that
    exists silently returns that disk.
  - An Archil image is built only from an OCI reference: nothing captures
    a sandbox as an image.
- **A guest can stop its own VM only by a reset.**
  - Firecracker on x86 has no ACPI power-off. A kernel power-off halts the
    guest, Archil goes on reporting `running`, and a stop of it ends
    `failed`.
  - `reboot(2)`'s restart command makes Firecracker exit. Archil then
    reports `stopped`, with "process exited normally", and commits a
    checkpoint that a later start reads back.
- **systemd runs as PID 1 of namespaces of its own** below Archil's init.
  Launched over the process API with `unshare --pid --fork --mount`, it
  outlives the session that launched it, reaches `running` with no failed
  unit, and sees the disks the provider mounted.
- **Guest operations go through a process API**, a WebSocket per process
  with a directory, variables, stdin, framed stdout and stderr, an exit
  code, a terminal and its resizes; a process runs as root. Only Archil's MIT-licensed TypeScript SDK implements it; there
  is no Go SDK. There is no file API.
- **A sandbox's mounts change only at a start.** `start` takes a new mount
  list; nothing else does, and a fork copies its source's. A disk can be
  deleted while a stopped sandbox lists it, and a fork of that sandbox is
  then refused (`disk ... not found`).
- **Other facts the driver relies on:**
  - A port is exposed publicly at `https://<port>-<route>.<zone>.archil.app`.
  - Egress is a policy of a default (`deny`) and an allow list.
  - API tokens never expire and reach the whole account.

## Decision

### 1. A driver, `archil`, built into every binary

It calls Archil's REST API in the region `$ARCHIL_REGION` names
(`aws-us-east-1` by default), with the key `Config.Keys` answers:
`ARCHIL_API_KEY`, which it names at `Register`, as boxd names its own
key. The REST client is the driver's own, written from Archil's OpenAPI
document, as docker's is from the Engine API. The process protocol is
ported from Archil's SDK into `pkg/machine/archil`.

**What is in a guest, and what runs there, is the caller's.** disco-vm
installs nothing, starts nothing, and changes nothing in it: not at
Create, not at Start, not at Commit.

### 2. A VM is a sandbox named for it, and keeps nothing else

As ADR 26-11-03-606 has it, there are only VMs:

- **Names:** a VM is the sandbox `dvm-<project>--<name>`. Archil keeps
  sandbox names unique, so a Create takes the name atomically, and
  `sandbox_name_conflict` is `machine.ErrExists`.
- **No records:** a listing reads the sandboxes with the project's prefix.
  Each one's fence and data volume are read from Archil.
- **Limits:** a sandbox's name is at most 63 characters, so a project and
  a name are at most 57 together (the data disk's name, six longer, fits a
  disk's 100). Longer is
  refused at Create, as boxd refuses a name too long for a machine.

### 3. A fresh VM runs the base, and a copy is a fork

- **A fresh VM** is a sandbox of the driver's base, the OCI image
  `machine.Config.Base` names, or Archil's default image when none is
  named, with its fence.
- **A VM created from another** is a fork of that one's sandbox, then
  fenced as the spec asks. The source must be stopped and have no mounts:
  a fork boots with its source's mounts, and would share its data disk.
  A fork keeps its source's size, so another size is refused.
- **Either way,** Create exposes port 7302, stops the sandbox, and makes
  the data disk once the name is taken. A data disk of that name with no
  sandbox is a VM caught mid-upgrade, or one a Destroy stopped before its
  last step: it is listed, so Create of its name is `ErrExists`. The data
  disk is mounted at every start.
- **Commit** leaves the stopped sandbox as it stands, with no mounts and
  no data disk. A sandbox with mounts is started once with none and
  stopped, since only a start changes a sandbox's mounts. Nothing records
  that it is committed: the caller does, and never starts it again.
- **Copies chain below the seam.** Destroy refuses, with
  `machine.ErrInUse`, a VM whose sandbox has forks, as hcs refuses a disk
  another VM differences over.

### 3a. An upgrade makes the sandbox again under its name

`Upgrade` (ADR 26-11-03-782) deletes the stopped VM's sandbox and makes it
again under the same name, fresh from the base or a fork of the committed
VM, at the old size and with the old fence, and mounts the same data disk
at each start. Archil can't rename a sandbox, so the old one goes first, and
everything that could refuse the upgrade (a running VM, a source gone,
running, mounted, or of another size, a VM with forks) is checked before it
does. An Instance from before still names the VM: a sandbox not found by ID
is found by name.

An upgrade stopped between the two leaves only the data disk, which is
listed as the VM, with no size and the strictest fence, `none`, since
Archil keeps nothing else of it. Boot and Start refuse it, Attach finds it
stopped, Destroy removes the disk, and Upgrade again finishes it, with the
size and fence of the Instance it is given. A VM with no data disk is gone
if its upgrade fails or stops after the sandbox is deleted, for any reason
(a base that doesn't pull, a fence that doesn't read back): nothing of it is
left to list, and the error says so. That is the one exception archil makes
to ADR 26-11-03-782 §3, which it can't keep without a record of the VM
elsewhere. A `failed` sandbox, a guest that powered itself off, is
upgraded as a stopped one.

### 4. The guest's operations are Archil's process API

disco-vm puts no agent in the guest, as boxd puts none (`Capabilities.Agent`
is off):

- **Exec** is a process with the request's directory, variables and
  terminal. Its argv is one quoted command line, and another account is
  `runuser`'s, with that account's HOME.
- **Copies** are tar over a process. A copy out writes one ready byte once
  it has found the path, so a missing path is an error, not an empty tar,
  then sends the tar a chunk at a time as the host asks for each, as
  Archil's SDK downloads a file: Archil drops a process's output that
  outruns its connection. An exec whose output Archil dropped fails.
- **Info** is `uname -m` and `hostname`.
- **Shutdown** is Archil's stop, which checkpoints the disk. A reboot is
  refused: Archil has none.
- **Start** returns once a process runs. A sandbox Archil paused at its
  24-hour limit is resumed first by Start. Attach leaves it paused until
  the machine's first operation, so a listing wakes nothing.
- **A guest that powers itself off** halts its VM, which Archil goes on
  reporting `running`; it is not supported, as on boxd. One that resets
  its VM is a stop.

### 5. What it lists

| capability | archil |
|---|---|
| guest OS | linux, amd64 |
| agent | no: Archil's process API |
| install | none |
| ports, isolation, display, shared directories | no |
| keys | none: `ErrCannotIssue`, as an Archil token never expires |
| egress | `none` and `allow`: `network.egress` with the default `deny`, read back at Create |
| base | an OCI image (`Config.Base`), or Archil's default |
| commit (ADR 26-11-03-868) | yes: a committed VM is its stopped sandbox, and a copy a fork of it |
| upgrade (ADR 26-11-03-782) | yes: the sandbox made again under its name, mounting the same data disk |
| volumes | `data`: a disk per VM, `dvm-<project>--<name>--data`, mounted at each start and deleted at commit or with the VM; no `cache` (ADR 26-11-02-180, Deferred) |
| service | 7302, exposed at Create; the URL is Archil's hostname for it, the same across stop and start, and the private URL is the URL |

## Alternatives rejected

- **Random sandbox names, with marker disks as records.** This was drafted
  while disco-vm had images: an instance became an image at a snapshot,
  and a sandbox can't be renamed to free its name. Each sandbox was
  `dvm-<project>-<key>`, and empty disks named for instances, images, and
  tags held what disco-vm knew. Disk names are no claim, so two Creates
  could take one name. ADR 26-11-03-606 dropped images, and a VM keeps
  its name when it is committed, so the sandbox's name is the VM's.
- **A disk marking a committed VM.** ADR 26-11-03-606 makes that the
  caller's record.
- **systemd booted by disco-vm, and the agent.** It works: systemd runs as
  PID 1 of namespaces of its own below Archil's init, reaches `running`,
  and a guest power-off becomes a VM reset (checked by hand, 2026-11-02).
  It was drafted and dropped: what a guest holds and runs is the caller's,
  not disco-vm's, and readying cost a package install per fresh VM of a
  base and an exec at every start.
- **Clearing `/etc/machine-id` at Commit.** It is the caller's, with the
  rest of the guest.
- **A committed VM as a copy of its root disk.** Archil has no API for it. A
  fork is instant, and its cost is the chain (§3).

## Consequences

- **Speed:** Create takes a boot and a stop, about 5 seconds, and Start
  about 2.5. Commit of a VM with a data volume takes one boot, only to
  drop the mounts.
- **The guest is the caller's:** a VM that needs systemd, or a service
  started at boot, has to arrange it itself; Archil runs no entrypoint.
- **Copies chain:** a VM can't be removed while a VM forked from it
  exists.
- **Archil can't tell a committed VM:** a committed VM is a stopped
  sandbox with no mounts, and Start would boot it. The caller never does.
- **The conformance suite** runs against Archil where
  `DISCO_VM_ARCHIL_IMAGE` names the base, with `ARCHIL_API_KEY` set; CI
  skips it. A driver brief, `docs/drivers/archil.md`, documents how the
  seam maps.
- **The protocol:** it is not Archil's published API, so a change to it
  breaks the driver until it is ported again.

## Deferred

- **Committed VMs that stand alone:** revisit if Archil lets a sandbox with
  forks be deleted.
- **The cache volume:** as ADR 26-11-02-180 defers it.
