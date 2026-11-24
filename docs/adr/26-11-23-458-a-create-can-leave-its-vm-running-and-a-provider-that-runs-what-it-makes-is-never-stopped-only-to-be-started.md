# 26-11-23-458 — A create can leave its VM running, and a provider that runs what it makes is never stopped only to be started

- **Status**: Accepted
- **Date**: 2026-11-23

Accepted by the owner by merging #220, 2026-11-24.

## Context

`machine.Driver.Create` (`pkg/machine/machine.go`) "makes a stopped VM ...
that boots from power-off", and `Boot` and `Start` are the only ways to run
one. No ADR asks for it: it is how the local hypervisors work, where a
create is a disk operation and a boot is separate work. A provider that runs
what it makes pays whole power cycles to honour it:

- **archil.** Archil starts a sandbox as soon as it is made or forked.
  `makeSandbox` (`pkg/machine/archil/archil.go`) waits for it to run, sets
  its fence, installs and starts systemd (ADR 26-11-20-447), then stops it
  (`stopAndWait`), and makes the data disk after, since a sandbox mounts its
  disks only when started. `engine.Create` then `engine.Start`, which is
  what `disco-vm run` does (`internal/cli/instances.go`), starts it again.
- **boxd.** `CreateVm` and `CreateVmFromSnapshot` run the machine they make.
  `Create` (`pkg/machine/boxd/boxd.go`) fences it, turns off its idle
  timers, pins or closes its proxy, then `powerOff`s it, and `Boot` starts
  it again.
- **discobox's pool agent** (`pool-agent/sandboxruntime`) calls
  `engine.Create` then `engine.Start`, and restarts a new box onto the files
  it places: three starts and two stops before a new archil box is ready,
  and a snapshot build (`machinesnapshot.go`), which creates, starts,
  shapes, stops, and commits, two more cycles.

Two things a create's stop does do, and must keep doing:

- **A copy boots cold** (ADR 26-10-15-851). A boxd snapshot holds a running
  machine's memory, and a restore resumes it (`docs/drivers/boxd.md`), so a
  VM created from a committed one on boxd is stopped and booted from its
  disk to be cold. An Archil fork boots its source's checkpointed disk, so
  it is cold already.
- **What needs a stopped VM keeps one**: `Commit` (ADR 26-11-03-606),
  `Upgrade` (ADR 26-11-03-782), `SaveCache` (ADR 26-11-02-180), and a
  create's `From`, which `engine.Create` and every driver refuse while it
  runs.

## Decision

### 1. A create may be asked to start its VM

`machine.InstanceSpec` gains `Start *BootOptions`. Nil is today's create: the
VM is stopped when Create returns, on every driver. Non-nil asks Create to
return it running, as `Start` with those options would: outliving this
process, attached by `Attach` from any process, its guest operations
answering, or, on a driver whose `Start` returns once its provider runs the
machine (boxd), returning `ErrUnreachable` until they do. It is the
create's, never the VM's: nothing records it, and an `Instance` carries
nothing of it.

`Create`'s signature is unchanged. The caller attaches to the machine it
asked for (`Attach`), as any other process would.

### 2. Every driver honours it, its own way

It is required of every driver, not a capability: every driver runs what it
makes, one way or another.

| driver | Create with `Start` |
|---|---|
| archil, fresh | the data disk made first, and mounted in the sandbox's create (`createSandbox.Mounts`); the fence set at create, the ingress mark after, systemd installed and settled as today; then it is returned running. One start. |
| archil, `From` | the fork, fenced, unexposed, its tokens revoked, systemd settled, as today; returned running. A fork boots with its source's mounts, and a committed source has none, so a VM with a data volume is stopped and started once with it mounted, unless Archil's fork takes mounts (§7). |
| boxd, fresh | as today, without the `powerOff`: the machine `CreateVm` booted from its base is the cold boot. One start. |
| boxd, `From` | as today, `powerOff` included, then started, so the copy boots cold (ADR 26-10-15-851). The caller's start is the one that remains; the cost is boxd's restore, not the contract's. |
| docker | the container created, then started. |
| hcs, vz, libkrun, fake | the disk made as today, then `Start`, in `pkg/machine/local`, once for all four. |

`BootOptions` are refused as `Start` refuses them (`GUI` on a driver without
`Display`), before anything is made. A start past `Capabilities.MaxRunning`
is `ErrCapacity`, as `Start`'s is.

### 3. A create asked to start that fails makes nothing

A Create that fails leaves nothing, today. One asked to start keeps that:
where the start fails (the machine does not run, systemd does not settle,
the guest does not answer by the context's end), the driver removes what it
made, as it does for any failed create, and returns the start's error. A
caller gets a running VM, or none, never a stopped one it did not ask for.

### 4. The engine and the CLI

- `engine.CreateOptions` gains `Start *StartOptions`. `engine.Create` checks
  them as `Start` does (`CheckGUI`), passes them to the driver, attaches, and
  writes the guest's `guest.ServiceFile` (`announceService`), trying again
  while the guest is unreachable, as `engine.Start` does. Where that fails,
  the engine removes the VM (`Driver.Destroy`), and its error says so, or,
  where the removal fails too, that the VM may still be there.
- `disco-vm run` creates with `Start`. `disco-vm create` stays a create
  without it: on archil it still starts and stops the sandbox, since a VM
  asked for stopped is one.
- `engine.Start`, `Stop`, `Commit`, `Upgrade`, and `SaveCache` are
  unchanged: each still needs, or leaves, the state it does today.

### 5. What does not change

- `Commit`, `Upgrade`, and `SaveCache` still need a stopped VM, and a create
  still refuses a running `From`.
- `Upgrade` still leaves its VM stopped; a VM upgraded on archil still pays
  for a stop (Deferred).
- A VM created from a committed one still boots cold.

### 6. Conformance

`pkg/machine/machinetest` gains, on every driver:

- **a VM created started**: `Create` with `Start` returns a VM whose `State`
  is running, that `Attach` reaches, whose guest runs a command (through
  `UntilReachable`), that stops in order, and is then stopped; once from the
  base, where the driver has one, and once from a committed VM, where it
  reports `Commit`, the second carrying a file its source wrote.
- **a create asked to start that is refused makes nothing**: `Start` with
  `GUI` on a driver without `Display` is `ErrUnsupported`, and the name is
  free after.
- The existing "created" case keeps its `StateStopped` and `ErrNotRunning`
  checks for a create without `Start`.

`internal/e2e`'s `run` exercises it through the CLI. The fake driver tests a
start that fails after the disk is made, and that nothing is left.

### 7. Left open, for implementation

- **Whether Archil's fork takes mounts.** If it does, a VM created from a
  committed one with a data volume is one start too; if not, it is a start,
  a stop, and a start, which the archil driver's brief records. Checked
  against Archil itself, not assumed.
- **The order of an archil fresh create's disk and sandbox**: the data disk
  now comes first, so the driver must still take the VM's name atomically
  and never remove a disk another create of the name made.

## Alternatives rejected

- **Create returns the VM in whatever state its provider leaves it.** It
  removes the stop with no option, but every caller then reads `State` to
  know what it has, `disco-vm create` leaves an archil or boxd machine
  running and billing where the user asked for none, and a snapshot build
  or a `From` that needs it stopped stops it itself: the cost moves to the
  caller rather than going away.
- **A new `Driver.Run(ctx, spec, opts) (Instance, Machine, error)`.** A
  second way to make a VM, which every driver implements by sharing its
  create, and every refusal `Create` makes repeated on it and in
  `machinetest`. One create with one option keeps one set of refusals.
- **Create returns the `Machine` it started.** Every one of the dozens of
  `Create` call sites and refusal tests changes for a value one `Attach`
  gives, at the cost of one read of the provider.
- **Leave an archil sandbox paused at create, and have `Start` resume
  it.** Archil keeps a paused sandbox's memory, so systemd would not boot
  again, but the VM is then paused, not stopped (ADR 26-11-21-438): the
  contract changes all the same, a fork and a commit need it stopped, and
  the caller still pays a pause and a resume it did not ask for.
- **Let a boxd copy resume its snapshot's memory.** It would save boxd's
  stop, but the copy would run its source's processes, unfenced until the
  fence is set, and be warm, which ADR 26-10-15-851 removed with its
  reasons. Revisiting that is its own ADR.
- **A capability saying which drivers start on create.** Every driver can;
  how cheaply is the driver's, and a caller that wants a running VM asks for
  one either way.

## Consequences

- A new archil VM made fresh is one start, not a start, a stop, and a start;
  one from a committed VM is one too, or two where its data volume needs
  Archil to mount it (§7). A new boxd VM made fresh saves a stop and a
  boot.
- `disco-vm run` that fails to start leaves no VM behind, where today it
  leaves a stopped one.
- discobox's pool agent creates with `Start` rather than calling `Start`
  after, and its snapshot build skips one cycle. Its place-then-restart on a
  box's first start is a separate change that this makes possible on archil.
- `DESIGN.md`'s proof table, each driver brief's `Create` row, and
  `machinetest` change with the implementation.
- Implementation, in order: the seam, `pkg/machine/local`, fake,
  `machinetest`, and the engine and CLI; archil; boxd; docker; then
  discobox's pool agent.

## Deferred

- **`Upgrade` that leaves its VM running.** On archil it makes the sandbox
  again and stops it. Revisit once a caller (discobox's box upgrade)
  measures the stop as what slows it, with the same option on `Upgrade`.
