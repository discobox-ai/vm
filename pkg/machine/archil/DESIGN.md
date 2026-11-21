# pkg/machine/archil

A VM is the sandbox named for it, and nothing else is kept (ADR
26-11-03-676, ADR 26-11-03-606). See
[docs/drivers/archil.md](../../../docs/drivers/archil.md).

Archil's sandboxes are Firecracker microVMs behind a REST API, beside disks
the provider mounts in them. A sandbox's name and labels never change, and
the name is all a VM needs. The driver has no agent in the guest and
nothing on the host, and the guest runs systemd (ADR 26-11-20-447):

- **A driver is one region's**, its configuration's `region`, which is
  required, as boxd's is: each region is its own control plane, with its
  own sandboxes and disks, so a pool sees only its region's VMs, and a
  default that changed would lose sight of VMs still billed in the old one.
  `aws-us-east-1` is where a pool made before it was required keeps its
  VMs, and the refusal says so.
- **A VM is the sandbox `dvm-<pool>--<name>`** (`names.go`). Archil
  refuses a sandbox name it has, so a Create takes the name before it makes
  anything else, and the refusal is `machine.ErrExists`. A listing reads the
  sandboxes under the pool's prefix, and Archil holds each one's fence and
  data disk. A sandbox whose name is no VM's, such as an earlier driver's
  `dvm-<pool>-<key>`, is not listed. Archil filters every sandbox listing
  to the pool's prefix, or a VM's name (`filter`); the pool's disks are
  still read from the whole account's, or one by its name.
- **Every sandbox the driver makes is labeled** (`names.go`):
  `ai.discobox.vm.pool`, `ai.discobox.vm.instance`, and
  `ai.discobox.vm.from` for a copy, at a fresh Create, a fork, and an
  upgrade's new sandbox, so a pool's sandboxes are found in Archil with
  `label=ai.discobox.vm.pool=<pool>`, and a listing reports a copy's
  `From`, which Archil does not keep. Archil sets labels only when it makes
  a sandbox, and a fork given none copies its source's, so a fork is always
  given its own. Labels are not the VM: a sandbox made before them has none,
  can't be given any, and is listed by its name with no `From`. The guest
  sees none of them (seen 2026-11-21: not in its environment, its kernel
  command line, or its disk). The ingress mark stays in the network policy,
  which every VM carries, labeled or not.
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
  way Create sets the spec's fence and ingress mark and reads them back,
  exposes no port, and stops the sandbox; a fork has 7302 unexposed and
  every token it carried revoked first.
- **Commit** leaves the stopped sandbox as it stands, with no mounts, no
  data disk, and no port token: a sandbox with mounts is started once with none and stopped,
  since only a start changes mounts. Nothing records the commit, and Archil
  can't tell a committed VM from a stopped one, so Start does not refuse
  one: the caller never starts it.
- **Copies chain.** A fork pins its source, so Destroy of a VM with forks is
  `machine.ErrInUse`, as hcs's chain is.
- **Every guest runs systemd** (`systemd.go`), as PID 1 of a PID and mount
  namespace of its own below Archil's init, which runs nothing of the
  image's. While Create's sandbox runs, under its fence, `ensureScript`
  installs systemd with apt where the base has none, and registers it once
  as the service `disco-vm-systemd` of Archil's supervisor in the guest
  (`archil-client services`), which starts it at every boot, however the VM
  was started, and again when it exits. A base with no `apt-get`, or one
  fenced from its mirror, fails Create naming `/lib/systemd/systemd`. A fork
  carries the root and the service. Create and Start each return once
  `systemctl is-system-running` has settled. Only Create registers, where
  `services get` says there is none (exit 1; any other answer fails, rather
  than risk a second systemd): Start installs and registers nothing, and
  leaves a VM whose systemd does not settle stopped. No VM from before
  this exists to migrate. The guest's root is made shared and
  systemd's mounts its slaves, so a disk Archil mounts after systemd starts
  reaches its units.
- **Every guest operation enters systemd's namespaces** (`enterScript`):
  the line finds the process whose PID in its namespace is 1 and runs the
  request through `nsenter --all`, `cd` to its directory and `runuser` for
  another account. Archil's kill SIGKILLs the process's whole group, which
  the command stays in, so it ends in there too (seen 2026-11-21); should
  `nsenter` alone die, `setpriv --pdeathsig TERM` (set again under
  `runuser`, whose fork and change of account clear it) sends the command
  TERM. With systemd not running, it fails saying so (exit 125).
- **An upgrade sees its new root only once it boots,** after the old
  sandbox is gone: a fresh one of a base without systemd, fenced from its
  mirror, fails there, as a base that does not pull does, and leaves a VM
  with a data disk mid-upgrade, which an upgrade from a committed VM with
  systemd, or of a base with it, finishes.
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
  limit. A request to 7302 carrying a port token starts a stopped sandbox or
  resumes a paused one (seen 2026-11-16 of an exposed port: a stopped VM ran
  2.8 s after a `GET` of its URL), so a VM stopped by the engine can be
  running at its next listing; one without a token is refused and wakes
  nothing. Attach reports a paused one running and leaves it paused: the
  machine's first exec or copy resumes it (a stop stops it as it is), so a listing, which
  attaches to every VM, wakes none. A guest that powers itself off halts its VM, which
  Archil goes on reporting `running`: unsupported, as on boxd.
- **A 502, 503, or 504 is `machine.ErrUnavailable`,** from the REST API or
  the process API's WebSocket handshake: Archil, or its gateway, not
  serving for now. From the REST API, a 401, or a 403 with no code, is
  `machine.ErrUnauthorized`, and any other answer, a 500 or a 429 included,
  is Archil's own error. The handshake follows a REST call that asks for its
  URL with the same key, so a refused key fails there first.
- **The machine is fenced at Archil's edge** (ADR 26-11-17-311), where the
  guest's root reaches nothing (`edge.go`):
  - **No port is exposed**, and every sandbox is made with
    `enable_service_ingress: false`, which a fork keeps.
  - **Its service is reached only with a port token** at 7302's hostname,
    which Archil gives only with a minted token. `Service` mints one each
    call, for `tokenLife` (a day), and returns it as `Credential` with
    `X-Archil-Token`; the driver never keeps it, and Archil never returns it
    again. Every Start, Commit, Service, and Create from a committed VM
    (source and fork) unexposes 7302 first, so a VM made before this is
    fenced at the first of them it meets. The hostname is both URLs, the same across stop and start; the
    ingress URL is withheld where it was asked off.
  - **A token never outlives its machine:** Destroy, Commit, and Upgrade
    revoke the sandbox's tokens before they delete or freeze it, and a fork's
    are revoked at Create (and by a failed Create's undo), since Archil documents
    neither that a delete revokes them nor that a fork copies none (it was
    seen to do both, 2026-11-18). Archil
    shows a sandbox's forks only by refusing its delete, and lists no tokens
    of a sandbox deleted, so the revoke comes first (ADR 26-11-17-311 §4): a
    destroy or upgrade refused for forks leaves the VM as it was but for its
    tokens, which their holders renew with `Service`. Stop revokes nothing: a holder's
    request starts the VM, as Archil does.
  - **Ingress off is a mark in the network policy:** the denied address
    `ingressOff` (TEST-NET-1), which a VM made before labels carries too. It is read
    back with the fence, kept across an upgrade (one finished after an
    interruption, which left nothing of the sandbox, ends with ingress off,
    the strictest), set or cleared on a fork
    as the spec asks, and changes no traffic; an address, not a name,
    since any domain rule has Archil terminate the guest's TLS on 443.
  - **A fence closes DNS when asked** (`Egress.CloseDNS`) by denying
    8.8.8.8 and 1.1.1.1, the resolvers `resolv.conf` names, which an
    allow-list leaves open on 53. A list closes it only when asked, so an
    allow-list by name keeps resolving; `none` always closes it. A list
    reads back closed only where it denies both, and one that allows either
    is refused.
  - **Every machine is bare** (`BareAlways`): nothing of the account is in
    the guest. A data disk's mount token is the guest's own disk's.
