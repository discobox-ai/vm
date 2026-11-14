# pkg/machine/local review

- **A shim client made per call keeps no connection.** `openShim` runs on
  every Attach, stop, and kill, so its `http.Transport` disables keep-alives:
  one kept idle would hold a descriptor in the caller and a goroutine in the
  shim per call, without end in an embedder that runs for days. A review
  found the leak once.
- **A boot holds its admission until its machine is done.** Every path that
  runs a guest goes through `admit`: `boot`, and `Install`, since vz's
  install runs its installer and the installed system. A new one must too,
  or the cap is exceeded deep in the hypervisor with no `ErrCapacity`. A
  machine `boot` returns is wrapped so its `Done` closes, and its `Kill`
  returns, after the release (a review found `Kill` returning first), and it
  passes `machine.HostListener` through: anything the shim asserts on a
  machine must reach the hypervisor's.
- **An unfinished upgrade's error names a target its caller can pass.**
  `whole()` tells Boot, Start, Commit, and a mismatched Upgrade how to
  finish, and a target another pool's (`FromPool`) is one only an
  embedder's `Engine.From` names: no `--from` reaches it. A review found
  advice that sent a pool's engine looking in its own pool, the second
  resume path missed after the root's note on finishing an interrupted
  step.
- **A shim outlives the binary that started it, so a new route has a plan
  for an old shim.** An instance started before an upgrade keeps its shim
  until it stops. `/kill` falls back to a stop; `/display` turns the old
  shim's 404 (`errNoRoute`) into "restart the instance". A review found a
  new route with no plan twice.
- **A nil `*shimMachine` is never returned as a `machine.Machine`.** The
  interface holding a nil pointer is not nil, so a caller that checks
  `m != nil` calls into it. Return an untyped nil with the error, as
  `Attach` does.
- **A peer forward cannot refuse.** The shim's listener has accepted the
  guest's connection before it can know the peer is stopped, so the guest
  connects and reads EOF. Write it that way wherever it is described.
- **Create refuses a malformed forward itself (`checkForwards`), whatever
  the engine checked.** A served driver's server (`pkg/machine/served`)
  calls it with what a pool sent, and the engine's checks ran on the pool's
  side; a forward the driver took on trust is dialed by the shim with the
  owner's privileges.
- **A shim's protocol moves with what it serves.** A change a shim of
  another build would misread, to `/guest` or `/dial` or what they carry,
  raises `shimProtocol`; a change to `guest.Protocol` reaches the relay by
  itself. The part of a shim that lasts (`shimState`'s PID, socket, address,
  token, and service, and `/status`, `/stop`, and `/kill`) never changes, or
  an engine could not stop a shim of another build.
- **A field a VM inherits from the VM it is made from goes through every
  way it is made.** Create from a committed VM, Commit (which keeps the
  record), and Upgrade, which holds it in the unfinished upgrade's
  `upgradeTarget`: that target is written before the upgrade starts, and
  the committed VM it came from may be gone by the time a resumed upgrade
  finishes, so the field has to travel with it. A record written before the
  field existed is filled from what the hypervisor keeps where it can
  (`Hypervisor.Release`), not left empty. `pkg/engine`
  TestInstallRecordsItsRelease is the test a new inherited field copies; a
  review asked for each of these for `Instance.Release`.
- **Every way out of `Start` after the shim is spawned settles it.** Only an
  attached shim is told it is started (`startedShim.started`); every other
  return abandons it (`startedShim.abandon`), or a shim booting slower than
  its caller waits holds a guest and its slot behind an instance that reads
  as stopped. A review found a deadline that left one running (#130).
- **Killing a shim's process does not stop its VM.** An HCS VM belongs to
  vmcompute, and a fake guest is a child of the shim; only a vz or libkrun
  VM dies with it. A shim is ended through its boot (its lifeline, its
  context) or its control API, never by killing its process, or the guest
  runs on with no shim, no `shim.json`, and its slot released. A review
  found a give-up that killed the shim (#130).
