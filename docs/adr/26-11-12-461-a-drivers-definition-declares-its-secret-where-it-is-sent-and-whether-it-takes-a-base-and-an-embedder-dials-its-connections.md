# 26-11-12-461 — A driver's definition declares its secret, where it is sent, and whether it takes a base, and an embedder dials its connections

- **Status**: Accepted (supersedes in part 26-11-11-215: §7, that a
  definition has no type for a secret; §2, `Registration.KeyEnv`; §1, that
  every definition describes a base; and §4, that a process-wide proxy
  variable stays where it is read, for a driver given an embedder's dialer)
- **Date**: 2026-11-12

Accepted by the owner by merging #88, 2026-11-12.

## Context

discobox makes each disco-vm driver a provider type of its own name,
configured by the driver's own fields (ADR 26-11-11-215's
`machine.DefinitionOf`; discobox's ADR 26-11-11-080). To run that provider
inside a box, discobox keeps the real key outside it: the box is handed a
stand-in, and discobox's egress proxy puts the real key in its place on the
way out, and refuses the stand-in anywhere else. To build that, discobox
needs, per driver, more than 26-11-11-215 gives it:

- **The secret, declared.** 26-11-11-215 §7 keeps a credential out of the
  definition altogether: `Config.Keys` carries it, the variables it is read
  from are `Registration.KeyEnv`, and `machine.Register` refuses a field of
  discobox's `secret` type. So nothing an embedder reads says that a driver
  takes a key, or what it is called.
- **Where the secret is sent.** boxd sends an API key (`bxd_...`) in the
  JSON body of a POST to `tokenUrl` alone, and the token it gets back as
  every gRPC call's bearer; a token given as the key is itself that bearer,
  at `addr` and at the region endpoint `addr` lists. archil sends its key as
  `Authorization: Bearer key-...` to `/api/` at `baseUrl`, or at the
  region's control plane. Today only the code says so.
- **Whether a driver takes a base.** Every definition describes
  `Config.Base` as a field, and hcs's says in prose that it takes none.
- **A dialer of its own.** discobox checks where a provider connects at the
  moment it connects (`addr`, `tokenUrl`, and `baseUrl` are privileged, and
  a name can resolve elsewhere by then). boxd, archil, and docker dial with
  `net.Dialer`, `http.DefaultClient`, and gRPC's own.

## Decision

1. **A definition lists its secrets,** `Definition.Secrets`, each a
   `SecretField` in the shape of discobox's `sandbox.ProviderConfigField`
   of type `secret` (`machine.ConfigSecret`: key, label, type, description,
   placeholder), plus `Env`, the variables the command line reads it from
   in order (taking `Registration.KeyEnv`'s place, which goes), and
   `Sends`. A secret is never a configuration field: its key is no field's,
   `Normalize` refuses it in a configuration by name, and
   `ConfigFromValues` refuses it as a field. Its value is carried as before,
   by `Config.Keys`; so a driver declares at most one, its key, and
   `Register` refuses a second until `Config` can carry it.
2. **A secret is written, never read back.** A definition holds where a
   secret comes from and where it goes, never a value. Nothing disco-vm
   prints, logs, returns, or writes to disk carries one (`info`,
   `drivers`, `--help=DRIVER`, an error, a VM's configuration file, a
   shim's environment); only the driver reads it, to send it as its
   definition says. A key `key issue` makes is printed once, as it is
   made, and listed without it after (ADR 26-10-24-562).
3. **A secret's sends say where it goes,** each a `SecretSend`: the hosts
   (`Endpoint`, the privileged configuration field whose value, or else
   `Default`, names one; else `Hosts`, every host it may reach, as
   `HOST:PORT`), `Listed` where the endpoint names more at run time (boxd's
   regions), the protocol (`https`, or `grpc`, an HTTP/2 POST over TLS),
   the method, the path (exact, or a prefix ending in `/`, below an
   endpoint URL's own path), and the place: a header, as `ValuePrefix` and
   the secret without `TrimPrefix`, or a top-level field of a JSON body. A
   secret of several forms has a send per form, told apart by `Prefix`
   (boxd's `bxd_`). `machine.SecretSends(driver, config)` resolves them
   under a configuration, as `New` would take it. A send a driver makes
   that its definition does not list is the driver's bug, and a test per
   driver drives its real client and compares what arrived with what
   `SecretSends` says.
4. **A driver takes a base or does not,** `Definition.Base` nil for one
   that does not (hcs), and `Definition.TakesBase()`. A driver whose base is
   configured away (docker with none set) still takes one.
5. **An embedder dials a driver's service connections,** `Config.Dial`, a
   `DialFunc` with `net.Dialer.DialContext`'s shape. boxd's gRPC and token
   exchange, archil's API and process API, and docker's daemon at its host
   (unix or tcp) dial through it, each at the service's own address: with
   it set, a driver reads no proxy variable (`HTTPS_PROXY` for net/http and
   gRPC, boxd's `ALL_PROXY`), and an embedder that proxies does so within
   its dialer. Nil is the process's own way, proxy variables and all, as
   before. Drivers take it as `Config.Dialer()` and
   `Config.HTTPTransport()`, so all of them follow that one rule. It is a
   value an embedder holds, not configuration: no command line sets it and
   no shim is given it. A local driver's download of its base or runtime
   from a registry (vz's and libkrun's) is not a service connection and
   does not go through it yet. It stays on `machine.Config`, since the
   engine is made from a driver already built (`engine.Open`).

## Alternatives rejected

- **Secrets as `ConfigField`s of type `secret` in `ConfigFields`,** as
  discobox lists a provider's. A configuration is written to a VM's
  directory for its shim, printed by `info`, and compared with the
  environment in warnings; every one of those would have to skip a secret,
  and one that forgot would read it back. A separate list, carried
  separately, cannot be.
- **Two secrets for boxd, an API key and a token.** Both are one credential
  in two forms, read from either variable, and `Config.Keys` carries one
  key; the driver tells them apart by `bxd_`, as boxd's own CLI does, and
  a send per form says the same.
- **A `Definition.Key` of its own, not a list.** It is what one key per
  driver is today, but discobox lists a provider's secrets as fields, and a
  driver that comes to need a second secret would change the definition's
  shape; the list keeps it, and `Register` holds it to one until then.
- **Sends resolved by a function each driver registers.** It would put the
  hosts beside the code, but a proxy is configured from the definition, and
  a function per driver can say anything; the declared sends, and a test
  that drives the real client against them, keep the two the same.
- **A dialer on the engine,** `engine.Open(driver, pool, dial)`. The engine
  makes no connection; a driver does, from what it was built with.
- **Leaving the proxy variables in place under an embedder's dialer.** gRPC
  takes a proxy only when it dials itself, while net/http dials the proxy
  through the dialer; the same driver would reach its embedder's check at
  the service for one call and at the proxy for the next, and the
  embedder's check is of the service. An embedder that checks each
  connection proxies within its dialer.

## Consequences

- discobox reads each driver's secret, its variables, and its sends from
  `machine.DefinitionOf`, resolves them for a provider's configuration with
  `machine.SecretSends`, and sets `Config.Dial` and `Config.Keys` per engine.
- boxd's token (`BOXD_TOKEN`, a JWT) is sent to region endpoints no
  definition lists (`Listed`); a stand-in for one reaches only `addr` until
  the proxy follows `ListRegions`. An API key never leaves for anything but
  `tokenUrl`.
- A secret's sends describe the secret, not what a provider returns for it:
  boxd's exchange answers a real key with a real JWT, which the driver
  keeps and sends as its bearer for up to an hour, at the home endpoint and
  every region. A box handed a stand-in `bxd_` key so holds a live token
  once it has called; keeping that outside it too (swapping the answer, or
  handing the box a token instead) is the proxy's to decide, and needs no
  more from the definition than the exchange's send.
- archil's send lists every control plane when `baseUrl` is unset, more
  than one configuration reaches.
- A driver's new way of sending its key is a send in its definition and a
  case in its test; `REVIEW.md` refuses one without.
- `Registration.KeyEnv` is gone: a driver's variables are its secret's
  `Env`. `docker.HostDaemon` takes the dialer.
