# 0003 — A client converges a guest to state files, through exec and copy

- **Status**: Accepted
- **Date**: 2026-10-13

## Context

An image is made by a build spec's `run` steps, which apply once, at build.
An instance of an old image has no way to reach a newer image's state except
to be replaced by an instance of the newer one. Replacing is not always
possible: an instance's disk is a copy-on-write clone of its image and also
holds what its user wrote, and not every provider keeps that on a disk of its
own. discobox's control plane upgrades on its own schedule, so the guests it
meets are of every age.

Most of what a discobox image holds is not a procedure. Its harness images are
one package and a handful of files each, and its base is about seventy
packages, a hundred files, and a list of units to enable or mask, around a
dozen installers that have to be scripts (Nix, Homebrew, rustup). State of
that shape can be declared, compared with what a guest already has, and
applied as a difference.

Exec and copy are what every machine offers (ADR 0001): a driver with an
agent in the guest provides them through it, and boxd through its own API.
Something built on those two alone needs nothing installed in a guest.

## Decision

Desired state is state files on the client. `disco-vm converge` and
`Engine.Converge` read them there, and make a running guest match them
through exec and copy. The guest holds only the record of what was applied.

### 1. A state file declares packages, files, services, and guarded scripts

Unknown keys are errors, as in a build spec.

```yaml
packages:
  apt: [jq, ripgrep, "nodejs=25.*"]   # the key is the provider
  npm: ["@anthropic-ai/claude-code"]
files:
  - path: /etc/claude-code/managed-settings.json
    source: ../managed-settings.json  # on the client, from this file's directory
    mode: "0644"                      # default 0644, or 0755 for a directory
    owner: root:root                  # default
  - path: /etc/profile.d/harness.sh
    content: |
      export DISABLE_AUTOUPDATER=1
  - path: /usr/local/bin/discobox-hook-publish
    link: /usr/local/bin/discobox-sandbox-agent
  - path: /usr/local/libexec/discobox
    directory: true
services:
  enabled: [docker.socket]            # enabled and started
  disabled: [docker.service]          # disabled and stopped
  masked: [apt-daily.timer]           # masked and stopped
exec:
  - name: nix
    key: "3"                          # runs again when key or run changes
    shell: [/bin/bash, -o, pipefail, -c]   # default: the guest's shell
    run: |
      curl -fsSL https://install.determinate.systems/nix | sh -s -- install
```

- **A package** is satisfied by any installed version unless one is given, so
  an unchanged guest never asks the network. Providers at first: `apt`, `npm`.
- **A file** is exactly one of `source`, `content`, `link`, or `directory`. A
  managed path is overwritten, whatever was there. A `source` that is a
  directory is every file under it, each a resource of its own, so a file
  that leaves the directory is deleted like any other.
- **A service** is a systemd unit at first. Units are turned off before any
  is turned on: a socket cannot start while its service runs on its own.
- **An `exec`** runs as root in the guest's default shell. It is the only
  resource that cannot be compared with the guest, so it runs once per `key`
  and script and must be safe to run again.

### 2. A run is given state files, and applies each one whole, in name order

A run takes any number of state files, or directories of them, and orders
them by name. Within one: files, then packages, then `exec`, then services,
each in the order declared. Files are first so a package source and its key
are there before the package is asked for, and a later state file can need
what an earlier one installed.

- **Runs add up.** A run touches the state files it is given and leaves the
  guest's others as they are, so an image layer, or one changed file, is a
  run of its own. The record keeps, for each resource, the state file that
  declared it.
- **A later state file's declaration is the resource**, the way a later image
  layer's file is the file, whether the two are given together or in two
  runs. Twice in one state file is an error.
- **Removal.** A file that leaves a state file is deleted when that state
  file is next given. A package or a service that leaves is forgotten and
  left as it is. A state file is dropped by giving it empty.

### 3. The guest holds the record, and the client applies the difference

`/var/lib/disco-vm/converge` (`C:\ProgramData\disco-vm\converge` on Windows)
holds `record.json`, the digest each resource was applied at, and
`status.json`, how the last run ended.

- **A record, not a probe.** A resource whose digest is in the record is not
  looked at, so converging an instance to the state files its image was built
  from applies nothing. What a user changes by hand stays changed until the
  declaration does. A snapshot carries the record, so an image is a guest
  that was booted, converged, and snapshotted.
- **Batches, not round trips.** One state file's pending files are one copy
  and one script, a provider's packages one script, its services one script.
  A script is copied in as a file and run with the shell the guest OS has
  with nothing installed, sh or PowerShell, chosen by the OS the guest
  reports. The record is written after every batch.
- **Failure.** The first resource that fails ends the run with its error,
  which the status keeps. What was applied before it, within the same batch
  too, stays recorded, so the next run starts at the failure. State files
  that do not parse fail before the guest is touched.

### 4. A run holds a lease on the guest

Two clients must not converge one guest at once. A run takes a lease in the
guest before it reads the record: a file naming the holder and when the lease
runs out, by the guest's clock, made in one step that fails if it exists.

- The lease lasts a minute, and the run renews it every thirty seconds.
- A run that finds a live lease fails at once with the holder's name
  (`converge.ErrLeaseHeld`). It does not wait.
- A lease that ran out is taken over, by a rename only one client can win.
- A run whose heartbeat finds the lease another's, or that has not reached
  the guest for a whole lease, stops: by then another client may hold it.
- A run that ends gives the lease back.

### 5. From the CLI, the engine, and a build

```
disco-vm converge box harness/claude-code/converge   # apply; 0, or 1 with the error
disco-vm converge box                                # the last run's status
```

`Engine.Converge(ctx, inst, converge.Options{Paths: …})` is the same call for
a program that embeds the engine. In a build spec, `converge: PATH` is a step
that converges the layer's guest to a context state file or directory, and
its cache key is a digest of what that declares, the content of every source
included.

### 6. Where it lives

`pkg/converge` holds all of it: the state file, the digests, the run, the
lease, and the scripts for each guest OS. The scripts are text on the client,
chosen by the guest's reported OS, never by the client's. A guest that is a
directory (the fake driver) reports its root; a run in it writes packages,
services, and owners to a ledger and does not change the host, so
`internal/e2e` covers a build, a no-op, a change, and a removal on every OS.

## Alternatives rejected

- **The guest converges itself.** This was the first design, and was built:
  the agent watched a directory of state files in the guest and applied them
  at boot and on change, and a client copied files in and waited. Rejected
  for what it cost:
  - The logic was the guest's version, pinned in every image. Each change to
    the converger meant reinstalling the base layer, which is the staleness
    converge exists to remove.
  - Every guest needed the `disco-vm` binary, against ADR 0001, which takes
    the agent out of boxd images.
  - Sources had to be copied into the guest and kept there, a second copy of
    every file the image installs.

  What it had and this does not is a guest that converges with no client: at
  a boot nothing watches, or because a program in the guest changed its own
  state files. See Deferred.
- **Compare with the guest's real state on every run.** It costs a package
  query and a read of every managed file, and it undoes what a user with
  root did on purpose.
- **Declarative steps in the build spec only.** They would apply at build,
  which leaves an existing instance where it is today.
- **An existing tool (Ansible, cloud-init).** A runtime or a second binary in
  every guest, and no Windows or macOS guest support we could own.
- **Wait for a held lease.** A caller that wants to wait can retry; a run
  that blocks on another client's lease hides which of them is stuck.

## Consequences

- A guest needs nothing installed to be converged, and the logic is always
  the client's version.
- A run lasts as long as its client's connection. A client that dies leaves
  the guest part applied, with the record true and the lease running out in
  a minute; the next run carries on. What the guest was doing when the
  client died (an `apt-get`) is the driver's to end or not.
- The scripts for each guest OS are in `pkg/converge`, above the seam, as
  text keyed on the guest's OS. That is a second place guest-OS knowledge
  lives, beside the agent's own code in `pkg/guest`.
- The agent's info gains the root of a guest that is a directory.
- The build spec gains the `converge` step, and `docs/build-spec.md` and a
  state file reference document it.
- discobox's images are state files beside their Dockerfiles.

## Deferred

- **A guest that converges with no client**: at boot, or from inside.
  Revisit when something in a guest has to change its own state; the record
  and the state file format would not change.
- **Repairing drift** (comparing with real state). Revisit when a guest is
  found out of step with its record.
- **Removing packages and restoring services.** Revisit with a use for it.
- **Restarting a service when a file it reads changes.** Revisit with the
  first state file that needs it; until then an `exec` does it.
- **Windows and macOS providers** for packages and services (winget, brew,
  Windows services, launchd), and a file's owner on Windows. Files and `exec`
  work there from the start.
- **A download resource** (URL, checksum, extract), `when:` on a resource,
  and templates or per-user files. Until then an `exec` and discobox's own
  manifest cover them.
