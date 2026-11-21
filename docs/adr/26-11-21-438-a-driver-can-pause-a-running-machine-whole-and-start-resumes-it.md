# 26-11-21-438 — A driver can pause a running machine whole, reported as a capability, and Start resumes it

- **Status**: Proposed (supersedes in part 26-11-03-676, its Attach of a
  sandbox Archil paused, read as running and resumed by its first
  operation)
- **Date**: 2026-11-21

## Context

discobox's ADR 26-11-21-395 (Proposed, discobox-ai/discobox#1274) has a
machine pool freeze a sleeping sandbox, as a Docker pool already pauses
one. Its §§1–2 ask disco-vm for a freeze reported as a capability, resumed
by `Start`, and leave its names, its paused state's spelling, and each
driver's clock handling to disco-vm. This ADR records that contract.

Today, at `92729b4`:

- **The seam has no pause.** `machine.Machine` (`pkg/machine/machine.go`)
  is `Guest`, `Kill`, `Done`, `Err` and `Dial`. `machine.Capabilities` has
  no field for it.
- **The engine has no paused state.** `engine.State`
  (`pkg/engine/engine.go`) is `Stopped`, `Running` or `Unknown`, read from
  `Driver.Attach`: a machine is `Running`, `ErrNotRunning` is `Stopped`,
  and any other error is `Unknown`. `Engine.Stop` attaches and shuts down
  in order, and returns nil on `ErrNotRunning`. `Commit`, `SaveCache` and
  `Remove` read `State` and refuse anything but `Stopped`.
- **The providers read a pause three ways:**
  - **boxd's `Attach`** returns `ErrNotRunning` for any status but
    `running` or `starting` (`pkg/machine/boxd/boxd.go`). A `suspended`,
    `standby` or `hibernated` machine reads as `Stopped`. Its boot wait
    (`untilRunning`) resumes `suspended` and `standby` with `ResumeVm` and
    wakes `hibernated` with `WakeVm`. `settle` turns boxd's auto-suspend
    and auto-hibernate off. No driver calls `SuspendVm`.
  - **archil's `Attach`** returns a machine for a `paused` or `pausing`
    sandbox and marks it paused. Its first exec or copy resumes it
    (`awake`, `pkg/machine/archil/guest.go`), and `Kill` stops it as it is
    (ADR 26-11-03-676). It reads as `Running`. `Start` resumes it
    (`POST /api/sandboxes/{id}/resume`). The driver's API client has no
    pause call (`pkg/machine/archil/api.go`).
  - **docker's `Attach`** reads `State.Running`, which a paused container
    keeps, so a paused container reads as `Running`
    (`pkg/machine/docker/docker.go`).
- **The hypervisors can pause, and nothing asks them to.** vz holds its VM
  in a shim (`pkg/machine/vz/machine_darwin.go`), and Code-Hex/vz has
  `VirtualMachine.Pause` and `Resume`. hcs binds no
  `HcsPauseComputeSystem` or `HcsResumeComputeSystem`
  (`pkg/machine/hcs/computecore_windows.go`). The shim's control API
  (`pkg/machine/local/shim.go`) has `/status`, `/stop`, `/kill`, and the
  protocol-2 routes, and no pause. libkrun's API has no pause.
- **discobox-ai/vm#196** asks for a distinct paused state, for a machine
  its provider paused, that `Start` alone undoes, and a fake test hook
  that pauses like a provider does. It is being delivered without an ADR
  of its own. This ADR settles how the seam reports a paused machine,
  which both issues need. #196's delivery and this ADR must agree before
  either lands.

## Decision

### 1. `Capabilities.Pause` reports a pause the caller can ask for

`machine.Capabilities` gains `Pause bool` (`json:"pause"`): the driver
pauses a running machine whole when asked (`Machine.Pause`), and `Start`
resumes it. A paused machine's guest runs nothing, its memory is kept, and
every process in it carries on after the resume. It is reported, never
probed, as `Commit` is (ADR 26-11-03-868).

A driver that does not report it may still find a machine its provider
paused (archil's session limit). It reads that machine as paused (§3),
and `Start` resumes it (§5).

### 2. `Machine.Pause` is required, and refuses without the capability

`machine.Machine` gains, beside `Kill`:

```go
// Pause freezes the running guest whole, and returns once its provider
// says it is paused.
Pause(ctx context.Context) error
```

- A driver that does not report `Capabilities.Pause` returns
  `ErrUnsupported`, and the machine runs on.
- A pause that fails leaves the machine running, or reports why it cannot
  tell (an API that stops answering, as `Kill` does).
- Pausing a paused machine does nothing, and returns nil. A pause the
  provider still has under way (Archil's `pausing`) is waited out.
- A paused machine has not stopped. `Done` stays open, and `Err` stays
  nil. A driver whose machine watches its provider (boxd, docker, archil)
  must not read a paused machine as stopped.
- A machine held by a shim of another build returns `ErrStale`, as its
  other operations do.

### 3. A paused machine reads as paused, through `ErrPaused`

`machine` gains `ErrPaused`: the VM's machine is paused, whoever paused
it. **It does not wrap `ErrNotRunning`.** Every caller that reads
`ErrNotRunning` as "stopped" (`Engine.Stop` returns nil on it) would
otherwise lose a paused machine.

`Driver.Attach` of a paused VM returns `ErrPaused` and no machine. Reading
it wakes nothing. Each driver maps its provider's word:

| Driver | Paused when the provider says |
|---|---|
| boxd | `suspended`, `standby`, `hibernated` |
| archil | `paused`, `pausing` |
| docker | `State.Paused` |
| vz, hcs, fake | the shim says it holds a paused machine |
| libkrun | never |

`engine` gains `Paused State = "paused"`. `Engine.State` maps `ErrPaused`
to it. `disco-vm ps` and `inspect` print it, and `ps` without `-a` lists
it, as it lists a running VM. `pkg/machine/served` carries `ErrPaused`
across its wire as it carries `ErrNotRunning`.

disco-vm does not record who paused a machine. A caller that needs to
know records it (discobox 395 §4).

### 4. `Engine.Pause` attaches and pauses

```go
// Pause freezes a running instance whole, so that Start resumes it.
func (e *Engine) Pause(ctx context.Context, inst Instance) error
```

- On a driver that does not report `Capabilities.Pause`, it returns an
  error wrapping `ErrUnsupported` that names the driver, before the driver
  is asked.
- Otherwise it attaches. A paused VM returns nil. A stopped one is an
  error saying it is not running. A running one is paused with
  `Machine.Pause`.
- The command line gains `disco-vm pause VM`, which calls it.

### 5. `Start` resumes a paused machine, with its clock current

`Driver.Start` of a paused VM resumes it, boots nothing, and returns once
its guest operations answer, as a start does. `Engine.Start` refuses only
`Running` (today's check), so it reaches the driver for a paused VM, and
writes the service file as at every start. Nothing else resumes a paused
machine: no guest operation, no attach, no listing. There is no resume
call of its own.

- **Archil's lazy resume goes.** Its `Attach` returns `ErrPaused` (§3), so
  no machine is handed out to resume at its first exec or copy. This
  supersedes that part of ADR 26-11-03-676 (§9).
- **`Boot` of a paused VM is refused,** with `ErrPaused`. It is already
  held, as a running VM is.
- **The guest's clock is current when `Start` returns.** A VM keeps its own
  clock, and a paused one falls behind by the time it was paused. A guest
  left behind reads its timers, its wakes, and its certificates against a
  stale time. After a resume, the guest's wall clock is within five
  seconds of the host's when `Start` returns. Each driver meets this in
  its own way (§10). A container shares its host's clock, so docker meets
  it with nothing.

### 6. What else does to a paused machine

| Operation | On a paused VM |
|---|---|
| `Engine.Stop` | resumes it with `Driver.Start`, then shuts it down in order and forces it off after the timeout, as for a running VM. If the resume fails, it returns that error, and the VM stays paused |
| `Engine.Remove` | refused without force, as for a running VM. With force, `Stop`, then `Destroy` |
| `Driver.Destroy` | removes it, stopping it first, as it does a running VM. The guest's unwritten state is lost, as with a running VM's |
| `Commit`, `Upgrade`, `SaveCache` | refused, since the VM is not stopped: the engine reads `Paused`, and each driver refuses a paused VM as it refuses a running one |
| `Engine.Guest`, `Dial`, `GUI` | refused, saying the VM is paused and naming `start` |
| `Service` | as while running: a URL that outlives a stop stays. Whether a request to it resumes the VM is the provider's (Archil resumes a paused sandbox for a request carrying its port token) |
| `MaxRunning` | a paused VM counts, since its hypervisor still holds its memory. A shim that holds it keeps its slot |
| `MaxMachines` | unchanged: it counts every VM |

A machine stopped while paused boots again from its disk, as any stopped
VM does.

### 7. Who pauses

| Driver | `Pause` | Resume (`Start`) | Reports `Capabilities.Pause` |
|---|---|---|---|
| boxd | `SuspendVm`, then wait for `suspended` | `StartVm`, then `ResumeVm` or `WakeVm` from the boot wait (today's path) | yes |
| vz | `VirtualMachine.Pause`, through a route of the shim's | `Resume`, through the same shim | yes |
| hcs | `HcsPauseComputeSystem`, through a route of the shim's | `HcsResumeComputeSystem`, through the same shim | yes |
| docker | `docker pause`, through the daemon's API | `docker unpause`, the same way | yes |
| fake | #196's hook, through a route of the shim's | through the same shim | yes, and a test-only option without it, as `NoCommit` is |
| archil | Archil's caller pause, if Archil has one (§10) | `POST .../resume` (today's path) | no, until a caller pause is seen on Archil itself |
| libkrun | `ErrUnsupported`: libkrun has no pause | — | no |

The shim's new routes move `shimProtocol`. An engine attached to a shim of
an older protocol already reads it as stale (`ErrStale`), so `Pause` on it
is refused and `Stop` still stops it.

boxd's `settle` keeps auto-suspend and auto-hibernate off. A pause is
asked for, never made by boxd's idle timers.

### 8. The tests follow the capability

- **`machinetest`**, on a driver that reports `Capabilities.Pause`:
  - start a VM and a long-lived process in it, and note the process's PID;
  - pause it: `Attach` returns `ErrPaused`, and a second `Pause` of the
    machine returns nil;
  - leave it paused for at least 30 seconds, then `Start` it: the process
    runs under the same PID, and the guest's wall clock is within five
    seconds of the host's;
  - pause it again, and stop it: it reads stopped, and boots again;
  - pause a VM and `Destroy` it: it is gone;
  - on a driver with `MaxRunning`, a paused VM holds its slot.
- **`machinetest`**, on a driver that does not report it: `Machine.Pause`
  returns `ErrUnsupported`, and the machine still runs.
- **`machinetest`**, on a driver with a way to pause a machine as its
  provider would (fake's hook, #196): `Attach` returns `ErrPaused`, and
  `Start` resumes it.
- **`pkg/engine`**, on fake: `Engine.Pause` refused without the
  capability; `Commit`, `Upgrade` and `SaveCache` refuse a paused VM;
  `Stop` and `Remove --force` act on one; `Guest` refuses one.
- **`internal/e2e`**: `pause`, `ps` reading `paused`, `start`, and `stop`
  of a paused VM; `pause` refused on a fake without the capability.

fake runs both ways in CI, as it does for `Commit`.

### 9. What this supersedes

This supersedes in part ADR 26-11-03-676: Attach of a sandbox Archil paused
reads as running, and its first exec or copy resumes it. Archil's `Attach`
returns `ErrPaused` instead, and `Start` resumes it. The rest of 676
stands. Once this is accepted, 676's status notes it.

### 10. Left to implementation, open

None of these is decided here, and none is decided by default.

- **How each driver keeps the guest's clock current at resume** (§5): a
  step the driver runs through the guest after the resume (OS-specific
  code in `pkg/guest`, never in the engine), the hypervisor's own handling
  (Hyper-V's time synchronization integration service on hcs), or the
  guest's clock source. The conformance case decides whether each driver
  passes. A driver that fails it does not report `Capabilities.Pause`.
- **How a shim tells its paused machine** to an `Attach` from another
  process: a field of `/status`, or a route of the protocol's.
- **Whether Archil offers a caller pause.** If it does: whether a paused
  sandbox still runs down its 24-hour session, and whether Archil stops
  one left paused too long.
- **boxd's states around a pause:** whether there is a `suspending`, what
  `SuspendVm` answers for a machine still starting, whether `DeleteVm` takes
  a suspended machine as it is, and whether a suspended machine survives
  boxd's host maintenance. One that does not reads as stopped, which is
  safe.
- **What fake's pause holds,** so that its PID survives on every OS (its
  guest is a host process).

## Alternatives rejected

- **`ErrPaused` wrapping `ErrNotRunning`.** It would leave every
  `errors.Is(err, ErrNotRunning)` check answering as before. But
  `Engine.Stop` returns nil on that error, so a stop would leave a paused
  VM paused while reporting it stopped. `Remove`, `Commit`, and the served
  client read the same error as stopped too. A paused VM is held, not
  stopped, and every check has to say which it means.
- **`Attach` returns a machine for a paused VM, as archil and docker do
  today,** with a method on it that says it is paused. `State` would then
  need that method too, and its answer goes stale the moment the machine is
  paused or resumed. Archil's form, a machine that resumes at its first
  operation, makes any guest call a resume. That is the second way to undo
  a pause that #196 rules out, and it wakes a sandbox discobox froze on
  purpose.
- **`Pause` on `machine.Driver`, taking an `Instance`.** The attached
  machine already reaches the guest through the provider's API or the shim
  that holds it, and `Kill` lives there for the same reason. A driver
  method would repeat `Attach`'s lookup in every driver.
- **An optional interface** (`machine.Pauser`, found by type assertion). A
  caller outside the Go process sees a driver only through what it reports
  (`disco-vm info`). The repository's rule is a required method and a
  capability bit, as for `Commit` (ADR 26-11-03-868).
- **A resume call of its own** (`Machine.Resume`, `Engine.Resume`).
  `Start` already resumes a provider's pause on boxd and archil, and #196
  makes it the one way to undo one. A second way would make every caller
  choose between them, and a caller that chose wrong would boot or refuse
  where it meant to resume.
- **Stop of a paused VM by killing it.** It is faster, and every provider
  can kill a paused machine. But a guest killed while paused loses
  everything it had not written to disk, a data volume's writes included,
  where an orderly shutdown after a resume flushes them. `Destroy` kills,
  because it removes the disk anyway.
- **The name `Suspend`** (boxd's word, and discobox's `suspended` status).
  vz, HCS and Docker all call it pause. boxd's `suspended` sits beside
  `hibernated`, which keeps memory on disk. discobox's `suspended` is a
  sandbox's status, and `paused` is a machine's.
- **Leave the clock to the guest's NTP client.** chrony or
  `systemd-timesyncd` would slew it back over minutes, or step it only at
  their next poll. A caller acting as soon as `Start` returns would act
  against a stale time first, and not every guest runs one.
- **Freeze inside the guest** (the cgroup freezer). discobox 395 rejected
  it: the machine still runs on its provider, against its caps and its
  bill.

## Consequences

- **The seam, every driver, the engine, the shim, `served`, the CLI,
  `machinetest`, `pkg/engine`, and `internal/e2e` change together.**
  libkrun and archil gain a `Pause` that returns `ErrUnsupported`.
- **boxd's suspended machines stop reading as stopped.** A caller that
  read a machine boxd suspended as stopped now reads it paused, and starts
  it to resume it, as before.
- **archil's paused sandboxes stop reading as running.** A caller that ran
  a command in one and relied on it waking (discobox's resume of a pause it
  did not ask for, 619 §5) calls `Start` first. That is what 395 §4 has
  discobox do.
- **discobox's machine pool** reads `Capabilities.Pause` once and freezes a
  sleep where it is reported, as 395 §3 decides.
- **A paused remote machine holds its memory on its provider,** and may be
  billed for it, until it is resumed or stopped. Nothing here stops it.
- **`DESIGN.md`** (root, `pkg/machine`, `pkg/engine`, `pkg/machine/local`,
  and the boxd, archil, docker and vz package files) gains the capability,
  and the proof table gains a row for it, in the change that implements
  it. `docs/drivers/*.md` say which driver pauses.
