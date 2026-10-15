# 0005 — Every instance has one HTTP URL, and the driver picks where its service listens

- **Status**: Proposed
- **Date**: 2026-10-15

## Context

This supersedes ADR 0001 §2, and `run --forward` in its §3. The rest of
0001 stands.


ADR 0001 gave an image one service port, declared in its build spec
(`service: {port: N}`), and gave an instance of such an image an endpoint: a
public HTTPS URL on boxd, or `http://guest` plus an `http.RoundTripper` that
dials the port elsewhere. That shape has three problems for the way a box is
used:

- The caller who copies in an agent and runs it decides what serves the URL,
  not the image's author. An image declaring a port ties every image to one
  agent's choice, and an image with no service has no URL at all.
- `http://guest` with a transport is reachable only from a Go program that
  embeds the engine. A CLI caller, or anything in another language, has no
  URL to use.
- What a guest program must listen on differs by driver: TCP on boxd and
  Docker, where a proxy or the daemon reaches the guest's network; vsock on vz
  and hvsocket on hcs, where the host has no network path into the guest.
  Nothing told the guest which.

Boxes expose exactly one HTTP port. `run -- COMMAND`, `run --rm`, and
`run --forward` assumed a container-like lifecycle a VM does not have.

## Decision

### 1. The driver picks the service, and every instance has one

`machine.Driver.Endpoint` is replaced by `Service(ctx, inst) (machine.Service,
error)`. `machine.Service` is the guest listen address, in `guest.Listen`'s
form, the guest port `Machine.Dial` reaches it on, and the driver's URL if it
has one:

| driver | listen | URL |
|---|---|---|
| vz | `vsock:7302` | the shim's loopback |
| hcs | `vsock:7302` (the hvsocket service ID of 7302, in every VM's ServiceTable) | the shim's loopback |
| boxd | `tcp:0.0.0.0:7302` | `https://<machine>.boxd.sh`, pinned to 7302 at Prepare, the same across stop and start |
| docker | `tcp:0.0.0.0:7302` | `http://127.0.0.1:N`, published by the daemon at each start |
| fake | `tcp:127.0.0.1:N`, N picked at Prepare | the shim's loopback |

`guest.ServicePort` is 7302. A build spec no longer declares `service:`, and
one that does is refused with a message saying why.

### 2. The URL is always HTTP, and local ones are the shim's

`Engine.URL(ctx, inst)` returns the driver's URL, or, for a driver with a
shim, `http://127.0.0.1:PORT`: the shim listens on a loopback port it binds at
each start and splices every connection into `Machine.Dial(service.Port)`.
The port changes at every start, and there is no URL while the instance is
stopped. `disco-vm url INSTANCE` prints it. `disco-vm endpoint` and the
`Endpoint` type are gone.

### 3. The guest learns where to listen from a file

At every start, before the start returns, the engine writes the listen address
through the guest agent to `/var/lib/disco-vm/service` (Linux and macOS) or
`C:\ProgramData\disco-vm\service` (Windows): `guest.ServiceFile`. It is on
disk, so a reboot inside the guest keeps it. A Go program in the guest calls
`guest.ListenService()`; anything else reads the one line and listens there.

### 4. `run` creates and starts, and nothing more

`run IMAGE` takes no command, no `--rm`, and no `--forward`. A command goes
through `exec`. Forwards stay in the engine API (`CreateOptions.Forwards`),
which discobox calls directly; only the flag is gone.

## Alternatives rejected

- **Keep the image-declared port, and only add discovery.** Images would still
  have no URL unless their author predicted the agent, and the caller who
  copies in the agent is the one who knows what serves it.
- **Pick the local port at create and keep it across starts**, as boxd's URL
  is kept. Another process can take the port while the instance is stopped,
  and start would then fail on something outside disco-vm's control.
- **Tell the guest through an environment variable on `exec`.** A service the
  caller installs as a systemd unit or launchd daemon, or one that comes back
  after a reboot, never sees it.
- **One fixed listen address on every driver.** A fake guest is a host
  process, so two of them on one fixed loopback port would collide, and vz
  and hcs have no TCP path into the guest to put a fixed TCP port on.
- **Keep the `http://guest` transport beside the loopback URL.** Two ways to
  reach the same service, and the loopback URL already serves an embedding
  program.

## Consequences

- An instance of any image has a URL, and a box's caller needs nothing from
  the image to serve it.
- The local URL needs no credential, like boxd's: anything on the host can
  reach it while the instance runs, so the service authenticates its callers.
- Layers and instances recorded with a service port keep the column in the
  store; it is no longer read. A layer built with `service:` has a new key,
  so the next build of such a spec, with `service:` removed, makes it again.
- A boxd instance prepared before this change has no URL; it is created again.
  A Docker container created before it publishes no port, and is created
  again.
- An hcs fork holder started before this change cannot list 7302 in a clone's
  ServiceTable; the clone is refused with the instruction to warm the image
  again.
- The Docker URL assumes the daemon is on this host; one reached over
  `tcp://` publishes on its own loopback.
