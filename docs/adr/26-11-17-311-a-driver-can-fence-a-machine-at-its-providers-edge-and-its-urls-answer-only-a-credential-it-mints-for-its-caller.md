# 26-11-17-311 — A driver can fence a machine at its provider's edge, and its URLs answer only a credential it mints for its caller

- **Status**: Accepted (by the owner, merging #188, 2026-11-18)
- **Date**: 2026-11-17
- **Issue**: #186 (for discobox-ai/discobox's ADR 26-11-16-619 §§1–2)
- **Supersedes in part** (both shipped, so superseded, not amended):
  - [26-11-14-610](26-11-14-610-a-services-urls-are-named-by-who-reaches-them-ingress-and-pool-and-a-caller-asks-for-the-one-it-means.md)
    §1's ingress "with no credential of the driver's", §2's archil row, and
    §6's "on archil, which always gives ingress, it still cannot". The two
    scopes, their audiences and their requests stand.
  - [26-11-03-676](26-11-03-676-an-archil-driver-puts-nothing-in-its-guests-and-makes-a-vm-a-sandbox-named-for-it.md)'s
    "Create exposes port 7302" and its service row.
- **Relates to**: 26-10-23-417 (the driver keeps no state), 26-10-29-129 §2
  (a driver fences egress), 26-11-12-604 (URLs asked on or off, strictly).

## Context

discobox's ADR 26-11-16-619 lets a pool run on a driver with no networks if
the driver fences each machine at its provider's edge: no peer network,
egress fenced with DNS closed, a listener that answers only a credential the
guest never holds, and nothing of the account in the guest. discobox
decides from what disco-vm reports, never from a driver's name, and leaves
the field names, the credentialed URL's shape, and a token's life to
disco-vm, under one rule: a token never outlives the machine it reaches.

Today, at `dc22eaf`:

- **One bit for two things.** `Capabilities.Isolation`
  (`pkg/machine/machine.go:180`) says both "stripped of what the provider
  gives" (`InstanceSpec.Isolated`) and "placed on the caller's networks"
  (`InstanceSpec.Networks`). The engine checks it (`pkg/engine/engine.go:122`),
  the served protocol narrows it by `FieldIsolated` and `FieldNetworks`
  (`pkg/machine/served/server.go:155`), `machinetest` reads it
  (`machinetest.go:1009`), and only boxd reports it (`boxd.go:126`).
- **A fence leaves DNS open.** `Egress` is an allow-list
  (`machine.go:639`). Archil's allow-list leaves port 53 open to `8.8.8.8`
  and `1.1.1.1`, the resolvers the guest's `resolv.conf` names. That is a
  recursive-DNS channel out. A `deny` of those two in the create body closes
  it, is read back unchanged, and still reaches an allowed host by name at
  any address (#86; discobox's spike, 2026-11-16).
- **A public, unauthenticated port.** archil's `makeSandbox` and `Service`
  `PUT …/ports/7302` (`pkg/machine/archil/archil.go:358`, `:774`). An exposed
  port answers anyone, and one anonymous `GET` starts a stopped sandbox. The
  driver reports `URLs{Ingress: always, Pool: always}` (`archil.go:85`).
- **A URL is credential-free by definition.** 610 §1 defines ingress as
  reached "with no credential of the driver's". `Service.IngressURL`
  (`machine.go:747`) and `Engine.URL`'s doc (`engine.go`) say the same.
- **Archil's port tokens** (docs.archil.com, `…/port-tokens`, checked
  2026-11-17):
  - `POST /api/sandboxes/{sid}/port-tokens {port, ttl}` mints one. `ttl` is
    a Go duration up to `8760h`, or absent for no expiry. The response
    carries `token` and the port's stable `hostname`. **The token is never
    returned again.**
  - `GET …/port-tokens` lists the unexpired tokens' `id`, `port`,
    `created_at` and `expires_at`, never the token. `DELETE
    …/port-tokens/{id}` revokes one.
  - A request needs `X-Archil-Token`. Without it, `401`, and a stopped
    sandbox stays stopped. HTTP/1.1 over HTTPS only. A token is checked
    when a connection opens. An exposed port ignores tokens.
  - **An unexposed port's hostname comes only with a minted token.** The
    sandbox has no route field (`endpoints` lists only the guest's own
    services), and `GET …/ports` lists only exposed ports.
  - Whether deleting a sandbox deletes its tokens, and whether a fork
    inherits them, is not documented.
- **The driver keeps no state** (ADR 26-10-23-417). Archil sandboxes have no
  labels or metadata, and nothing in the guest is the driver's to keep a
  secret in.
- **`Service` is read at every start.** The engine's `announceService`
  (`engine.go:485`) calls it at each start, and uses only `Listen`.
  `Engine.URL`, the served `OpService` (`server.go:362`) and `machinetest`
  call it too.

## Decision

### 1. `Isolation` splits into `Bare` and `Networks`

`Capabilities.Isolation` goes, with no alias:

- **`Capabilities.Bare BareReach`**: whether a machine holds nothing of the
  provider's account.
  - `BareNever` (`""`): the driver claims nothing, and cannot strip.
  - `BareStrip` (`"strip"`): a machine holds something of the account
    unless the spec asks for it stripped.
  - `BareAlways` (`"always"`): the driver gives a machine nothing of the
    account.
- **`Capabilities.Networks bool`**: the driver places a machine on the
  caller's networks (`InstanceSpec.Networks`).
- `InstanceSpec.Isolated` and `Instance.Isolated` become **`Bare`**. A
  driver that reports `BareStrip` strips at create, as boxd's `Isolated`
  does today. A driver that reports `BareAlways` takes `Bare` as already
  met. A driver that reports `BareNever` refuses it with `ErrUnsupported`.
- **boxd**: `BareStrip`, `Networks`. **archil**: `BareAlways`, no networks.
  **docker, hcs, vz, libkrun, fake**: `BareNever`, no networks, as they
  refuse `Isolation` today. Each may claim more later, in its own change
  with its own proof.
- **archil's `BareAlways` holds with a data disk.** The guest then holds
  that disk's mount token, readable by root. It reaches only the guest's
  own disk, and replay from another sandbox was refused (#86). It is the
  guest's, not the account's (discobox's 619 §1.4). Whether it replays
  from outside Archil is open, and changes nothing here.

### 2. A fence can close DNS

- **`Egress.CloseDNS bool`**: no resolver answers the guest beyond its
  networks. Hosts in `Allow` are still reached by name at whatever address
  the guest dials, where the provider connects by name.
- **`EgressCloseDNS EgressFence = "close-dns"`** in `Capabilities.Egress`
  says the driver holds it. `Capabilities.Fences` checks it, and a driver
  that does not list it refuses a fence that asks for it. `EgressNone`
  already closes DNS. `CloseDNS` with an empty `Allow` is met by any driver
  that holds `EgressNone`.
- **archil** lists it. With `CloseDNS`, the create body (and the network
  set after a fork) carries `deny: ["8.8.8.8", "1.1.1.1"]`. Reading the
  fence back gives `CloseDNS` only when both are denied. An `Allow` that
  names a denied resolver is refused. A guest whose resolvers change is
  caught by §5's case, not by the driver.
- **DNS closes only when asked.** An allow-list by name is how `disco-vm
  run --egress deb.debian.org` works. Closing DNS on every archil fence
  would break it.

### 3. A URL may answer only a credential the driver mints, which comes with it in `Service`

610 §1 changes in one clause: a scope's URL is reached with no credential
of the driver's **or with the one the driver gave the caller beside it**.
The scopes still name who reaches the URL, not how.

- **`URLCapabilities.Credential bool`**: every URL the driver gives answers
  only a request carrying the instance's credential. The provider checks
  it outside the guest, where the guest's root cannot loosen or answer it.
  Without it, the request is refused before the guest (`401` on Archil).
- **`machine.Credential{Header, Token string; Expires time.Time}`**:
  `Header` is the header that carries `Token` (`X-Archil-Token` on archil).
  discobox's dialers send it under that name and hard-code none.
  `Expires` is when the token stops opening connections. A connection
  already open is not cut.
- **`Service.Credential *Credential`** is the one credential for every URL
  in the `Service`. It is nil on a driver that does not report
  `URLs.Credential`.
- **`Service` mints it.** Each call to `Service` of a driver that reports
  `URLs.Credential` mints a new credential. It is not cached, stored, or
  written anywhere, and it is never placed in the guest. The URL and its
  token come from one call because on Archil the hostname comes only with
  the token.
- **`Listen` leaves `Service`** for a required **`Driver.Listen(ctx, inst)
  (string, error)`**, which mints nothing. The engine's start path
  (`announceService`) calls it, so a start never mints. `Service` is then
  called only by a caller that means to reach the service.
- **`Engine.URL(ctx, inst, scope)` returns `machine.Endpoint{URL string;
  Credential *Credential}`** in place of a string, with the same refusals.
- **archil**:
  - It never exposes 7302: `Create` and `Service` `PUT` no port.
  - It creates every sandbox with `enable_service_ingress: false`.
  - It reports `URLs{Ingress: optional, Pool: always, Credential: true}`.
  - `Service` mints a token for 7302 and gives its hostname as both URLs,
    with the token as `Credential`.
  - Ingress asked off leaves `IngressURL` empty. The same hostname is still
    the pool URL, as on a held VM (610 §6). So a pool's sandbox (ingress
    off, pool on, both strict) and a pool machine (ingress on, strict) are
    both met.
  - **A VM made before this change** has 7302 exposed. `Start` unexposes
    it (`DELETE …/ports/7302`) before it returns, so every VM is fenced
    from its next start. The fence of an old VM is read back without
    `CloseDNS`, which is true.

### 4. A token is minted per call, expires, and is revoked with its machine

The driver keeps no state, and Archil gives a token's secret only once. So
the driver never holds a token, and every holder holds its own:

- **Each `Service` call mints a token with a finite lifetime.** The driver
  fixes the lifetime and reports it in `Expires`. The driver never mints a
  non-expiring token, though Archil allows one.
- **A holder renews by calling `Service` (or `Engine.URL`) again** before
  `Expires`, and keeps a credential until then rather than asking per
  request. An old token lapses on its own. Tokens never pile up past the
  calls made within one lifetime.
- **Never outliving the machine**:
  - `Destroy`, and `Upgrade` before it deletes the sandbox, list the
    sandbox's tokens and revoke each one, then delete. A revoke that
    fails fails the call, as a failed delete does.
  - `Commit` revokes them too, because a committed VM is never run, and a
    request with an old token would start it.
  - `Create` from a committed VM lists the fork's tokens and revokes any
    it finds, so a fork never answers its source's holders. This applies
    whether or not Archil copies them.
  - A token that a failed revoke leaves behind still expires.
- **`Stop` revokes nothing.** A holder's request starts a stopped sandbox,
  as Archil does. The holder is the caller that manages the machine, and
  discobox's server and sandboxes keep their tokens across a sleep (619
  §2).
- **Two holders of one machine** (619 §2: the server's channel token and
  the sandboxes' shared token) are two `Service` calls, each with its own
  token and expiry. Neither revokes the other.

### 5. Conformance checks what a driver claims

`machinetest` gains, run on archil's fake API (`fakeapi_test.go`, PR #184)
and on Archil itself:

- **close-dns**, for a driver listing `EgressCloseDNS`: a guest fenced to
  an allowed host with `CloseDNS` resolves no name. It checks
  `getent hosts`, and a query to each resolver in its `resolv.conf` and to
  `8.8.8.8` and `1.1.1.1`. The fence reads back with `CloseDNS`.
- **credentialed-url**, for a driver reporting `URLs.Credential`:
  - Each URL refuses a request with no credential, or a wrong one, before
    the guest answers.
  - It reaches the service with `Service.Credential`.
  - A stopped instance stays stopped after a request with no credential.
  - After `Destroy`, the instance's tokens are revoked: the driver's
    listing of them is empty, on the fake and live.
- **bare**: the engine refuses `Bare` on a `BareNever` driver, and meets it
  on the others. Whether a machine holds nothing of the account is a
  property of the provider, proven per driver, not by the suite (#86 for
  archil).

### 6. The served protocol and the CLI follow

- `FieldIsolated` becomes `FieldBare`. The served capabilities report
  `Bare` as the driver's when `FieldBare` is allowed, `BareAlways` whether
  or not it is (it needs no request), and `BareNever` otherwise.
  `Networks` is the driver's only when `FieldNetworks` is allowed.
- `OpService` carries `Credential`. The embedder's allow-list on
  `OpService` already decides who may reach a service, and so who gets a
  token. Whether the served client reports the serving driver's
  `URLs.Credential` as its own is settled with 610 §6's open point on the
  served client's ingress.
- A client and server of different builds are refused by `Disco-Vm-Version`
  (610 §5), so a peer never misreads the split fields, and the wire needs no
  compatibility.
- `disco-vm url` prints the URL. It prints a credential only when asked by
  a flag (§7).

### 7. Left to implementation, not decided here

- **A token's lifetime on archil.** It must be finite, and long enough that
  discobox renews it well before it ends. A day is the starting point.
- **Whether Archil deletes a sandbox's tokens with it, and whether a fork
  copies its source's.** Check both live. §4 revokes either way, so the
  answer changes only `docs/drivers/archil.md`.
- **Whether Archil caps the tokens per sandbox.** If it does, the cap bounds
  how often a holder may renew.
- **The flag that prints a credential in `disco-vm url`**, and its form.
- **Whether fake gains a credentialed mode**, so the engine and
  `internal/e2e` exercise `Endpoint.Credential` on every OS, or archil's
  fake API alone covers it.

## Alternatives rejected

- **A third scope for a credentialed URL** (an "edge" scope). A scope names
  who reaches the URL. The credential is how they reach it, and the
  audience is the same. 610 already rejected naming scopes by path. A
  caller would also have to know which scope to ask for on which driver,
  which 610 §3 removed.
- **Archil gives no ingress, only a credentialed pool URL.** discobox's
  server asks its pool machine for ingress strictly (610's Consequences).
  That create would be refused on archil.
- **A separate `Driver.Credential` call beside an uncredentialed
  `Service`.** On Archil the hostname comes only from a mint, so `Service`
  could not name the URL without minting anyway. Two calls would mint
  twice.
- **Minting in `Service` and leaving `Listen` in it.** Every start reads
  `Service` (`engine.go:485`), so every start would mint a token nobody
  holds.
- **One non-expiring token per machine, minted at `Create`.** The secret is
  returned once. Keeping it needs state the driver must not keep (417). The
  only place on the provider to keep it is the guest, which must never hold
  it.
- **Non-expiring tokens per call.** They pile up for the machine's whole
  life, and one that leaks lives as long.
- **`Service` revokes the tokens it minted before.** On a pool machine that
  cuts off the other holder (619 §2).
- **Revoke at `Stop`.** Every sleep would cut off the server's channel and
  the pool's sandboxes. The tokens would then have to be minted and
  handed out again at every start, through the pool's bootstrap.
- **A port token as an issued key** (`IssueKey`). A key reaches the
  driver's API for a pool, and Archil issues none (`ErrCannotIssue`). A
  port token reaches one port of one machine and never the API.
- **Every archil allow fence closes DNS.** It breaks the plain use of an
  allow-list by name (§2).
- **Keep `Isolation` and add `Bare` beside it.** The one bit is the
  conflation discobox's 619 §1 removes. A third field that half-overlaps
  the other two would be the shim that the repository rules forbid.

## Consequences

- **The seam**: `Bare`, `Networks`, `EgressCloseDNS`, `Egress.CloseDNS`,
  `URLCapabilities.Credential`, `Credential`, `Service.Credential`,
  `Driver.Listen` and `Engine.URL`'s `Endpoint`. Every driver, fake,
  `machinetest`, `internal/e2e`, the CLI and the served protocol change
  together, with the `DESIGN.md` and `REVIEW.md` files that name
  `Isolation`, `Isolated` or a credential-free ingress.
- **archil** (after PR #184):
  - It closes DNS when asked and never exposes 7302.
  - Ingress is off at create.
  - It mints, lists and revokes port tokens, and unexposes 7302 on an old
    VM's start.
  - `docs/drivers/archil.md`'s Service, egress and "no isolation" lines
    change.
- **A pool on archil** reaches a machine only through `Service.Credential`
  over HTTP/1.1. discobox's dialers carry the named header and pin
  HTTP/1.1 (619 §2). A caller that calls `Engine.URL` per request mints per
  request, so discobox keeps a credential until its `Expires`.
- **`DESIGN.md`'s proof table** gains rows for a DNS-closing fence and a
  credentialed URL. The implementing PR fills archil's column from a run on
  Archil itself and the fake's from `machinetest`. The `Isolation` row
  becomes `Bare` and `Networks`, with boxd's proof unchanged.
- discobox moves its pin, and `fences`, `requireMachineFence`,
  `NewMachineSandboxRuntime`, `createOptions` and `hostCredential` read
  `Bare` and `Networks` (619's Consequences).

## Deferred

- **A lifetime the caller asks for.** Revisit when a holder needs a token
  to last longer or shorter than the driver's, for example a pool's
  bootstrap that cannot be renewed in time.
- **A credential per scope.** One credential covers every URL of a
  `Service`, because archil's two URLs are one hostname. Revisit when a
  driver's two scopes need different ones.
