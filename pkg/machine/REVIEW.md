# pkg/machine review notes

- **A libkrun stream's host side never half-closes.** libkrun 1.19 resets a
  vsock stream the host half-closes, before the guest has read what came
  before, so every host-side conn of `pkg/machine/libkrun` overrides
  `CloseWrite`: a no-op on a caller's `Dial` (the guest never reads the
  caller's EOF), a full close on a guest's forward (the guest reads EOF once
  the far end is done). A new conn type there, or a splice that half-closes
  by another path, gets one of the two, never the default. A review found a
  forward that kept its half open and hung a guest that read to EOF.
- **Every guest port a caller reaches is known at boot** (ADR 26-11-03-247):
  `Machine.Dial` takes the agent's port and the service's
  (`guest.DialablePort`), and a forward's ports are in the record. Widening
  `DialablePort`, or dialing a booted machine past the seam's checks, passes
  every test on hcs, vz, and fake, whose hypervisors connect to any port at
  any time, and fails only on libkrun, which maps ports before its VM starts
  and never after.
