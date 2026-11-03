# 26-11-03-868 — Commit is a capability a driver reports, and a driver without it refuses commit and copies

- **Status**: Accepted (supersedes in part 26-11-03-606, its §4 requiring
  `Commit` and `InstanceSpec.From` of every driver)
- **Date**: 2026-11-03

Accepted by the owner by merging #34, 2026-11-03.

## Context

ADR 26-11-03-606 §4 gives every driver `Commit(ctx, inst)`, and
`InstanceSpec.From` on `Create`, and makes both required. Every driver on
`main` implements them today:

- boxd (`pkg/machine/boxd/boxd.go`), as a snapshot;
- docker (`pkg/machine/docker/docker.go`), as an image;
- hcs, vz, and fake, through `local.Driver.Commit`
  (`pkg/machine/local/driver.go`), as the VM's record and disk left in place.

`machine.Capabilities` (`pkg/machine/machine.go`) has no field for it.
`Engine.Commit` (`pkg/engine/engine.go`) checks only that the VM is stopped
before calling the driver, and `Engine.Create` with `From` checks only that
the source is stopped. The conformance suite
(`pkg/machine/machinetest/machinetest.go`) uses `driver.Commit` as a
fixture: its first subtest commits a VM, and most later subtests create
from it, so a driver without `Commit` fails nearly the whole suite.
`internal/e2e`'s `TestCommit` assumes it too.

Not every provider can freeze a VM and copy from it. The seam already has
a rule for that (`pkg/machine/machine.go`, package doc): a difference
between providers is reported through `Capabilities`, and "a driver that
lacks one returns ErrUnsupported instead of emulating it badly." Commit is
the one operation a provider may lack that the seam still makes required.

The caller that needs this settled is discobox's machine pool (discobox ADR
26-11-03-986, Proposed). A pool builds one snapshot per plan: create a VM,
provision it, stop it, `Commit`, then create each sandbox `From` it. On a
driver without commit the pool builds no snapshot and makes every sandbox
fresh, provisioning each. It must know which before it starts, because
provisioning takes up to 45 minutes. The pool keeps its own record of what
it built, so it needs no more from `Commit` than that it exists.

606 is Accepted and shipped against (`247f378`), so it is superseded in
part here, not amended.

## Decision

### 1. `Capabilities.Commit` reports commit and copy together

`machine.Capabilities` gains `Commit bool` (`json:"commit"`): the driver
can commit a stopped VM (`Driver.Commit`) and create a VM from a committed
one (`InstanceSpec.From`). The two go together because neither is any use
without the other: a committed VM exists only to be copied, and a copy
needs a committed VM. It is reported, as every capability is, never probed.

`Driver.Commit` stays on the interface. A driver that does not report
`Capabilities.Commit` implements it by returning `ErrUnsupported`.

### 2. A driver without it refuses, and changes nothing

On a driver that does not report `Capabilities.Commit`:

- `Driver.Commit` returns `machine.ErrUnsupported`, and leaves the VM as it
  was: running or stopped, with its volumes.
- `Driver.Create` with a non-nil `InstanceSpec.From` returns
  `machine.ErrUnsupported`, and makes nothing.
- Every VM it lists is a VM it can boot. `Instance.From` is always empty.

The engine refuses before it reaches the driver, as it does for
`Isolation` and `Egress`: `Engine.Commit`, and `Engine.Create` with
`CreateOptions.From`, return an error wrapping `machine.ErrUnsupported`
that names the driver. The command line inherits it: `commit VM`, and
`--from` on `create` and `run`, fail with that error.

`Destroy` is unchanged: it returns only once the provider no longer has the
VM. A pool on a driver without commit relies on that to replace sandboxes,
since it has nothing else to recycle.

### 3. A driver that makes VMs only from committed ones must report it

A driver with no base (606 §3: hcs, and vz for macOS) makes VMs only by
`Install` and from committed VMs, and `Install` leaves a committed VM. Such
a driver is useless without commit. So:

- a driver that lists any OS in `Capabilities.Install` reports `Commit`;
- a driver with no base reports `Commit`.

`machinetest` asserts both: if `Install` is non-empty, or a fresh `Create`
returns `ErrUnsupported`, then `Commit` must be reported.

### 4. Who reports it today

Every driver on `main` reports `Commit`: boxd, docker, hcs, vz, and fake.
Nothing changes for their callers. fake can be built without it (a
test-only option), so the paths in §2 and the skips in §5 run on every OS
in CI.

### 5. The tests follow the capability

- **`machinetest`** runs its commit cases (commit, a committed VM found and
  listed, create from one, a copy of a copy outliving or refused on the
  first's removal) only on a driver that reports `Commit`. Everything else
  starts from a fresh VM of the driver's base, or from `Config.From` or an
  install where the suite is given one. On a driver without it, the suite
  checks §2: `Commit` and `Create` with `From` return `ErrUnsupported` and
  leave nothing behind.
- **`internal/e2e`** skips `TestCommit` on a driver that does not report
  it, and checks that the CLI's `commit` and `--from` refuse.

How the suite is restructured to drop its dependence on a committed
fixture is left to implementation.

### 6. What this supersedes

This supersedes in part ADR 26-11-03-606 §4: `Commit` and
`InstanceSpec.From` are required of every driver. Once this is accepted,
606's status notes it. 606's model (only VMs, commit as freezing in place,
the caller recording what it committed) stands.

## Alternatives rejected

- **Find out by trying.** A caller provisions a VM, commits it, and falls
  back to fresh VMs on `ErrUnsupported`. That is how the answer arrives
  after the cost it was meant to avoid: a full provisioning build, up to 45
  minutes, then thrown away. It also cannot tell a driver without commit
  from one whose commit failed this time. Capabilities exist so a pool
  plans before it spends.
- **Amend 606 instead.** 606 is Accepted and `main` implements it
  (`247f378`). ADRs shipped against are superseded, not edited
  (`docs/adr/README.md`).
- **An optional interface** (`machine.Committer`, found by type assertion).
  A caller outside the Go process sees a driver only through what it
  reports: `disco-vm info` prints `Capabilities` as JSON
  (`internal/cli/instances.go`), and a type assertion shows there as
  nothing. An optional interface would also split what a driver can do
  between two places, and the repository's rules keep required methods on
  the core interface.
  `Driver.Commit` stays required, and returns `ErrUnsupported`, as `Install`
  does on a driver that lists no OS in `Capabilities.Install`.
- **Two capabilities, one for commit and one for creating from.** Neither
  works without the other on any provider we know of. One field cannot be
  set inconsistently.
- **Emulate commit on a provider that lacks it.** Keep the stopped VM as
  the "committed" one, and make a copy by re-provisioning or by copying
  files through the guest. A copy would be slow, would differ from its
  source, and would hide that from the caller. This is the "emulating it
  badly" the seam already rules out.
- **`ErrNotImplemented` instead of `ErrUnsupported`.** That error means
  work not written yet, on a provider that could do it. A driver without
  commit is one whose provider cannot.

## Consequences

- **The seam, every driver, the engine, the CLI, `machinetest`, and
  `internal/e2e` change together**, though no driver on `main` changes
  behavior: each reports `Commit`.
- **fake runs both ways in CI**, so the refusals and skips are tested on
  every OS, not only once a driver without commit exists.
- **A driver whose provider cannot freeze a VM can now join the seam**
  without emulating it, as long as it has a base (§3).
- **discobox's pool** reads `Capabilities.Commit` once, and builds a
  snapshot per plan only where it is reported. Where it is not, every
  sandbox is fresh and provisioned, and the pool's warm run has no
  `commit`.
- **`DESIGN.md`** (root, `pkg/machine`, `pkg/engine`) and its driver table
  gain the capability in the change that implements it.
