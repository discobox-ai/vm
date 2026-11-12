# 26-11-12-604 — A service's URLs are asked for, on or off, strictly or not

- **Status**: Accepted (by the owner, 2026-11-12, while delivering
  discobox-ai/discobox#1049)
- **Date**: 2026-11-12
- **Supersedes in part**: 26-10-29-129 §1, that `Service.URL` is every
  instance's public URL; 0005, that every instance has one HTTP URL

## Context

Every instance's service has a URL (0005, 26-10-29-129 §1), and a private
one where the driver has a path between instances. ADR 26-11-04-412 §4 names
three reaches, internal (the caller managing the pool), private (the
instance's peers) and public (anyone on the internet), and says a caller picks
among the URLs a driver advertises. Nothing lets a caller say which it wants.

On boxd the public URL is the machine's default proxy,
`https://<machine>.<zone>`, which Create pins to the service port on every
machine. discobox reaches a sandbox at its private URL alone, so a sandbox's
public URL serves nothing it needs. Yet the sandbox's root, which the code in
it holds, decides what answers on the service port: stop the service, bind
the port, and the URL is an inbound channel from the internet that the egress
fence, enforced outside the guest, never sees, and that the service's own
authentication no longer guards. boxd can remove the proxy (`DeleteProxy`),
and an isolated boxd machine has no in-VM CLI to make one again.

## Decision

### 1. A caller asks for each URL, on or off, strictly or not

`machine.InstanceSpec.URLs` and `engine.CreateOptions.URLs` (`disco-vm
create --url SCOPE=on|off[,strict]`) carry a `URLRequest` for each of
internal, private and public: a mode, unset (the driver's default), `on` or
`off`, and `Strict`.

- **Not strict**: the driver honors the mode where it can, and the instance
  gets whatever the driver gives where it cannot. The caller trusts the
  driver and takes either answer.
- **Strict**: the driver gives exactly what the mode asks, or refuses the
  create with `ErrUnsupported`. Strict with no mode is an error.

The zero value is every driver's default, so a caller that asks nothing gets
what it got before.

### 2. A driver says what it can give

`machine.Capabilities.URLs` is each scope's reach: `URLNever`, `URLOptional`
(given or withheld as asked) or `URLAlways`. `URLRequests.Check` refuses a
strict request a reach cannot meet, a URL never given asked on or one always
given asked off. Every driver's Create runs it, and so does the engine before
asking the driver. A reach that depends on the rest of the spec is reported
as what it is in the common case, and the driver's Create refuses the
combination it cannot meet: boxd's private URL is the public one on no
networks, so boxd refuses a strict private URL there with the public one off.

| driver | internal | private | public |
|---|---|---|---|
| boxd | never | always on networks (direct); on none, the public URL, so none when that is off | optional: the default proxy, pinned by default |
| archil | never | always (the public URL) | always: the exposed port |
| docker, fake | never | always | never: their URL is a loopback port |
| hcs, vz, libkrun | never | never | never: their URL is a loopback port |

A URL on the engine's host alone is not public. No driver gives an internal
URL yet; `Service` gains a field for it with ADR 26-11-04-412's stage E.

### 3. boxd removes every proxy of a VM whose public URL is asked off

Create, for a VM whose public request is `off`, deletes every proxy
`ListProxies` gives the machine, its default one among them, and reads back
that none is left. A refusal, or a proxy still listed, destroys the machine
and fails the create, as a fence that does not read back does (26-10-29-129
§2). Any other request pins the default proxy to the service port as before.
`Service` reports no URL for a machine with no default proxy, after `GetVm`
confirms the machine exists.

### 4. The driver is trusted

A caller relies on the create succeeding: it does not poll a running instance
to check the driver kept to the request. discobox asks for a sandbox's public
URL off, strictly, and asks nothing else strictly, since a URL that is
missing where it was wanted fails loudly where it is dialed.

## Alternatives rejected

- **A public URL opt-in, `Public bool`.** It changes every caller's default
  and covers one scope; a caller that only cares about one URL's absence
  would have to know every driver's default for the others.
- **Fold it into `Isolated`.** Isolation is the provider's defaults that
  reach the caller's account; a public URL is a separate inbound path, and
  discobox's pool machine is not isolated and needs its URL.
- **The caller re-checks instances** (on adoption, at each start, on an
  interval) and stops one with a URL it did not want. A strict request is
  the contract; a failed driver call while polling would stop a healthy
  instance, and the caller would duplicate what the driver already enforces.
- **A check in front of boxd's proxy** (bot protection). It is a browser
  check a real browser passes in a second, never applied to a subdomain
  proxy; it keeps out crawlers, not a party the guest works with.

## Consequences

- boxd's Create makes two or three more calls for a VM whose public URL is
  off. The fake API models `DeleteProxy`, a refusal, a delete boxd ignores,
  and a listing of an ID that is no machine; `TestNoPublicURL` checks on boxd
  itself that boxd removes a default proxy and none comes back at boot, which
  the fake cannot prove.
- The conformance suite checks that a strict request a driver cannot meet is
  refused, and that an optional public URL asked off is not there.
- A machine made before a caller asked its public URL off keeps it until it
  is made again: nothing re-checks it.
