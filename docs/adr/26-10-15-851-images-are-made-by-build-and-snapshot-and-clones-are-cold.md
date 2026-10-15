# 26-10-15-851 — Images are made by build and snapshot, and every clone boots cold

- **Status**: Accepted
- **Date**: 2026-10-15

## Context

An image is made fast to start in two ways. A build or a snapshot
(26-10-15-882) commits a guest's disk, so an instance of the image boots with
the work already done. Warming stages an image on this host so that clones
skip the boot: hcs freezes a live template that clones fork from, and vz and
boxd save a running guest that clones resume. `run` and `create` default to
`--mode auto`, which takes a stage when there is one and falls back to cold.

Warming is the larger of the two, and the less settled. It is the
`machine.Warmer` seam (`Warm`, `Warmth`, `Cool`, `Stage`), the `fork` and
`resume` clone modes and `Capabilities.CloneModes`, `ErrNotWarm`, the warm
shim, the `disco_vm_warm_stages` table and `warm/<layer>/`, `warm --user`
with vz's login machinery, and in every fast-mode driver a second Prepare and
a first Boot unlike the cold one. Its failures are its own: a vz restore that
needs an unlocked screen, a stage gone stale after a host update, an hcs
clone that dies when its template is cooled, a stage made before a change
(ADR 0005) that has to be warmed again. Each new feature has to be proven
against both kinds of clone. Build and snapshot are what the drivers have to
get right first, and they need none of it.

## Decision

Remove warming. Images are made by `build` and `snapshot`, and every
instance is a cold clone of its image.

- **The seam loses warming.** `machine.Warmer`, `Stage`, `WarmSpec`,
  `Warmth`, `ErrNotWarm`, `CloneMode` and its modes,
  `Capabilities.CloneModes`, `InstanceSpec.Mode` and `WarmDir`, and
  `Known.Stages` go. A driver's Prepare makes a cold clone, and nothing else.
- **The engine and CLI lose it.** `Engine.Warm`, `Cool`, `Warmth`, `Stage`,
  `IsWarm`, and `RunWarmShim`; `shim --warm`; the `warm` command; `--mode` on
  `run` and `create`; the WARM column of `images`, and `rmi`'s refusal of a
  warm image. The builder no longer has to say it clones cold.
- **The drivers lose it.** hcs's fork (`fork_windows.go`), vz's templates and
  logged-in stages (`warm_darwin.go`, `login_darwin.go`), boxd's stage
  snapshot and resume, and fake's pool, with their tests and the `machinetest`
  and `internal/e2e` warm cases.
- **State from before is upgraded on open.** The `disco_vm_warm_stages` table
  is dropped, and `warm.json` is no longer imported. An instance's `mode`
  column stays in the store and is no longer read. `warm/` under a root is
  left where it is, and the upgrade notes say to stop any warm shim and
  delete it; a boxd stage snapshot is no longer the store's, so it is listed
  as an orphan and `rmi` removes it by name.
- **Instances cloned fast before this keep what they can.** One that has
  booted once is a cold instance on its own disk: a vz resume clone is a
  bundle, a boxd one a machine, and an hcs fork clone's disk differences over
  the template disk it hard-linked into its own directory. hcs still refuses
  to commit a fork clone, by that file, since its layer would depend on it.
  One prepared fast and never booted (vz's `resume.pending`, an hcs fork
  ticket) is refused at start, with the instruction to remove it and run the
  image again.

## Alternatives rejected

- **Keep warming, and default `--mode` to cold.** It takes warming off the
  common path but leaves all of it in the seam, every driver, and the store,
  still to be kept working through each change. Not using it does not make it
  cheaper to carry.
- **Keep the seam, and remove only the CLI.** discobox embeds the engine and
  would keep reaching it, so every cost above stays. Removing the CLI alone
  only hides what still has to be maintained.
- **Remove the `mode` column and the hcs fork-clone check.** Instances made
  before this still exist, with a mode in their rows and fork disks on hcs. A
  column no one reads costs nothing, and committing an hcs fork clone would
  make a layer that silently depends on a file in a removed instance's
  directory.

## Consequences

- Starting an instance is a cold boot everywhere, so `run` is slower where an
  image was warm: vz from about 6 s to a full macOS boot, hcs from about a
  second to a full Windows boot, boxd from a resume to a boot.
- The fastest way to a ready guest is to put the work in the image: a build
  step, or `snapshot` after shaping a guest by exec and copy.
- The seam, the store, and every driver's Prepare and Boot get smaller, and
  `machinetest` and the proof table have one kind of clone to prove.
- discobox's plan in `docs/discobox.md` loses its fast path (`engine.Warm`,
  `warm --local-user`), and the doc is updated to say so.
- Capabilities that warming would have made use of, such as `run --warm` and
  `build --warm`, are no longer planned.

## Deferred

Fast clones. Revisit when every driver passes `machinetest` and
`internal/e2e`, builds and snapshots are proven on each in the proof table,
and a caller (discobox's sandbox pool) measures cold start as what limits it.
A return gets its own ADR and design, starting from what this one removed
rather than restoring it as it was.
