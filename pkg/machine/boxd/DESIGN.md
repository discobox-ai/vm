# pkg/machine/boxd

A layer is a snapshot, and the guest is reached through Exec.

boxd offers machines and snapshots of running machines, not disk images, and
no socket into a guest. The boxd driver maps the seam onto what it has, with
nothing of disco-vm's in the guest. See
[docs/drivers/boxd.md](../../../docs/drivers/boxd.md).

- **A layer is a snapshot** (memory and disk) of the guest booted from the
  committed disk. Commit starts the stopped instance and snapshots it, because
  boxd only snapshots a running machine. The layer is still cut from an
  orderly shutdown: the snapshot's disk is the one the guest flushed.
- **A clone** restores the layer's snapshot and stops it, so its first boot
  is a cold boot of that disk. **An instance of a base** has no machine until
  its first Boot makes one, from the image (`CreateVm`) or the snapshot
  (`CreateVmFromSnapshot`), at the size that boot asks for.
- **The guest's operations are Exec streams.** Each is one command line run
  as boxd's own user: `sudo -n -H -u USER -- env -C DIR -- K=V... ARGV...`,
  every word quoted, root unless the request names a user. Stdin, stdout,
  stderr, the exit code, and a TTY with its size and resizes are Exec's own.
  A copy in is `tar -x` reading the stream, and a copy out writes one ready
  byte once it has found the path and then `tar -c`, so a missing path is an
  error, not an empty tar. Info is `uname -m` and `hostname`. Boot, Commit,
  and Install wait until `systemctl is-system-running --wait` answers, so a
  command runs in a booted system. boxd authenticates every stream. An image
  installed before this carries an agent that nothing reaches; it does no
  harm.
- **The URL is the machine's default proxy.** Prepare (an instance of a
  base's first Boot) pins the default proxy
  at `<machine>.boxd.sh` to 7302 and turns off bot protection, which would
  put a browser challenge in front of a request that wakes the machine.
  Pinning it also stops the proxy forwarding to whichever common port the
  guest listens on (80, 443, 8080, 8000, 3000, 5000, 5173).
- **A machine's power is boxd's** (ADR 26-10-15-833). Its state is `GetVm`'s
  status. An orderly shutdown runs `sync` in the guest, since boxd does not
  say whether `StopVm` cuts the power, then `StopVm`; a reboot is `RebootVm`,
  and Kill is `StopVm`. A guest that powers itself off from inside is not
  supported: boxd leaves it `running`, and so does disco-vm, until `disco-vm
  stop`. An image built before this has a shutdown hook that nothing reads.
- **The driver talks to boxd's public gRPC API directly**, not through the
  boxd CLI: it works from a server that embeds the engine, from any host OS,
  and with an API key (`BOXD_API_KEY`) rather than a browser login.
