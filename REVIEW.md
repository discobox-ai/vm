# disco-vm review notes

- **A `machine.Guest` handed out holds no connection between calls.**
  `Engine.Guest` returns one with nothing to release it, so `guest.Client`
  keeps no connections alive. An idle connection is a hijacked conn and a
  goroutine in a shim, or a `docker exec` in a guest, for every call an
  embedder makes. (Commit 335ed4b.)
- **The boxd fake API reads a command line the way the guest's tools do.**
  Its Exec parses what `pkg/machine/boxd` sends; a parser written from the
  driver's own idea of the line agrees with the driver's mistakes. GNU `env`
  ends its options at the first `NAME=VALUE`, so a `--` after the variables
  was the command, and the fake passed it until boxd itself refused it.
  (Commit 335ed4b.)
- **The boxd fake API accepts only what boxd's published API documents.**
  Every field it widens (a snapshot found by name or ID) is one that
  docs.boxd.sh/reference/grpc-proto says boxd takes, and the hand-kept proto
  carries that page's comment on the field. A fake widened to match what the
  driver sends passes a test that boxd itself would fail. (ADR 26-10-15-833:
  a base's machine is made from its snapshot's ID.)
- **A driver's `Machine.Shutdown` returns once the stop is accepted, not
  before.** A caller may exit after it, and a stop left in a goroutine of its
  own dies with that process, leaving the machine running. Work before the
  guest accepts (boxd's `sync` before `StopVm`) runs inside the call, which
  `Booted.Shutdown` bounds by the shutdown's whole timeout, not a short ask
  window; a failure is returned, so the caller falls back to `Kill`.
  (ADR 26-10-15-833.)
- **On boxd, an instance of a base has no `vm.json` until its first Boot.**
  Every path that reads it (Boot, Attach, Service, Commit, Destroy) says what
  a never-started instance is, not "no such file". (ADR 26-10-15-833.)
- **A change to a shared method in `pkg/guest` or `pkg/machine` needs a
  search of `pkg/machine/vz` by hand.** Its cgo files, its tests among them,
  compile only on a Mac, so neither Linux nor `GOOS=windows` vet sees a
  caller left behind there. (Commit 335ed4b.)
- **A field removed from a struct that is read from a file and written back
  stays, as `json.RawMessage`.** vz's `vz.json` and hcs's `state.json` are
  read into a struct and rewritten whole, so a field that is no longer
  declared is erased on the next write, along with whatever it held: a warm
  user's only password, in vz's case. Keep it opaque, with a comment saying
  why. (ADR 26-10-15-851.)
