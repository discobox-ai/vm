# 26-10-29-129 — An instance's service has a public URL and a private one, and a driver can fence an instance's egress to its networks

- **Status**: Accepted
- **Date**: 2026-10-29

## Context

0005 gives every instance one service and one URL: boxd's
`https://<machine>.boxd.sh`, a port the Docker daemon publishes on the host's
loopback, or the shim's loopback port. That URL is how a caller outside the
provider reaches the guest. It is not how another instance reaches it.

discobox runs a pool agent on one instance and each of the pool's sandboxes
on an instance of its own, all on one provider network (discobox ADRs
26-10-23-551 and 26-10-29-481, issue discobox#129). Three hops cross them:
discobox's server reaches the pool agent from outside, the pool agent reaches
each sandbox's agent, and each sandbox reaches its pool's proxy, credentials
endpoint and DNS server. Only the first wants the public URL:

- **boxd's URL is an edge.** The proxy at `<machine>.boxd.sh` terminates TLS
  with boxd's certificate and forwards HTTP to one port. A client certificate
  never crosses it, and nothing but HTTP and WebSocket does. Two machines on a
  shared network reach each other directly instead, on every port: an
  isolated machine with `networks` reaches exactly the non-isolated machines
  on them, east-west, with boxd's internal DNS following the same rule.
- **Docker's URL is the host's loopback**, which another container cannot
  reach; the container's address on its network is what a peer dials.
- **vz and hcs guests** reach neither each other nor the host's loopback.

A sandbox must also reach nothing but its pool (discobox ADR 0126 §6).
Isolation does not give that: boxd's `isolated` strips a machine of the
org's credentials and peers, and leaves its internet alone. boxd's egress
allowlist does, enforced outside the machine where its root cannot loosen it
(`SetVmEgressAllow`; `["none"]` is nothing). It governs the internet, not the
machine's networks. `CreateVm` takes no allowlist, and a machine created from
a snapshot starts unrestricted. disco-vm exposes none of this.

## Decision

### 1. `machine.Service` has a public URL and a private one

`machine.Service` gains `PrivateURL` beside `URL`, which keeps its meaning and
is the public one. `PrivateURL` is where the instance's peers — other
instances of the same driver, on its networks — reach its service:

- **A direct path**: `http://<address>:<port>`, the instance's own address on
  its networks. Every port the guest listens on is reachable at that address,
  so a peer may dial any of them, not only the service's.
- **The public URL**, when the driver gives instances no path to each other
  but its public URL is reachable from them.
- **Empty**, when neither is.

A caller tells a direct path from `PrivateURL != URL`; the URL's own scheme
says what speaks at it, as `URL`'s does. `Engine.PrivateURL(ctx, inst)`
answers it, refused while the instance is not running as `Engine.URL` is, and
`disco-vm url --private INSTANCE` prints it.

| driver | `PrivateURL` |
|---|---|
| boxd | on `networks`: `http://<machine's name on boxd's internal DNS>:7302`; on none: `URL` |
| docker | `http://<container's address on its network>:7302` |
| vz, hcs | empty |
| fake | `URL`: fake guests are host processes that share its loopback |

### 2. An instance can be created with its egress fenced

`engine.CreateOptions` and `machine.InstanceSpec` gain `Egress`, a
`*machine.Egress`. Nil is the provider's default. `machine.Egress{Allow}` is
the hostnames, wildcards, IPv4 addresses and ranges the instance may reach
beyond its networks; an empty `Allow` is nothing beyond them. The instance's
networks are never fenced by it.

- **A driver that cannot fence refuses it.** `machine.Capabilities.Egress`
  says it can, and a spec with `Egress` set on any other driver is
  `ErrUnsupported`, as `Isolated` is.
- **It is in place before Create returns,** before the caller can run
  anything in the instance, and read back from the provider: an instance
  whose fence the provider does not report as asked is destroyed, and Create
  fails.
- **It is the spec's, never the image's.** An instance of a snapshot gets the
  egress its spec asks, whatever the snapshot's instance had.
- **It holds across stop and start,** and `machine.Instance` reports it as the
  provider does.

boxd sets it with `SetVmEgressAllow` as soon as `CreateVm` or
`CreateVmFromSnapshot` answers, before the driver waits for the machine to
settle, with `["none"]` for an empty `Allow`, and reads it back from `GetVm`'s
`egress_allow`. docker, vz and hcs list no `Egress`.

## Alternatives rejected

- **One URL, chosen per driver for whichever caller matters.** boxd's
  callers need both: the edge for discobox's server, which is not on the
  machines' network, and the direct path for the machines on it.
- **A flag beside the one URL saying it is direct.** On boxd the edge and the
  direct path are different hosts, so one URL cannot be both.
- **The instance's private IP address, not a URL.** boxd's API reports none:
  its peers reach a machine by its internal DNS name. A URL is `Service`'s
  existing shape, carries the port, and lets a driver name a host.
- **Egress through `Base.Options`.** Driver-specific, and a caller could not
  tell a driver that fences from one that silently ignores the option; a
  caller that must not run unfenced has to be refused.
- **The caller fences after Create.** An instance of a snapshot runs
  unfenced until then, and every caller would have to remember to.
- **`Isolated` implies no egress.** boxd keeps them independent, and so does
  an isolated instance that needs the internet but not its org.

## Consequences

- Every driver answers `PrivateURL`; boxd and docker learn their instance's
  address on its networks.
- boxd's Create makes two more calls when `Egress` is set. The conformance
  suite and the boxd fake API gain a fenced instance from a base and from a
  snapshot, and a read-back that does not match.
- The proof table gains rows for the private URL and the fence. A claim about
  boxd's internal DNS name, and that it resolves for an isolated machine
  whose allowlist is `["none"]`, is verified against boxd before boxd's row
  is checked.

## Deferred

- **Changing a running instance's egress** (`Driver.SetEgress`): boxd can, in
  place. Revisit when a caller needs to change it after Create.
