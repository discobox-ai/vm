# 26-10-24-562 — A driver is given its key, and issues keys that expire and cannot issue

- **Status**: Proposed
- **Date**: 2026-10-24

## Context

discobox's VM provider boots a pool machine and hands it a credential, so the
pool agent on it runs disco-vm's engine and provisions its own sandbox
machines (discobox ADR 26-10-23-551 §4; disco-vm issue #18). Until now the
boxd driver read `BOXD_API_KEY` or `BOXD_TOKEN` from the process environment
when it was made. One process could not run two engines with different keys,
as a server with two provider instances must. A key could not be replaced
without a new driver, and could not be handed to another host except as a
copy of the caller's own. No driver could say what a key reaches, or make a
narrower one.

boxd's API has `CreateApiKey`, `ListApiKeys`, and `DeleteApiKey`. A key is
fenced to one org and given an expiry. A `member` key acts as its maker
within that org. An `org` key reaches only the org's shared fleet and needs an
org admin. Against boxd (by hand, 2026-10-24), all three calls refuse any API
key, with `PermissionDenied`: "this operation requires an interactive session
(`boxd login`) — API keys are fenced to one org and cannot manage the
account".

## Decision

1. **A driver is given a key source, `machine.Config.Keys`**, a
   `machine.KeySource` that answers a `machine.Key`. The driver asks it on
   every call to its provider, so a key replaced where the source reads it is
   used from the next call. A driver given none has no key, and never reads
   the environment for one. The command line passes `machine.KeyFromEnv`,
   which reads the variables a driver names when it registers
   (`machine.Register`): boxd's `BOXD_API_KEY` and `BOXD_TOKEN`, and the fake
   driver's `DISCO_VM_FAKE_KEY`. Nothing else reads them.
2. **`Driver.IssueKey`, `IssuedKeys`, and `RevokeKey` are required.** An
   issued key expires (a TTL is required) and cannot itself issue keys. A
   driver that cannot promise both issues none: it returns `ErrNoKey` when
   it has no credential to hand on (docker, hcs, vz), and `ErrCannotIssue`
   when its provider cannot make such a key. It never returns the caller's
   own key. `Capabilities.Keys` says which, and the narrowest scope an issued
   key reaches. Issued keys are named for their project
   (`dvm-<project>--<name>` on boxd), so `IssuedKeys` finds them by name and a
   caller that lost its record of one can still revoke it. `RevokeKey`
   refuses a key the driver did not issue for its project.
3. **boxd issues a `member` key**, fenced to the org of the engine's key, of
   scope `org`: it reaches every machine of its maker's in that org, not only
   the project's. boxd's refusal of key management to every API key is what
   keeps an issued key from issuing; so boxd issues keys only when the
   engine's key is an interactive session, and returns `ErrCannotIssue`
   otherwise.
4. **The fake driver issues keys bound to its project**, kept in its
   directory as a hash, and checks the key on every call, so the engine path
   and `machinetest`'s issued-key subtest run on every OS.
5. **Isolation is a create switch.** `InstanceSpec.Isolated` and `Networks`,
   with `Capabilities.Isolation`, strip what a provider gives a machine by
   default. On boxd they are `isolated`, which is immutable and which a
   restore inherits, and `networks`. Every other driver refuses them.

## Alternatives rejected

- **The key on `engine.Options`, as the issue sketched.** The engine is
  handed a driver already made (`engine.Open(driver, project)`), and only the
  driver calls the provider. A key on the engine would have to be threaded
  back into a driver after it was made, or the engine would have to make the
  driver. A field of `machine.Config` is where every other thing a driver is
  made with already is, and an embedder sets it the same way.
- **An optional interface for drivers that issue keys.** Every caller would
  type-assert, and a driver that forgot the method would silently issue
  nothing. A required method with an empty answer says so out loud, as
  `Driver.Bases` does (ADR 26-10-15-833).
- **An `org`-kind boxd key**, which reaches only the org's shared fleet. It
  would be narrower, but disco-vm's machines are private to their maker, so
  it reaches none of them, and only an org admin can make one.
- **`IssueRequest.Project`.** A driver is one project's (`Config.Project`),
  so a key for another project is a key from another driver.
- **Returning the caller's own key when the driver cannot issue.** It would
  hand on a key that does not expire and can issue keys, which is the
  caller's choice to make explicitly, not the driver's to make silently.

## Consequences

- A server runs any number of engines, each with its own key, in one process,
  and rotates a key without reopening one.
- boxd issues keys only from a `boxd login` session. A program that holds an
  API key in configuration, as discobox's provider does, can open an engine
  with it but cannot issue from it.
- A boxd issued key reaches all its maker's machines in the org, wider than
  the project. `Capabilities.Keys.Scope` says so before anything is made.
- A boxd key is exchanged for a JWT that lives an hour. Whether boxd honors
  that JWT after the key is revoked or expires is unverified, so a TTL or a
  revoke may take up to an hour to bite on a holder that exchanged it.

## Deferred

- **A boxd key fenced to a project, or to the machines one key made.** boxd
  offers none. Revisit when it does: `keyCapabilities` then narrows to
  `project`.
- **Issuing from an API key on boxd.** Revisit if boxd lets an API key make
  a key that cannot itself issue keys.
