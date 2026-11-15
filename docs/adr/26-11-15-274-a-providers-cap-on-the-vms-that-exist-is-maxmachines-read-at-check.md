# 26-11-15-274 — A provider's cap on the VMs that exist is `MaxMachines`, read at `Check`, and boxd's key is refused while a host-bound secret reaches its machines

- **Status**: Proposed
- **Date**: 2026-11-15

## Context

boxd caps an org's machines: `GetOrgMachineLimit` answers `max_vms`, "how
many machines it may have at once", and `vm_count`, the non-destroyed
machines billed to the org, stopped ones included, shared and private,
every member's. boxd refuses `CreateVm` and `CreateVmFromSnapshot` past it;
a boot of a machine that exists is never refused for it. Snapshots (a
committed VM) do not count. The default is 50, and 2 for a new org without a
payment method (docs.boxd.sh/reference/grpc-proto).

The seam has one cap, `Capabilities.MaxRunning`: guests of an OS that run at
once, which the driver enforces across every pool, refusing `Boot` and
`Start` past it with `ErrCapacity` (ADR 26-11-04-412). `machinetest`'s
max-running subtest checks exactly that: it boots the cap's number and
expects the next `Boot` to be refused. discobox passes `MaxRunning` through
to a pool's reported `runtime` condition.

`Capabilities` takes no context and does no I/O, and boxd's limit is the
org's, which changes when the org's owner asks boxd for more.

boxd also lets a machine reach the hosts of every host-bound secret in its
scope (one with `domains`) whatever its egress allowlist says, so a caller
that fences a VM's egress (ADR 26-10-29-129) holds its fence only while no
such secret reaches the driver's machines. The driver's machines are its
key's owner's private machines in the key's org. boxd's
docs.boxd.sh/guides/env-secrets says which scopes reach one: the org's `all`
and the owner's `private`; `shared` reaches shared machines alone, and
`unattached` and `private-unattached` only a machine whose spec lists the
secret, which the driver's never do. A personal-scope secret's scope is
ignored but for `unattached`. boxd's credential fence resolves an empty org
to the org an API key or a machine's token is fenced to, as it does for a
create, which the driver sends with no org. boxd documents
`GetOrgMachineLimit` readable with a login or an API key issued for the org,
not with a token minted inside a machine.

## Decision

1. **`Capabilities.MaxMachines map[OS]int`** caps the VMs of a guest OS that
   may exist at once, running or stopped, committed VMs aside. Zero is no
   cap. It is the provider's: it may count VMs the pool does not see, and
   the provider enforces it. `Create` past it is refused with
   `ErrCapacity`, and a destroy makes room.
2. **A driver that reads a cap from its provider reads it at `Check`**, and
   keeps it; `Capabilities` reports it from then on and none before.
   `machinetest.Run` calls `Check` before it reads `Capabilities`. A served
   client reads the server's capabilities again after each `Check`.
3. **boxd reports its org's `max_vms` as `MaxMachines[linux]`**, read at
   `Check` from the home endpoint. A credential refused the read
   (`PermissionDenied`, `Unimplemented`) has no cap and passes `Check`:
   boxd enforces the limit still. `MaxRunning` stays unset: boxd caps no
   number of running machines. boxd does not document the code it refuses a
   create with, so on any refused create the driver reads the limit again,
   and when `vm_count` has reached `max_vms` the refusal is `ErrCapacity`,
   naming the org and its count.
4. **`machinetest` gains a max-machines subtest**: it creates until refused
   with `ErrCapacity`, failing past the cap or with no VM made, then
   destroys one and creates again. `Config.SkipMaxMachines` names why a run
   must not fill a cap others share; boxd's run against boxd itself sets
   it, and its fake API runs the subtest at a limit of 4.
5. **boxd's `Check` refuses a key whose scopes hold a host-bound secret
   that reaches the driver's machines**: it lists the secrets with no org
   (`ListVars`), resolved as the driver's creates are, and counts scope
   `all`, `private`, and none (what a personal one may be listed as) as
   reaching. The error names each secret, its scope, and its hosts; boxd
   never returns a value. An in-machine token is checked the same way. The key's
   description in the definition states the requirement.

## Alternatives rejected

- **`max_vms` as `MaxRunning[linux]`.** The running machines can never pass
  it, so as an upper bound it is true. But `MaxRunning` promises that the
  cap's number of guests can run and that `Boot` past it is refused; boxd
  refuses a `Create` instead, counts stopped machines and other members',
  and `machinetest`'s max-running subtest would fail against it. Loosening
  that subtest, so a driver may refuse at `Create`, would make
  `MaxRunning` mean two things, and a caller could not tell from it whether
  stopping a VM makes room.
- **Reading the limit in `Capabilities`.** It takes no context, and every
  caller of it would make a call to boxd, under no deadline of theirs.
- **Counting machines in the driver.** boxd enforces the limit itself, and
  counts what the driver cannot list (other members' machines); a count
  of the driver's own would admit what boxd refuses.
- **Listing the org's secrets by name and the personal scope apart.** The
  credential fence resolves an empty org to the fenced one, so the second
  listing would be the org's again, read as personal, and refuse a key
  over a `shared` secret, the scope the error tells the user to move it to.
- **Matching boxd's refusal by its code or message.** Neither is
  documented. Asking the limit again on a refusal is right whatever boxd
  answers with.
- **Checking secrets only for a fenced create.** A host-bound secret
  reaches every machine of the driver's, fenced or not, and a check at
  `Check` fails before any VM is made, where discobox already calls it
  before it makes a pool's machine. A caller that never fences pays for
  nothing but moving the secret to another scope.

## Consequences

- discobox passes `MaxMachines` through to the pool's reported `runtime`
  condition as it does `MaxRunning`: a change there besides its pin.
- An org at its limit fails a create with `ErrCapacity` whose message says
  the org's count, so a caller tells it from any other refusal.
- A key whose org holds a host-bound secret of scope `all`, `private`, or
  none no longer passes `Check`.
- A limit raised after `Check` is seen at the next `Check`, or the next
  create boxd refuses.
