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
- **A driver's setting is a field of its configuration, never an
  environment variable it reads** (ADR 26-11-11-215). A new one is a field of
  the driver's `Config`, a `ConfigField` in its `Definition` with the
  variable the command line reads as `Env`, and a check in its `decode`. An
  `os.Getenv` in a driver passes every test that sets the variable, and
  fails for an embedder, which sets none, and for a shim, which reads its
  configuration from the VM's file. A process-wide convention every client
  honours (boxd's `ALL_PROXY`, as gRPC honours `HTTPS_PROXY`) is not a
  driver's setting.
- **A new field is placed on the privileged list or argued off it**
  (`TestPrivileged`): one that names a host path, socket, or directory, or
  an endpoint the key or a guest's data reaches, is `Privileged`. A
  validator's error names the field and never its value, and a path is
  checked with `machine.CheckFile` before anything reads it.
- **A field is never removed or renamed**: it is `Retired`, with
  `ReplacedBy` where it has a replacement, and stays accepted for good.
- **A driver's configuration that scopes what it lists is part of the
  reaper contract.** The pool does, and so does a region (boxd's and
  archil's `region`): a driver lists only its region's VMs, so a reaper that
  lists with another region, or none, never sees them and they are billed
  forever. A new setting that narrows what `Instances` returns updates the
  reaping paragraph of `pkg/engine/DESIGN.md` in the same change. A review
  found the region added without it.
