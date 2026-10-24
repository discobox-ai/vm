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
- **Nothing above the seam keeps state, and a driver keeps it only in its
  provider.** A field an instance or an image needs goes on the provider's
  resource (a Docker label, a boxd name, a local driver's record beside its
  disk), never in a file or a table of the engine's; a remote driver writes
  nothing on the host. A fact boxd cannot keep (an image's comment, the image
  a machine was restored from) is reported missing, not remembered elsewhere.
  (ADR 26-10-23-417.)
- **A listing that fails is no answer.** `rmi` reads a failed listing of
  tags or instances as "in use", never as "unused", and a reaper
  above the engine does the same. (ADR 26-10-23-417.)
- **No image names another above the seam.** An image has no parent, an
  instance runs from one image, and `rmi` deletes one image. A hypervisor
  whose disks chain (hcs, `local.Hypervisor.Chains`) has `local` record the
  parent beside its disk and refuse `DeleteImage` with `machine.ErrInUse`
  while another image's record, in any project, names it; nothing above the
  seam may record or walk a chain. (ADR 26-10-23-410.)
- **`local` reads every image record before it deletes a disk.** A record
  it cannot read (a parse error, a permission) makes the listing an error,
  never a record left out: `DeleteImage` learns from it which disks another
  image differences over, and a record skipped is a parent deleted from
  under its child. (ADR 26-10-23-410.)
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
