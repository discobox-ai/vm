# 0001 — A driver runs guest operations itself and exposes one service endpoint

- **Status**: Proposed
- **Date**: 2026-10-09

## Context

Above the `machine.Driver` seam, disco-vm talks to every guest the same way:
the engine and the builder open `Machine.Dial(guest.AgentPort)` and speak the
guest agent's HTTP protocol (`guest.Client`) for exec, files, health, info,
and shutdown. Every image therefore carries the `disco-vm` binary as a guest
agent, installed by the driver at install time.

That is the only option on Virtualization.framework and Hyper-V: neither
offers an exec or file API into a macOS or Windows guest, so something inside
has to provide one. boxd does offer it. Its published API
(docs.boxd.sh/reference/grpc-proto) has an `Exec` stream that carries stdin,
stdout, stderr, exit codes, and a TTY with its size and resizes (`tty`,
`cols`, `rows`, `window_change`), and streaming file upload and download
(`UploadFileStream`, `DownloadFileStream`). The proto subset this repository
vendors in `pkg/machine/boxd/internal/boxdapi` has only what the driver calls
today — `tty` without its size, and upload without download — and is extended
from that page as its own header describes. On boxd the agent buys nothing and
costs:

- Install uploads a binary and adds a systemd unit to every base image.
- Every operation is a second hop: boxd Exec runs `sudo -n disco-vm pipe` to
  reach the agent's socket, and readiness polls it every 250ms.
- Every snapshot pins the agent's version.

The other side of the seam is ports, in two directions. `Machine.Dial(port)`
reaches any guest port from the host. `Forward`/`HostListener` let a guest
connect out to the host, and discobox needs that one: its ADR 0144 §4 has a
host-VM sandbox (vz, hcs) reach its pool's proxy, credentials broker and Git
over the guest socket, which is this path. The host-to-guest direction it does
not need beyond one port: its ADR 26-10-09-143 fixes the contract as start the
VM, place one bootstrap file through the guest, and get back **one address**
to the sandbox agent, which serves exec, files, terminals, and every user port
(`tcp/attach`, `udp/attach`) itself. On boxd
(and other providers, exe.dev among them) that address is the provider's
public HTTPS URL. `docs/discobox.md`'s earlier plan of routing discobox
through disco-vm's guest agent and `Dial(port)` is superseded by it.

## Decision

### 1. Guest operations are the driver's, behind the seam

`machine.Machine` gains the guest operations as required methods: exec (with
stdio, a TTY and its resizes, user, environment, and working directory), copy
in and out (tar), orderly shutdown, and info. The engine and the builder call
them; `guest.Client` is no longer used above the seam.

- **vz, hcs, fake** implement them with one shared implementation over their
  private channel to the guest agent (vsock, hvsocket, or a host socket).
  Their behavior does not change; the agent protocol becomes an implementation
  detail of the drivers that need an agent.
- **boxd** implements them over its API: `Exec` for exec (run as a user with
  `sudo -u`, environment and working directory through the shell), tar over
  `Exec`'s stdin and stdout for copy, and `systemctl poweroff` with the
  existing shutdown hook and `StopVm` for shutdown. A boxd image carries no
  disco-vm agent; install adds only the shutdown hook.

### 2. An image declares one service port, and an instance exposes it

A build spec may declare `service: {port: N}`. It is recorded on the layer and
inherited by layers built on it until one declares another. An instance of
the image has exactly one endpoint, for that port:

```go
type Endpoint struct {
	URL       string            // https://<name>.boxd.sh, or http://guest
	Transport http.RoundTripper // nil for a public URL; dials the guest locally
}
```

- **boxd**: at create, `SetProxyPort` pins the machine's default proxy, at
  `<machine>.boxd.sh`, to the port, with bot protection off. The endpoint is
  its HTTPS URL, stored with the instance, usable by any process with no boxd
  credential, the same across stop and start. boxd's proxy is public and
  offers no access control; the service behind it authenticates every
  request.
- **vz, hcs**: a transport that dials the port over vsock or hvsocket.
- **fake**: loopback.

`disco-vm endpoint <instance>` prints it. An image that declares no service
has no endpoint.

### 3. Raw guest ports leave the seam; forwards stay

`Machine.Dial(port)` is no longer part of `machine.Machine`; each driver keeps
whatever private channel it needs. The guest-to-host direction is unchanged:
`Capabilities.Forward`, `HostListener`, `run --forward`, and `dial-host` stay,
because they are how a host-VM sandbox reaches its pool (discobox ADR 0144
§4). A provider-hosted guest reaches its pool over the network instead, and
its driver does not list `Forward`.

## Alternatives rejected

- **Keep the guest agent on boxd for uniformity.** It is a relay hop, an
  upload at install, a pinned version in every snapshot, and a 250ms poll, to
  reach an API boxd already exposes. Uniformity belongs in the seam's methods,
  not in a binary every guest must carry.
- **An optional interface for guest operations**, implemented by boxd and
  emulated over the agent elsewhere. Every driver needs them; they are required
  methods.
- **Keep `Dial(port)` for boxd as `socat` over `Exec`.** A provider credential
  and an Exec call per connection, for a raw stream no caller needs once the
  service endpoint exists.
- **A named proxy (`CreateProxy`) for the endpoint**, at
  `<name>.<machine>.boxd.sh`. The default proxy would still forward to
  whichever common port the guest listens on (80, 443, 8080, 8000, 3000,
  5000, 5173, else 8000), which pinning it to the service stops, and bot
  protection, the challenge before a request may wake the machine, covers
  only the default domain.
- **boxd's `ExposePort` for the endpoint.** A raw public TCP port with no TLS
  and, like the proxy, no access control: everything the HTTPS proxy gives,
  minus TLS.
- **An endpoint per declared port.** The service behind the one endpoint
  reaches every other port from inside the guest, behind its own
  authentication; more public URLs are more surface for nothing.
- **Remove forwards too**, since a provider guest reaches its pool over the
  network. A host-VM guest has no network to its pool by design (discobox ADR
  0144 §4), and the guest socket is how it gets there.

## Consequences

- `pkg/engine`, `pkg/build`, and `internal/cli` stop importing `pkg/guest`'s
  client; `machinetest` checks the guest operations through `Machine`.
- boxd's install, builds, `exec`, and `cp` no longer depend on a guest agent,
  and a boxd build no longer waits on agent health between layers.
- The proof table in `DESIGN.md` gains a guest-operations row per driver and
  an endpoint row.
- The vendored boxd proto gains `Exec`'s TTY size and resize fields,
  `DownloadFileStream`, and the proxy calls the endpoint needs.
- `docs/discobox.md` is rewritten to the start, bootstrap, endpoint contract.

## Deferred

- **Access control on a provider's URL.** boxd offers only a tailnet-only mode
  enabled per organization. Revisit when a provider offers per-URL access
  control that disco-vm can set at create.
- **A second provider driver** (exe.dev). Revisit when one is written; it
  implements the same seam and endpoint shape.
