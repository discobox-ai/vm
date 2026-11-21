# 26-11-20-447 — An archil guest runs systemd, which the driver has Archil's supervisor start, and every guest operation enters

- **Status**: Proposed
- **Date**: 2026-11-20
- **Issue**: #193 (for discobox-ai/discobox's ADR 26-11-16-619 §4)
- **Supersedes in part**
  [26-11-03-676](26-11-03-676-an-archil-driver-puts-nothing-in-its-guests-and-makes-a-vm-a-sandbox-named-for-it.md):
  §1's "installs nothing, starts nothing, and changes nothing" in a guest,
  its rejected alternative "systemd booted by disco-vm", §4's guest
  operations run in Archil's own namespaces, and its Consequence that a VM
  that needs systemd arranges it itself. disco-vm still puts no agent of
  its own in the guest.
- **Relates to**: 26-11-17-311 (the fence a VM is made under).

## Context

disco-vm takes every Linux guest to run systemd: Windows and macOS aside,
what it runs is a machine, and discobox's ADR 26-11-16-619 §4 has the archil
driver start systemd and enter its namespaces, since an Archil guest has no
init of the image's. Today, at `a1fd6c3`:

- **Archil runs nothing of the image's.** PID 1 is `/opt/archil/init`; an
  image's ENTRYPOINT and CMD never run. Archil's REST API has no command at
  create, start, or fork: none of a sandbox create's 13 fields is one
  (checked against Archil's API reference, 2026-11-20).
- **Archil has a supervisor in the guest.** `/opt/archil/archil-client
  services create NAME -- ARGV` defines a service that starts at once, runs
  again when it exits, and comes back by itself at the next boot (vm#86,
  checked on Archil by hand, 2026-11-16). It is not in Archil's documented
  CLIs: it is the guest client's, on the read-only `/opt/archil`.
- **systemd runs below Archil's init.** Started by that supervisor as
  `unshare --pid --fork --mount-proc --kill-child /lib/systemd/systemd`, it
  was `running` with no failed unit, `systemd-run --wait` ran a unit, and the
  data disk was seen (vm#86). The process API runs in Archil's namespaces,
  so a command reaches systemd only through `nsenter`.
- **Archil's default image (`node:24-bookworm`) and `debian:sid` have no
  systemd.** A base that carries it (vm#194) is not published yet.

## Decision

### 1. Every archil VM runs systemd, with no option

There is no create option and no capability: an archil guest is a machine
with systemd, as every Linux guest of disco-vm's is.

### 2. Create has Archil's supervisor run it, once, under the VM's fence

While Create's sandbox runs, fenced as the spec asks, the driver runs a
script in Archil's namespaces:

- **systemd installed where the base has none.** Without
  `/lib/systemd/systemd`, `nsenter`, `unshare`, `setpriv`, `runuser`, or
  `mount`, it
  runs `apt-get install --no-install-recommends systemd systemd-sysv dbus
  util-linux`. The install is under the fence: a VM that can't reach its
  base's mirror (`--egress none`, or a list without it) fails Create, as
  does a base with no `apt-get`, each naming `/lib/systemd/systemd`.
- **The service `disco-vm-systemd` registered**, unless the supervisor has
  it, as `mount --make-rshared /` then `unshare --pid --fork --mount-proc
  --propagation slave --kill-child /lib/systemd/systemd --system`. The root
  shared and systemd's mounts its slaves let a mount made outside after
  systemd starts (a data disk) reach its units.

Where the supervisor has no answer for the service, a copy or a start waits
ten seconds for systemd to come up before registering one, since a second
would run a second systemd.

A fork, and so a VM made from a committed one and an upgrade from one,
carries the root and the service, and the script finds both. Start runs it
too, installing nothing: a VM made before this whose root has systemd gets
the service at its next start, and one whose root has none is refused and
stopped (an upgrade gives it one). Create and Start each return once
`systemctl is-system-running` is `running`, `degraded`, or `maintenance`.

### 3. Every guest operation enters systemd's namespaces

Exec, copies in and out, info, and terminals are a line that finds the
process whose PID in its own namespace is 1 (`NSpid`), and runs the request
in its namespaces with `nsenter --all`, then `cd` to the request's
directory, and `runuser` for another account. Archil's kill SIGKILLs the
process's whole group, which the command in systemd's namespaces stays in,
so a process Archil kills ends there too (seen 2026-11-21). Should `nsenter`
alone die, `setpriv --pdeathsig TERM` sends the command TERM, so a
script's traps still clean up. `runuser` forks the account's command, and the
change of account clears the signal, so it is set again under `runuser`.
While systemd does not run, an operation fails saying so.

## Alternatives rejected

- **A create option for it.** The issue asked for one. disco-vm takes
  systemd as given in every Linux guest, and an option would need the
  driver to tell, at each operation, a VM with it from one without, with
  nothing on Archil to record which.
- **The driver starting systemd at every Boot and Start.** It works, and is
  an exec per start; a guest that resets itself (a stop, to Archil) comes
  back without systemd until the driver starts it again, and a request with
  a port token starts a VM with no call of the driver's. Archil's supervisor
  starts it at every boot, whoever started the VM.
- **Requiring a base with systemd.** It keeps disco-vm out of the guest's
  packages, but no such base is published (vm#194), and Archil's default and
  `debian:sid` would both be refused. Installing on demand boots one without
  a new image; a base with systemd skips the install.
- **Installing before the fence, then fencing.** Archil sets a policy on a
  running sandbox, so the install could run open. It leaves a fenced VM
  unfenced for its first seconds; a VM fenced from its mirror needs a base
  with systemd instead.
- **Installing in the boot service.** A boot under a fence can't install,
  and the failure would be the guest's, after Create had returned.
- **Entering systemd's mount namespace alone, or with `nsenter --no-fork`.**
  A command outside systemd's PID namespace sees a `/proc` that is not its
  own, so `/proc/self` is broken; entering all of them forks, which
  `setpriv`'s parent-death signal ties to the `nsenter` Archil kills.
- **A cgroup namespace of systemd's own.** systemd takes the guest's cgroup
  root, beside Archil's init and agent in `/`; nothing broke in vm#86.
  Untried, and left until something does.

## Consequences

- **A fresh upgrade can fail after its sandbox is deleted.** Archil can't
  rename a sandbox, so the new root boots under the name only once the old
  is gone; a base without systemd fenced from its mirror fails there, as a
  base that does not pull does (ADR 26-11-03-676 §3a). A VM with a data disk
  is left mid-upgrade, which an upgrade from a committed VM with systemd, or
  of a base with it, finishes; one without is gone.
- **A VM running since before this** fails its guest operations, saying
  so, until its next start registers systemd. A copy of a VM committed
  before this, fenced from its mirror, fails Create as a fresh one does.
- **Create of a bare base takes a package install,** about 40 s on Archil
  for `debian:sid`, and needs its mirror (one fenced from it fails in
  about as long); a base with systemd (vm#194) skips it. Start waits for
  systemd to settle.
- **A base without systemd fails a fenced Create.** The live conformance
  suite's fresh VM fenced to nothing needs `DISCO_VM_ARCHIL_IMAGE` to name a
  base with systemd. Alpine, which has no systemd, is refused.
- **disco-vm changes the guest** at Create: systemd's packages and a service
  of Archil's supervisor. The guest is still the caller's otherwise.
- **The supervisor's CLI is not Archil's published interface,** as the
  process protocol is not: a change to it breaks Create and Start until the
  driver follows it. `services get` answers the service as JSON, and exit
  1 for none (seen on Archil, 2026-11-21).
- **The container test** (`TestSystemdInAContainer`, `go tool task
  test:archil:container`) runs the driver's own lines in Docker containers
  whose PID 1 is sh, with a stand-in for the supervisor: it proves systemd,
  its namespaces, the mounts, and a restart there, not Archil's supervisor.
  systemd there runs in container mode (it sees Docker's markers); as a
  Firecracker VM's PID 1 below Archil's init it starts the units a machine
  has. On Archil (2026-11-21) it was `running` with no failed unit, a
  console getty among its units, and `systemd-detect-virt` said
  `container-other` there too.
