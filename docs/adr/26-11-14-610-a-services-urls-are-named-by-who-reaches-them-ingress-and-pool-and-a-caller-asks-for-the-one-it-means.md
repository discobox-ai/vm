# 26-11-14-610 — A service's URLs are named by who reaches them, ingress and pool, and a caller asks for the one it means

- **Status**: Accepted (supersedes in part 26-11-12-604 §1 and §2, its
  scope names and reach table; 26-11-04-412 §4, its internal URL,
  selection order, and host relay; and 26-10-29-129 §1, the names `URL` and
  `PrivateURL`; §1's ingress with no credential of the driver's, §2's
  archil row, and §6's archil always giving ingress, superseded by
  [26-11-17-311](26-11-17-311-a-driver-can-fence-a-machine-at-its-providers-edge-and-its-urls-answer-only-a-credential-it-mints-for-its-caller.md))
- **Date**: 2026-11-14
- **Supersedes in part**: 26-11-12-604 §1 and §2, their three scope names
  (internal, private, public), §2's reach table, and that a URL on the
  engine's host alone is not public (the on/off/strict requests and the
  reaches stand); 26-11-04-412 §4, its internal URL, its
  selection order and its host relay for the pool's own traffic;
  26-10-29-129 §1, the names `URL` and `PrivateURL`, `Engine.PrivateURL` and
  `disco-vm url --private`

Accepted by the owner by merging #149, 2026-11-14. The owner answered its
Open item while #148 was delivered, 2026-11-14: §6.

## Context

A service has two URLs a caller can ask for and one more named but never
built:

- `machine.Service` has `URL` and `PrivateURL` (`pkg/machine/machine.go`,
  ADR 26-10-29-129 §1). `Engine.URL` and `Engine.PrivateURL` answer them
  (`pkg/engine/engine.go`), and `disco-vm url [--private]` prints them
  (`internal/cli/instances.go`).
- `machine.URLCapabilities` and `machine.URLRequests` name three scopes,
  internal, private and public (ADR 26-11-12-604, after ADR 26-11-04-412
  §4), and `disco-vm create --url SCOPE=on|off[,strict]` asks for each.
  The served protocol carries both (`pkg/machine/served`).
- **Internal is unused.** `Service` has no field for it and no driver
  reports it. It was 412 §4's stage E, for docker-vz's host-loopback relay,
  and stage F, the docker-vz composite, was never built (there is no
  `pkg/machine/dockervm`).
- **Public reads as "on the internet".** 604 §2 says a URL on the engine's
  host alone is not public, so docker, fake, vz, libkrun and hcs report
  public `URLNever`, though their `Service.URL` (a loopback port the Docker
  daemon publishes, or the shim serves) is exactly where the caller managing
  them reaches the service. A caller cannot strictly ask for "a URL I
  reach": `public=on,strict` is refused on every driver but boxd and archil.
- **412 §4 has callers switch URLs.** discobox is to take the first non-empty
  URL in the order internal, private, public.

discobox-ai/discobox#1077 tried to give a pool a setting that turns its pool
machine's public URL off and has the server reach the pool agent at its
private URL. Review found it could not work: on boxd a private URL
(`http://<machine>.boxd:7302`, `pkg/machine/boxd/service.go`) is reached
only by peers on the machine's networks, which the server is not; it is
plain HTTP, and every request on it carries pool-agent bearer tokens; and
discobox's accepted ADRs have the server reach the pool agent at its public
URL. The owner's decision (2026-11-14, on that issue): "public" never meant
"on the internet". It is where the pool's controller reaches the machine,
an internet HTTPS URL on boxd, a private VLAN address in an enterprise
install. "Private" is where the pool's machines reach each other. A caller
never switches between them. This ADR records that decision for disco-vm.

## Decision

### 1. Two scopes, named by who reaches the URL

A service has at most two URLs, each named for its audience. Neither name
says how public the network under it is.

- **Ingress**: where the driver's caller, the controller managing the pool,
  reaches the instance's service over HTTP with no credential of the
  driver's. It may be an internet URL (boxd's proxy, archil's exposed
  port), an enterprise VLAN address, a loopback port the Docker daemon
  publishes or a shim serves, or a relay the driver runs. How the driver
  gets the caller there is the driver's business.
- **Pool**: where the instance's peers, the other instances of the driver
  in its pool on its networks, reach its service. It is a direct path,
  `http://<address>:<port>` at an address where every port the guest
  listens on is reachable too, or the ingress URL where the driver gives
  peers no path to each other but its ingress URL reaches them. A caller
  tells a direct path by an `http://` pool URL differing from the ingress
  URL, as 26-10-29-129 §1 had it for `PrivateURL != URL`.

### 2. Every driver reports where its caller reaches it as ingress

`Capabilities.URLs` keeps 604's reaches (`URLNever`, `URLOptional`,
`URLAlways`) and its requests (on or off, strictly or not), for the two
scopes:

| driver | ingress | pool |
|---|---|---|
| boxd | optional: the default proxy, given by default; asked off, every proxy removed (604 §3) | always on networks (direct); on none, the ingress URL, so none with that off |
| archil | always: the exposed port | always: the ingress URL |
| docker | always: the port the daemon publishes on its host's loopback | always: the container's address on its network |
| fake | always: the shim's loopback port | always: the ingress URL (a shared loopback) |
| vz, libkrun, hcs | optional: the owner-only local socket the shim serves (§6), given by default | always: the same socket (§6) |

A strict ingress request, `ingress=on,strict`, then passes
`URLRequests.Check` on every driver, unless one of the two points left to
implementation (the served client's ingress, docker with a daemon on
another host) is settled as `URLNever`. `ingress=off,strict` is refused on
archil, docker and fake, which always give it. A reach that depends
on the rest of the spec is still reported as its common case, and the
driver's Create refuses the combination it cannot meet (boxd: a strict pool
URL on no networks with ingress off). Where a driver's ingress URL lasts
(across starts on boxd and archil, one start elsewhere) is unchanged.

### 3. Internal goes, and a caller asks for the scope it means

The internal scope is removed from `URLCapabilities`, `URLRequests`, the
CLI and the served protocol, and with it 412 §4's order of internal,
private, public. A caller asks for the URL of the audience it is: a
controller for ingress, a peer for pool. disco-vm never answers one scope's
request with the other's URL; a driver that gives the same URL for both
(boxd on no networks, archil, fake) is reporting where each audience
reaches the instance, not falling back. An instance with no URL in the
scope asked is `machine.ErrUnsupported`, as `Engine.URL` of an instance
whose public URL is off is today.

A relay, such as 412 §4's host-loopback listener, is one way a driver can
provide ingress. It is not a third audience.

### 4. On a driver with pool URLs, no pool member is off the pool's network

On a driver that gives pool URLs (boxd, archil, docker, fake), any member
of a pool reaches the others at their pool URLs. A pool's own
agent, the one that reaches the pool's other instances, therefore runs on
the pool's network: for docker-vz, inside the VM, where the containers'
bridge addresses are its pool URLs. 412 §4's host relay is not how the
pool's own traffic flows. Whether docker-vz needs a relay to give its
caller an ingress URL on the physical host is decided with docker-vz
itself (Deferred).

vz, libkrun and hcs give their instances no network path to each other.
Their pool URL is the socket of §6, on the host; a guest reaches a peer
only through the forwards its shim relays over vsock, through the peer
shim's owner-only control socket (ADR 26-10-31-990 §4), never at a URL.

### 5. The API, the CLI and the protocol take the new names

A breaking rename with no alias, as 412 §1's pool rename was: there are no
production deployments, and no URL scope is persisted (requests are not
stored, and the provider reports none on `Instance`).

- `machine.URLScope`, `URLIngress` (`"ingress"`) and `URLPool` (`"pool"`).
- `URLCapabilities{Ingress, Pool}` and `URLRequests{Ingress, Pool}`, JSON
  `ingress` and `pool`.
- `machine.Service{Listen, IngressURL, PoolURL}`, replacing `URL` and
  `PrivateURL`.
- One engine accessor, `Engine.URL(ctx, inst, scope)`, replacing
  `Engine.URL` and `Engine.PrivateURL`, refused while the instance is not
  running, as both are today.
- `disco-vm url INSTANCE` prints the ingress URL, and `--scope ingress|pool`
  picks the scope (the default ingress). `--private` goes. The flag is not
  `--pool`: that is the global flag naming the pool every resource is in
  (`internal/cli/root.go`).
- `disco-vm create --url SCOPE=on|off[,strict]` takes `ingress` and `pool`;
  `internal`, `private` and `public` are refused as unknown scopes.
- The served protocol's capabilities, requests and service carry the new
  names, and its `FieldURLs` keeps its meaning. A client and server of
  different builds are already refused by `Disco-Vm-Version`, so the wire
  needs no compatibility.

Every driver, the fake, `machinetest`, the CLI and `internal/e2e` change
together, with the `DESIGN.md` and `REVIEW.md` files that name the old
scopes.

### 6. On a driver that holds its VMs on the host, both URLs are an owner-only socket over vsock

The owner's answer (2026-11-14, on #148) to what ingress is on vz, libkrun
and hcs. The only way to such a VM is its vsock, so vsock is both its
ingress and its pool scope:

- **The shim serves the service on a platform-native local socket**,
  spliced into the guest's vsock service port, as it splices the loopback
  port it served before: on macOS and Linux an owner-only Unix socket,
  `service.sock` in the instance's directory (`unix:///PATH`); on Windows
  a named pipe only the owner may open (`npipe:////./pipe/NAME`). It takes
  any number of connections while the instance runs. The owner chose a
  named pipe for Windows over its Unix sockets.
- **The loopback port goes.** It is no scope's URL, and nothing serves it:
  any local process reached it, where the socket keeps the trust boundary
  of discobox's ADR 26-11-11-963 §4, the owner alone.
- **Both scopes' URL is that socket.** Ingress is optional and given by
  default. Asked off, the URL is not returned as ingress, though the socket
  still serves the pool scope, the same path. Pool is always given.
- `machine.DialURL` connects to any URL a `Service` gives, `http`, `https`,
  `unix` or `npipe`, for a caller that speaks HTTP over it.

So a held-driver sandbox can ask for ingress off and pool on, strictly,
which discobox#1077 needs; on archil, which always gives ingress, it still
cannot.

Left to implementation, not decided here:

- **The served client's ingress.** Its `Service` is the serving driver's,
  where the server's host reaches the instance, and a client reaches it by
  `Dial` (`pkg/machine/served/client.go`). Whether the client reports that
  reach unnarrowed, as it does today, or narrows ingress to what its own
  caller reaches is settled with the rename, against discobox's use of a
  held driver.
- **docker with a daemon on another host.** Its published loopback port is
  the daemon host's, not the caller's (`pkg/machine/docker/docker.go`).
  Whether docker then reports ingress `URLNever`, or refuses such a daemon
  configuration, is settled with the rename; today it reports the URL
  regardless.

## Alternatives rejected

- **Keep public and private, and report the loopback drivers' public URL
  as always.** It fixes the reach and keeps the word that misled: "public"
  reads as the internet, which is why 604 drew a loopback port out of it
  and why discobox#1077 tried to turn the pool machine's public URL off to
  keep it off the internet. Naming by audience makes the enterprise case
  (ingress on a VLAN) and the local case (ingress on loopback) read right.
- **Keep internal as a third scope for docker-vz's relay.** It is a
  mechanism that gives the caller a way in, not a third audience; the
  caller that would use it is the controller, which ingress already names.
  Its selection order is the URL-switching this decision removes.
- **Let a caller fall back from one scope to the other** (the pool URL when
  ingress is off, or 412 §4's first non-empty). On boxd the pool URL is
  plain HTTP that only peers reach. Falling back points the controller at
  an address it cannot reach, where it fails only later, at the dial, and
  wherever it can reach it, sends the controller's bearer tokens
  unencrypted. A strict request reports the misconfiguration at create.
- **Name the scopes by path: edge and direct, or external and internal.**
  Edge or direct is a property of a pool path (whether the pool URL is the
  ingress URL), not of who dials it. "External" again says how public the
  network is, and an ingress on a VLAN or loopback is not external.
- **One accessor per scope** (`Engine.IngressURL`, `Engine.PoolURL`). Two
  methods with the same refusals and one difference read better as one
  method taking the scope the request already names.

## Consequences

- discobox pins the release with these names and asks its pool machine
  for ingress strictly (discobox-ai/discobox#1077). What it asks for a
  sandbox's machine on a driver other than boxd follows from §6. Its bootstrap fields are placed again
  once, which is discobox's to say. discobox's `scripts/live-run.sh`
  (discobox-ai/discobox#1050) reads `disco-vm url`, and is changed for the
  new flags when discobox pins the rename.
- `machinetest` checks a strict ingress request on every driver, that an
  instance's ingress URL is reached from the test's process, and, as today,
  that a strict request a driver cannot meet is refused and an optional
  ingress URL asked off is not there. boxd's `TestNoPublicURL` becomes an
  ingress test with the same proof on boxd itself.
- 604 §3 (boxd removes every proxy of a VM whose public URL is off) and
  §4 (the driver is trusted) stand, read for ingress.
- 412's stage E (an internal URL across the seam) is dropped, and stage G
  (discobox's selection order) is replaced by discobox#1077.
- This ADR moves no layer of `DESIGN.md`'s proof table. The rename is
  proven by the fake, `machinetest` and `internal/e2e` on every OS, and by
  boxd's, docker's and the held drivers' conformance runs; a run of each
  is named in the implementing PR.

## Deferred

- **docker-vz's ingress.** Whether its caller reaches an inner container
  through a relay on the physical host, as 412 §4 designed, through the
  VM's own address, or not at all, is decided with the docker-vz composite
  (412 stage F), when it is built.
