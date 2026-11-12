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
- **A peer forward cannot refuse.** The shim's listener has accepted the
  guest's connection before it can know the peer is stopped, so the guest
  connects and reads EOF. Write it that way wherever it is described.
- **Create refuses a malformed forward itself (`checkForwards`), whatever
  the engine checked.** A served driver's server (`pkg/machine/served`)
  calls it with what a pool sent, and the engine's checks ran on the pool's
  side; a forward the driver took on trust is dialed by the shim with the
  owner's privileges.
