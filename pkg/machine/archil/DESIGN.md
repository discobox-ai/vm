# pkg/machine/archil

A VM is the sandbox named for it, and nothing else is kept (ADR
26-11-03-676, ADR 26-11-03-606). See
[docs/drivers/archil.md](../../../docs/drivers/archil.md).

Archil's sandboxes are Firecracker microVMs behind a REST API, beside disks
the provider mounts in them. A sandbox has no labels and its name never
changes, which is all a VM needs. The driver has nothing of disco-vm's in
the guest and nothing on the host:

- **A driver is one region's**, its configuration's `region`
  (`aws-us-east-1` by default). Each region is its own control plane, with
  its own sandboxes and disks, so a pool sees only its region's VMs.
- **A VM is the sandbox `dvm-<pool>--<name>`** (`names.go`). Archil
  refuses a sandbox name it has, so a Create takes the name before it makes
  anything else, and the refusal is `machine.ErrExists`. A listing reads the
  sandboxes under the pool's prefix, and Archil holds each one's fence and
  data disk. A sandbox whose name is no VM's, such as an earlier driver's
  `dvm-<pool>-<key>`, is not listed.
- **The data volume is the disk `dvm-<pool>--<name>--data`**, made once
  the name is taken and mounted at each start. Archil answers a disk of a
  name it has rather than making one, so a data disk with no sandbox is a
  VM: listed, and Create of its name is `ErrExists`. A sandbox's name is at
  most 63 characters, so a pool and a name are at most 57 together.
- **Upgrade** (ADR 26-11-03-782) deletes the stopped VM's sandbox and makes
  it again under its name, fresh or a fork, at its size and fence, after
  checking all that could refuse it. One stopped in between is its data disk
  alone, listed with no size and the fence `none`: Start refuses it, Destroy
  removes the disk, and Upgrade again finishes it with the Instance's size
  and fence. A VM with no data disk is gone if its upgrade fails or stops
  after the delete: the one exception to ADR 26-11-03-782 §3. A sandbox not found by an Instance's ID is found by its name,
  so an Instance from before an upgrade still names the VM.
- **A fresh VM** is a sandbox of `Config.Base`, an OCI image, or of
  Archil's default image. **A VM created from another** is a fork of its
  sandbox, which must be stopped and have no mounts, since a fork boots with
  its source's. A fork keeps its source's size: another is refused. Either
  way Create sets the spec's fence and reads it back, exposes 7302, and stops
  the sandbox.
- **Commit** leaves the stopped sandbox as it stands, with no mounts and no
  data disk: a sandbox with mounts is started once with none and stopped,
  since only a start changes mounts. Nothing records the commit, and Archil
  can't tell a committed VM from a stopped one, so Start does not refuse
  one: the caller never starts it.
- **Copies chain.** A fork pins its source, so Destroy of a VM with forks is
  `machine.ErrInUse`, as hcs's chain is.
- **The guest's operations are Archil's process API** (`process.go`,
  `guest.go`), ported from Archil's MIT TypeScript SDK, since the protocol is
  not in its OpenAPI document: a WebSocket per process, stdin as binary
  frames, output as framed stdout and stderr with offsets, kill and resize on
  a connection of their own. Each stream's unread output is held to 4 MiB,
  so a slow reader slows the process rather than the host filling up, and a
  kill drops the rest. Exec is one quoted command line with the
  request's directory, variables, and terminal, and `runuser` (util-linux:
  Alpine has none, so `-u` fails there) for another
  account; copies are tar, a copy out writing one ready byte first and then
  the tar a 512 KiB chunk at a time as the host asks, since Archil drops
  output that outruns its connection (an exec that loses some fails). The
  tar is written whole to the guest's `/var/tmp` first, so a copy out needs
  that much free space on the root disk, and sends nothing until tar is
  done: a large copy is silent for a while before it streams. Shutdown
  is Archil's stop; a reboot is refused.
- **Power is Archil's.** Start resumes a sandbox Archil paused at its 24-hour
  limit. Attach reports a paused one running and leaves it paused: the
  machine's first exec or copy resumes it (a stop stops it as it is), so a listing, which
  attaches to every VM, wakes none. A guest that powers itself off halts its VM, which
  Archil goes on reporting `running`: unsupported, as on boxd.
- **The URL** is the hostname Archil gives the exposed port 7302, the same
  across stop and start, and the private URL is the URL.
