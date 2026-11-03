# 26-11-03-247 — A caller reaches a guest at its agent's and its service's ports alone

- **Status**: Proposed
- **Date**: 2026-11-03

## Context

ADR 26-10-31-990 §4 gave `machine.Machine` `Dial(ctx, port)`, a stream to
any guest port, which `Engine.Dial`, `disco-vm dial`, the shim's `/dial`, and
a peer `Forward` (`Instance`, `InstancePort`) all reach through. hcs, vz, and
fake connect to any port at any time: hvsocket and the framework's vsock take
the port at connect, and fake's guests share the host's loopback.

libkrun does not (ADR 26-11-03-530). Its vsock device reaches a guest port
from the host only through a Unix socket the launcher registers for that port
before the VM starts (`krun_add_vsock_port2` with `listen`), and it has no way
to add one later. So on libkrun the ports a caller can reach are fixed at boot.

How the API is used (discobox-ai/discobox at 49e29b3, by hand on 2026-11-03):
discobox never calls `Engine.Dial` or a machine's `Dial`, and makes no
`Forward`. It reaches a guest through its one service (`Engine.URL`,
`Engine.PrivateURL`) and the driver's guest operations, which are the
agent's. Its one accepted plan beyond that (discobox ADR 26-10-31-206) adds
guest-to-host forwards to host Unix sockets, never a forward to a peer, and
its ADR 26-10-09-143 rejects a backend that exposes any guest port the pool
asks for. A caller that needs another port in a guest reaches it through
something listening on the service.

## Decision

### 1. `Dial` reaches the agent's port and the service's, on every driver

A caller dials a guest at `guest.AgentPort` and at `guest.ServicePort` alone.
Every other port is refused with `machine.ErrUnsupported`, naming the port,
on every driver that lists `Capabilities.Ports`, whatever its hypervisor
could reach. `guest.DialablePort` is the rule, and it is checked where a
port enters the seam: `Engine.Dial` (so `disco-vm dial`), a local driver's
attached machine and its shim's `/dial`, and Create's check of a peer
forward's `InstancePort`.

`guest.ServicePort` is the instance's service as its driver serves it: a
local shim's `/dial` of it dials the port `Hypervisor.Service` names, which on
fake is the loopback port Prepare picked, and on hcs, vz, and libkrun is
`ServicePort` itself.

### 2. A peer forward targets the peer's agent or its service

A `Forward` to another instance takes an `InstancePort` that
`DialablePort` accepts, or Create refuses it with `ErrUnsupported`. A
forward to a host `Socket` keeps any guest port: its port is in the
instance's record, so a driver knows it at every boot.

### 3. A driver is given the forwards at boot

`local.InstanceSpec` carries the record's `Forwards`, so a hypervisor that
must map a guest-to-host port before its VM starts (libkrun) maps each one,
and the agent's and service's ports host-to-guest. Nothing else is mapped.

`Capabilities.Ports` keeps its meaning otherwise: a caller dials those two
ports, guest processes connect out to the host, and to a peer's.

## Alternatives rejected

- **Narrow libkrun alone, and report the narrowing as a capability.** A
  caller would have to know which driver reaches which ports, for a feature no
  caller uses. One rule for every driver keeps them interchangeable (owner,
  2026-11-03).
- **Relay any port through the agent** (an agent endpoint that connects to
  its own vsock port over vsock loopback, and splices it). `Dial` would reach
  any port on libkrun, at the price of a guest-protocol endpoint and a kernel
  feature, for no caller.
- **Ports declared at Create** (`InstanceSpec.Ports`, kept in the record and
  mapped at boot). It is boot-time too, but adds seam, record, and command-line
  surface for a use nobody has.
- **A fixed block of ports mapped at every boot.** An arbitrary range, and a
  listener per port in every VM.

## Consequences

- `disco-vm dial INSTANCE PORT` and `Engine.Dial` refuse a port other than
  7300 and 7302, and a peer forward to one is refused at Create. A root's
  instance whose record holds such a forward (made before this) boots, and
  the peer's shim refuses its `/dial`, so the guest connects and reads EOF,
  as it does from a stopped peer.
- fake's `Dial` of `ServicePort` no longer reaches the host's port 7302: the
  shim maps it to the instance's service.
- machinetest checks that a third port is refused and a peer forward to one
  is refused at Create.
- This supersedes in part ADR 26-10-31-990 §4, its `Dial` of any guest port.

## Deferred

- **Reaching any guest port.** Revisit when a caller needs a guest port
  beyond the service that it cannot reach through the service; ports declared
  at Create, or a relay through the agent, are the candidates above.
