# 26-10-15-703 — disco-vm does not converge guests; a client above it does, through exec, copy, and snapshot

- **Status**: Accepted
- **Date**: 2026-10-15
- **Supersedes**: [0003](0003-a-client-converges-a-guest-to-state-files.md)

## Context

ADR 0003 put converge in disco-vm: state files, a record in the guest, a
lease, and scripts for each guest OS in `pkg/converge`, reached through
`disco-vm converge`, `Engine.Converge`, and a build spec's `converge:` step.

Converge needs nothing of disco-vm's beyond what every machine already
offers. It reaches a guest only through exec and copy (ADR 0001), and it
learns the guest's OS from the agent's info. What it owns is a policy: which
packages and files an image holds, how a difference is applied, and what a
guest records. That policy is the product's, discobox's, and it changes on
discobox's schedule, while disco-vm builds images and runs them on four
hypervisors.

Keeping it here costs disco-vm a second place guest-OS knowledge lives
(scripts above the seam, beside `pkg/guest`), a build step that depends on
it, and the tests for both on every OS's CI. A Windows failure in those
scripts kept `main` red across three pushes that did not touch them.

`disco-vm snapshot` (ADR 26-10-15-882) now commits a stopped instance as an
image. With exec, copy, stop, and snapshot, a client can make an image any
way it likes, converge included, with nothing in disco-vm knowing how.

## Decision

disco-vm has no converge. It offers the operations a client converges with,
and a client above it, discobox, owns converging.

- **Removed**: `pkg/converge`, `disco-vm converge`, `Engine.Converge` and
  `Engine.ConvergeStatus`, the build spec's `converge:` step, and the state
  file reference (`docs/state-file.md`). A build spec with a `converge:` step
  is refused as an unknown key, as any other is.
- **Removed with it**: the guest info's `Root`, which only converge read. A
  fake guest still tells the processes it runs its root
  (`DISCO_VM_GUEST_ROOT`).
- **Kept**, because they stand on their own: `cp SRC/.` copying a
  directory's contents, and docker-init giving the guest's root user the
  container's `/`.
- **What a client converges with**: `Engine.Guest` (exec and copy),
  `Engine.Stop`, and `Engine.Snapshot`, or the same through the CLI. An image
  made by converging is: create an instance, converge it, stop it, snapshot
  it.
- Guests that a converge run already touched keep its record and scripts on
  their disks. disco-vm never reads them, so they do no harm; the converger
  that wrote them can read them again wherever it now lives.

## Alternatives rejected

- **Keep converge in disco-vm** (ADR 0003). It is disco-vm's only code that
  decides what an image holds rather than how to build or run one, and it
  ties the product's image policy to disco-vm's releases and CI.
- **Move it to `discobox-ai/x`.** `x` holds generic helpers. Converge is one
  product's image policy, not a helper other programs share.
- **Keep the `converge:` build step and drop only the command.** The step
  needs all of `pkg/converge`, so nothing would leave. A build that wants
  converged state can be replaced by create, converge, stop, snapshot.

## Consequences

- disco-vm's guest-OS knowledge is back in `pkg/guest` alone.
- A client that converges carries its own copy of the logic. The last
  version is `pkg/converge` at 871f7ac, plus the Windows link fix in PR #12,
  in this repository's history.
- A converged image is a snapshot, not a build layer: it has a random ID and
  no cache key, so it is not rebuilt from its inputs. The client decides
  whether to converge again or to reuse a snapshot it made.
- discobox's images move their state files to the client that converges
  them.
