# pkg/machine/archil

A VM is the live sandbox its labels name, and nothing else is kept (ADR
26-11-24-905, ADR 26-11-03-606). See
[docs/drivers/archil.md](../../../docs/drivers/archil.md).

Archil's sandboxes are Firecracker microVMs behind a REST API, beside disks
the provider mounts in them. A sandbox's name and labels never change, and
its labels are all a VM needs. The driver has no agent in the guest and
nothing on the host, and the guest runs systemd (ADR 26-11-20-447):

- **A driver is one region's**, its configuration's `region`, which is
  required, as boxd's is: each region is its own control plane, with its
  own sandboxes and disks, so a pool sees only its region's VMs, and a
  default that changed would lose sight of VMs still billed in the old one.
  `aws-us-east-1` is where a pool made before it was required keeps its
  VMs, and the refusal says so.
- **A VM is the live sandbox labeled with its pool and its name**
  (`ai.discobox.vm.pool`, `ai.discobox.vm.instance`; `names.go`), found by
  Archil's label filters, which AND: a pool's with one, a VM's with both. A
  sandbox's name is never read for what it is, and one with neither label is
  no VM. Each sandbox also carries `ai.discobox.vm.from` for a copy, which
  is how a listing reports `From`, since Archil keeps nothing of it. Archil
  sets labels only when it makes a sandbox, and a fork given none copies its
  source's, so a fork is always given its own. The guest sees none of them
  (seen 2026-11-21: not in its environment, its kernel command line, or its
  disk). The ingress mark stays in the network policy.
- **One sandbox is each VM** (`vms`, ADR 26-11-24-905 §3): an upgrade's new
  sandbox is labeled `ai.discobox.vm.replaces` with the old one's ID, and is
  not the VM while that one lives; of the rest, the oldest is, then the
  lowest ID. A create names its sandbox `dvm-<pool>--<name>`, so Archil
  refuses most races (`sandbox_name_conflict`, `machine.ErrExists`), and one
  that finds an older sandbox of the VM once its own is made removes its own
  and is `ErrExists`. A sandbox's name is at most 63 characters, so a pool
  and a name are at most 57 together.
- **The data volume is the disk `dvm-<pool>--<name>--data`**, mounted at
  each start; disks take no labels. A create makes it only once no sandbox
  but its own is the VM, so a disk Archil answers 200 for, not 201, is left
  from a VM removed midway, and is deleted and made again: no VM sees
  another's data. A fresh VM asked to start has it made first and mounted
  by its sandbox's create, where Archil made it for this create (201); one
  already there may be a racing create's, so it is left, and the VM is made
  as a copy is: a copy asked to start with a data volume is stopped and
  started once with it mounted, since Archil's fork takes no mounts (ADR
  26-11-23-458 §2, ADR 26-11-24-905 §6). Destroy deletes it after the
  sandboxes, so a destroy refused for forks keeps it.
- **Upgrade** (ADR 26-11-03-782, ADR 26-11-24-905 §4) makes the new
  sandbox first, fresh or a fork, at the VM's size, fence, and ingress,
  named `dvm-<pool>--<name, cut to fit>--<hex>` and labeled as replacing the
  old, with systemd settled, and stopped; only then revokes the old one's
  tokens and deletes it. A failure before that removes the new one and
  leaves the VM as it was. One stopped in between leaves a new sandbox
  nothing lists, which the next Upgrade or Destroy removes, after any a
  create of the name left. An Instance from before an upgrade still names
  the VM, which is found by its labels.
- **A fresh VM** is a sandbox of `Config.Base`, an OCI image, or of
  Archil's default image. **A VM created from another** is a fork of its
  sandbox, which must be stopped and have no mounts, since a fork boots with
  its source's. A fork keeps its source's size: another is refused. Either
  way Create sets the spec's fence and ingress mark and reads them back,
  exposes no port, and stops the sandbox, or, asked to, leaves it running;
  a fork has every token it carried revoked first.
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
  systemd's process runs, so a guest operation can enter its namespaces, and
  not once its boot is done: as on any machine, systemd may still be booting
  when the guest is reached, and a caller that needs a unit waits for that
  unit. Create then stops the sandbox with systemd's first boot perhaps
  not done, as any stop may find it: systemd commits the machine ID only
  once a first boot completes, so the next boot, of the VM or of a copy, is
  a first boot again and does what was left. Only Create registers, where
  `services get` says there is none (exit 1; any other answer fails, rather
  than risk a second systemd): Start installs and registers nothing, and
  leaves a VM whose systemd is not launched within a minute stopped. No VM from before
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
- **An upgrade sees its new root once it boots,** before the old sandbox
  is deleted: a fresh one of a base without systemd, fenced from its
  mirror, fails there, as a base that does not pull does, and leaves the VM
  as it was.
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
  done: a large copy is silent for a while before it streams. A reboot
  is refused.
- **A stop is a kill, a flush, and Archil's stop, and no shutdown**
  (vm#217): a discobox has no shutdown to run. Archil's stop sends each
  service of its supervisor TERM and kills it only after a grace (about
  30 s); `unshare` ignores TERM while it waits, and systemd as PID 1 does
  not exit at it, so every stop of a guest running systemd waited out the
  grace. So `systemdScript` ends at TERM by killing `unshare`, and systemd
  and all under it with it, which makes a stop Archil makes alone (its
  console's) prompt too, for a VM registered since. Before every stop it
  asks of a running sandbox, the driver runs `haltScript` in Archil's
  namespaces: SIGKILL to systemd, so nothing in the guest writes after the
  flush, then `sync`, and `archil sync` of the data volume where it is
  mounted (Archil's client sends writes about once a second and loses those
  unsent when the machine stops). A flush that fails fails `Shutdown` and
  asks no stop, and the engine's fallback is `Kill`, which flushes no data
  volume; the driver's own stops (Create's, Commit's, Destroy's, a start's
  that did not settle) flush it where it is mounted and stop the sandbox
  whether or not the guest answered, within `haltLimit`. A VM registered
  before this keeps its service's old argv, since only Create registers:
  the kill makes the driver's stops of it prompt, and a stop Archil makes
  alone still waits out the grace.
- **Power is Archil's.** Start resumes a sandbox Archil paused at its 24-hour
  limit, or that `Pause` paused: Archil's caller pause (`POST .../pause`),
  which snapshots its memory and processes and is waited out to `paused`
  (`Capabilities.Pause`), reported since that pause was seen on Archil
  itself (2026-11-21: `machinetest`'s pause-and-resume, and by hand): a
  process Archil restores keeps its PID, and the guest's clock is current at
  resume. Archil's docs count `max_ttl_seconds` against a powered-on session
  only; whether Archil stops a sandbox left paused, and after how long, is
  neither documented nor seen. A process a guest
  operation starts in the background is killed with the operation's process
  group, unless it leaves the group (`setsid`). A request to 7302 carrying a port token starts a stopped sandbox or
  resumes a paused one (seen 2026-11-16 of an exposed port: a stopped VM ran
  2.8 s after a `GET` of its URL), so a VM stopped by the engine can be
  running at its next listing; one without a token is refused and wakes
  nothing. State reads a paused (or pausing) sandbox as paused, and wakes
  nothing, so a listing wakes none; Start resumes it, and nothing else
  does. Attach and Boot of a paused one
  are `machine.ErrPaused`, and leave it paused (discobox-ai/vm ADR
  26-11-21-438 §5); Destroy stops it as it is. A session limit of its
  own (`max_ttl_seconds`, `on_timeout`) is not set. A guest that powers itself off halts its VM, which
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
    again. The hostname is both URLs, the same across stop and start; the
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
    `ingressOff` (TEST-NET-1). It is read back with the fence, kept across
    an upgrade, set or cleared on a fork
    as the spec asks, and changes no traffic; an address, not a name,
    since any domain rule has Archil terminate the guest's TLS on 443.
  - **A fence closes DNS when asked** (`Egress.CloseDNS`) by denying
    8.8.8.8 and 1.1.1.1, the resolvers `resolv.conf` names, which an
    allow-list leaves open on 53. A list closes it only when asked, so an
    allow-list by name keeps resolving; `none` always closes it. A list
    reads back closed only where it denies both, and one that allows either
    is refused.
  - **Every machine is bare** (`BareAlways`): nothing of the account is in
    the guest. Nor is a data disk's mount token, as far as the guest's
    processes can see: Archil's client mounts the disk from outside the
    namespaces systemd and every guest operation run in, seeded with a
    handshake Archil's host side bakes into it, and root there finds no token
    in a file or the environment, none in `archil status`, and no token verb
    that prints one; a mount from the guest of its own disk, or of another
    VM's, is refused (vm#216). Those namespaces are the driver's PID and mount
    namespaces, not a user namespace, so this is not proof that root could
    not reach the client itself; whether a token it holds replays from
    outside Archil stays open, as ADR 26-11-17-311 §1 left it. Observed with
    Archil client version 1006 on `debian:sid`, 2026-11; another base, or an
    image from an older client, may differ.
