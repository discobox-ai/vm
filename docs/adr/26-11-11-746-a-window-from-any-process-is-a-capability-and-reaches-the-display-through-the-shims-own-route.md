# 26-11-11-746 — A window from any process is a capability, and reaches the guest's display through the shim's own route

- **Status**: Proposed (supersedes in part 26-11-02-274 §1, its one sign-in
  for `TERMSRV/127.0.0.1`, and §3 and §5, the optional `machine.Viewer` and
  `local.MachineViewer`)
- **Date**: 2026-11-11

## Context

ADR 26-11-02-274 gave hcs a window on a running instance from any process
(`disco-vm gui`) and on an install in progress (`gui --install`), as an
optional interface a driver may implement (`machine.Viewer`) and one a
local hypervisor may implement (`local.MachineViewer`). The engine found the
behavior by a type assertion.

Two things went wrong once it merged (discobox-ai/vm#74):

- `gui` on a running instance cannot show the agent's display stream. The
  window opens in the `gui` process, which reaches the machine through its
  shim; the stream is guest port 7301 (`guest.DisplayPort`); and since ADR
  26-11-03-247 the shim's `/dial`, like every `Machine.Dial` a caller makes,
  takes the agent's port and the service's alone (`guest.DialablePort`).
  Only the shim's own window (`start --gui`) dials 7301, because the shim
  holds the machine.
- `AGENTS.md` asks that behavior the system requires not be an optional
  interface: a capability is a `Capabilities` field, and every driver
  implements the method.

Out of scope: how a window signs in to an image installed with a password
of its own (`--option password`), which is decided on discobox-ai/vm#74.

## Decision

1. **`Capabilities.View`** reports a window on a running instance from any
   process and on an install in progress. `machine.Driver` gains `View` and
   `ViewInstall`, and `local.Hypervisor` gains them too. Every driver and
   hypervisor implements them; a driver without `View` returns
   `ErrUnsupported`, and a hypervisor without it is never asked (§4).
   The engine checks the field, not a type. hcs reports it; vz, whose window
   needs the VM object the shim holds, and every other driver, do not.
2. **The shim serves the display on a route of its own, `/display`**, which
   takes no port and splices the request into its machine's `Dial` of
   `guest.DisplayPort`. It is served only by a hypervisor that reports
   `View`, behind the shim's token like every route. `local.Hypervisor.View`
   is given a function that opens the display stream: the shim's `/display`
   for an attached machine, the machine's own `Dial` in the shim and in an
   install. `guest.DialablePort` and `/dial` do not change. A shim from
   before the route answers 404, which reads "restart the instance".
3. **Each Remote Desktop window signs in at a loopback address of its own.**
   mstsc finds a stored sign-in by host alone (`TERMSRV/HOST`, no port), so
   a window claims the first of `127.77.0.1` to `127.77.0.254` by binding a
   fixed port on it, stores its sign-in there for as long as it is open, and
   then removes it and mstsc's record of the host. One the user stored is never
   written over: that address is skipped.
4. **vz still has no window after boot, as 26-11-02-274 defers.** With
   `View` false the engine and the local driver refuse `gui` as
   `ErrUnsupported`, the field's contract, as for `Commit`; vz's own `View`
   and `ViewInstall` say `ErrNotImplemented`, not yet. The deferral stands:
   lifting it is vz reporting `View`.

## Alternatives rejected

- **Add 7301 to `DialablePort`.** Every caller of `Engine.Dial`,
  `disco-vm dial`, and a peer forward could then reach a Windows guest's
  desktop and inject its input, and libkrun would have to map a port for
  a display it does not have. ADR 26-11-03-247 keeps the dialable ports to
  the two every driver has; a window is one hypervisor's concern.
- **Relay the stream through the agent's exec** (`disco-vm pipe` of the
  display, as Remote Desktop is relayed). It needs a new guest-side address
  kind in the agent baked into each image, so every image installed before
  it would still have a `gui` with no stream. The shim's route fixes those
  images too.
- **Dial the VM's hvsocket by its runtime ID** from the `gui` process, as
  `gui --install` does for an install. The shim would have to publish the
  runtime ID, and the connection would bypass the shim's token: anyone who
  can open the VM's hvsocket service reaches the desktop. The install has no
  shim; a running instance has one, and its token is the gate.
- **One sign-in for `TERMSRV/127.0.0.1`**, as 26-11-02-274 §1 had it. Two
  windows share it: the second signs in to its guest as the first's
  `COMPUTER\user`, and the first's close deletes it from under the second.
  A port in the target does not help; mstsc matches the host alone.
- **A random `127.a.b.c` per window.** mstsc records each host it connects
  to, so a random one per window leaves a new record each time; a fixed 254
  bounds them, and each is removed when its window closes.
- **Keep the optional interfaces.** Against `AGENTS.md`, and a caller learns
  only by trying whether a driver opens a window; a field says so up front,
  as `Display`, `Ports`, and `Commit` do.

## Consequences

- `/display` gives the shim's token holder nothing it lacked: `/guest`
  already runs any command in the guest as SYSTEM.
- A new driver writes `View` and `ViewInstall`, usually to return
  `ErrUnsupported`.
- `disco-vm info` lists `view`.
