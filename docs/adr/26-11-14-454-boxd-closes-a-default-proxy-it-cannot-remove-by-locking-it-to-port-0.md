# 26-11-14-454 — boxd closes a default proxy it cannot remove, by locking it to port 0

- **Status**: Accepted
- **Date**: 2026-11-14
- **Supersedes in part**: 26-11-12-604 §3, that boxd removes every proxy of a
  VM whose public (now ingress) URL is asked off

Accepted by the owner by merging #155, 2026-11-15.

## Context

ADR 26-11-12-604 §3 has Create delete every proxy of a VM whose ingress URL
is asked off, the default one at `<machine>.<zone>` among them. That shipped:
discobox asks every pool sandbox's ingress URL off, strictly, and a boxd pool
then made no sandbox at all. Run against boxd itself (fra, 2026-11-14), the
deletion cannot work:

- `ListProxies` gives the default proxy an empty name, and `DeleteProxy` of
  the empty name answers `NotFound`.
- `DeleteProxy` of `@`, boxd's name for the default proxy, answers
  `FailedPrecondition: cannot delete the default proxy`. No other call and
  no machine setting removes or disables it.
- An `--isolated` machine has one too, and it forwards to the guest: with
  bot protection off, a server on 8000 answered at its URL.
- `SetProxyPort` of the default proxy (empty name) to `0` is taken. The
  proxy reads back `locked` at port 0, boxd answers 502 there while the
  guest listens on 80 and 8000, and it stays so across a stop and start.

The fake API named the default proxy after the machine and let it be deleted,
so the fake tests passed. `TestNoPublicURL`, the check against boxd itself
that 604 named, had not been run.

## Decision

1. Asked off, strictly or not, Create **closes** the default proxy: it
   locks it to port 0 with `SetProxyPort` and reads it back `locked` at 0.
   No guest can listen on TCP port 0, so whatever the guest's root runs
   answers at no `<machine>.<zone>` URL.
2. Create deletes every other proxy (`DeleteProxy` by its name), and reads
   back that `ListProxies` lists the default one alone, closed. A refusal,
   or a proxy still open, destroys the machine and fails the create, as 604
   §3 did.
3. `Service` reports no ingress URL for a machine whose default proxy is
   closed, as for one with none.
4. The fake API models boxd as found: an empty-named default proxy that
   `DeleteProxy` never removes, a port of 0 taken, and named proxies that
   are deleted. `TestNoIngressURL` checks on boxd that the default proxy
   stays the only one and closed across two boots, and that servers on 7302,
   80, 8000 and 8080, with bot protection off, answer at no URL.

## Alternatives rejected

- **Refuse ingress off, `Capabilities.URLs.Ingress` always.** True to
  boxd's API, but discobox asks it off strictly for every sandbox, so a
  boxd pool would make none; and a closed proxy is no inbound path.
- **Lock it to a port the guest does not use.** The guest's root can listen
  on any port from 1 to 65535, so it would be closed only until it does.
- **Leave bot protection on.** A browser check, already rejected by 604.
- **Ask boxd for a way to remove it.** Worth asking, but not needed: port
  0 closes it today. Removal would replace this if boxd offered it.

## Consequences

- Create makes one more call for a VM whose ingress URL is off, and none
  fewer: the default proxy is listed, locked, and read back.
- A closed machine still has a DNS name and a TLS certificate at
  `<machine>.<zone>` that answer 502: what is withheld is the guest, not
  the name.
- That boxd keeps port 0 closed rests on its proxy refusing a connection to
  port 0, which `TestNoIngressURL` checks; a boxd that started forwarding
  port 0 elsewhere would fail it.
