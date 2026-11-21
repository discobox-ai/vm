# pkg/machine/boxd

A committed VM is a snapshot, and the guest is reached through Exec.

boxd offers machines and snapshots of running machines, not disk images, no
labels on either, and no socket into a guest. The boxd driver maps the
seam's VMs (ADR 26-11-03-606) onto what it has, with nothing of disco-vm's
in the guest and nothing on the host: what it knows is in the names of its
machines and snapshots (the root
[DESIGN.md](../../../DESIGN.md#the-engine-keeps-no-state-and-a-driver-keeps-it-in-its-provider)). See
[docs/drivers/boxd.md](../../../docs/drivers/boxd.md).

- **A VM is a machine, and a committed VM a snapshot, of one name**,
  `dvm-<pool>--<name>`. A name is the pool's across both: Create
  refuses one a machine or a ready snapshot holds. Commit starts the stopped
  machine, because boxd only snapshots a running one, snapshots it under its
  own name, and deletes it. The snapshot is still cut from an orderly
  shutdown: its disk is the one the guest flushed. A listing that finds a
  snapshot and a machine of one name is a Commit that has not deleted the
  machine yet: it lists the committed VM, and Destroy removes both. Destroy
  of a machine judges the snapshot of its name by what it was before the
  machine was destroyed: ready is a Commit that finished, the committed VM,
  and stays, even for a caller still holding the machine; pending or failed
  is deleted first, since boxd finishes a pending snapshot of a machine it
  destroys; and one that appears meanwhile, the machine's own Commit's, is
  deleted after. That Commit then fails, since a snapshot gone once seen was
  deleted. A failed snapshot with no machine of its name (one an earlier
  Destroy left) is listed as a VM, which is refused like a committed one
  (`ErrCommitted`) and which Destroy removes.
  That a VM is committed is the caller's to record (ADR 26-11-03-606); the driver
  tells one by its kind, the ready snapshot of its name whose ID is the
  VM's, and refuses to boot, start, serve, or commit it (`ErrCommitted`),
  or to restore from a machine. There is no install.
- **A fresh VM** is made with `CreateVm` from the driver's base
  (`machine.Config.Base`), or boxd's default image (`GetConfig`) when it has
  none; **a VM from a committed one** restores its snapshot by name
  (`CreateVmFromSnapshot`). Either is made at the VM's size and stopped, so
  its first boot is a cold boot of its disk, and a fresh one's first `run`
  is two boots. boxd does not say which snapshot a machine was restored
  from, so `Instance.From` is empty, and a restored machine outlives its
  snapshot's removal.
- **A NotFound for a machine just made is not a machine gone.** boxd has
  answered one for a machine `CreateVm` had just made, and does not say
  that every call sees a new machine at once, so each step of a create (the
  fence, the networks, the idle timers, the wait for it to run, the proxies,
  the power-off) is made again while it answers NotFound, until a minute
  after the create answered. A create that fails destroys its machine, and
  counts it gone only when `DestroyVm` succeeds, or answers NotFound while
  `ListVms` lists it destroying or, once a step has found it, not at all;
  otherwise it fails naming the machine as left, or as possibly left.
- **The guest's operations are Exec streams.** Each is one command line run
  as boxd's own user: `sudo -n -H -u USER -- env -C DIR -- K=V... ARGV...`,
  every word quoted, root unless the request names a user. Stdin, stdout,
  stderr, the exit code, and a TTY with its size and resizes are Exec's own.
  A copy in is `tar -x` reading the stream, and a copy out writes one ready
  byte once it has found the path and then `tar -c`, so a missing path is an
  error, not an empty tar. Info is `uname -m` and `hostname`. Boot and
  Start return once boxd says the machine is `running`, and wait for
  nothing in the guest. boxd holds an Exec sent while the guest
  boots until it answers; one boxd answers `Unavailable` before the guest
  has answered anything is `machine.ErrUnreachable`, which the engine's
  start tries again ([engine](../../engine/DESIGN.md)). Commit waits
  until the guest is settled, so its snapshot is of one, and so does Create
  before it powers a new machine off, so the disk is one its first boot
  finished: settled is `systemctl is-system-running`, asked every 200 ms and
  each time for up to 5 s, printing `running`, `degraded` or `maintenance`,
  waited for up to 5 minutes by Commit and 2 by Create.
  Nothing in the guest waits. Early in a boot, `is-system-running --wait`
  has hung until its timeout, and returned at once with `offline` or a bus
  error while systemd was still initializing; `systemctl start
  default.target` failed at once on the bus, or returned while systemd was
  still `starting` (sandbox image, nyc, 2026-11-16). A Shutdown's flush
  waits for nothing but the Exec. boxd authenticates every stream. An image
  installed before this carries an agent that nothing reaches; it does no
  harm.
- **The ingress URL is the machine's default proxy, unless it is asked
  off.** By default, and asked on, Create pins the default proxy at
  `<machine>.<zone>` (the domain `ListProxies` reports; the zone is the
  region's, `boxd.sh` in fra) to 7302 and turns off bot protection, which would put
  a browser challenge in front of a request that wakes the machine. Pinning
  it also stops the proxy forwarding to whichever common port the guest
  listens on (80, 443, 8080, 8000, 3000, 5000, 5173). Asked off, strictly or
  not, Create closes it: boxd never deletes a default proxy, so Create locks
  it to port 0, which no guest can listen on (boxd answers `502` with
  `x-boxd-proxy: placeholder`; once every
  proxy reads back closed, Create turns bot protection off so its challenge
  does not answer in its place), removes
  every other proxy (`DeleteProxy`), and reads back the default one alone and
  closed, or destroys the machine and fails; it has no ingress URL
  (`Capabilities.URLs`: ingress when asked, pool always; ADR 26-11-12-604,
  ADR 26-11-14-610, ADR 26-11-14-454). A strict pool URL on no networks with
  ingress off is refused: peers reach such a machine at nothing. An
  isolated machine has no in-VM CLI to open or add a proxy again. The pool URL of a
  machine on networks is `http://<machine>.boxd:7302`, its name on boxd's
  internal DNS, where its peers reach every port directly; on none it is the
  ingress URL, if it has one (ADR 26-10-29-129).
- **A machine's power is boxd's** (ADR 26-10-15-833). Its state is `GetVm`'s
  status, polled every 200 ms while a boot or stop settles. A start asks
  `StartVm` before it reads anything, since the expected machine is stopped
  and boxd answers `StartVm` OK for one starting, running or suspended (which
  it resumes); the wait then boots whatever else it finds, resizes once a machine
  it started at another size than its instance asks (a second boot), leaves
  one it found running at its size, and fails, rather than restarting it, on
  a machine that stops once it has come up. `Boot` reads `GetVm` first, and
  refuses a machine suspended, in standby, or hibernated (`ErrPaused`):
  `Start` alone resumes one. A `GetVm` answered NotFound, for a machine just
  made, reads as stopped, and the boot goes on to `StartVm`. A VM
  is found by name (`GetSnapshot`, then `GetVm` when there is no ready
  snapshot), listing the region only when neither finds it, and a create leaves a taken
  machine name to boxd's refusal. An orderly shutdown runs `sync` in the guest, since boxd does not
  say whether `StopVm` cuts the power, then `StopVm`; a reboot is `RebootVm`,
  and Kill is `StopVm`. A guest that powers itself off from inside is not
  supported: boxd leaves it `running`, and so does disco-vm, until `disco-vm
  stop`. An image built before this has a shutdown hook that nothing reads.
- **A driver is one region's**: its configuration's `region`, a boxd region
  ID (ADR 26-11-11-215), which is required. boxd's own default, the org's
  default region, is not followed: it can change under a pool, leaving the
  pool's VMs unlisted and billed in the old one, and a credential may not be
  allowed to read it. A pool made before it was required is at `fra`.
  boxd gives each region its own API endpoint, so the region is where calls
  on machines and snapshots go: the endpoint `ListRegions` names for the
  region (the home endpoint, `boxd.sh:9443`, for the home region), asked
  there once it answers (a failure is asked again). A region boxd does not list,
  or one the key's org has not `enabled`, is an error naming why. Account and
  key calls (`Whoami`, the API keys, the key exchange) always go to the home
  endpoint. A region's endpoint lists that region's machines and snapshots
  alone, so a pool never sees another region's, and a name is the pool's in
  each region. The URL needs nothing: the proxy's domain is the region's.
- **The driver talks to boxd's public gRPC API directly**, not through the
  boxd CLI: it works from a server that embeds the engine, from any host OS,
  and with an API key rather than a browser login. The key is the one
  `machine.Config.Keys` answers, asked on every call; an API key (`bxd_`) is
  exchanged for a JWT, again whenever the key changes, and anything else is
  sent as a JWT.
- **With `inMachine` and no key, a boxd machine's own token is the
  credential** (discobox-ai/discobox#1090). The configuration's `inMachine`
  set and `machine.Config.Keys` nil, the driver asks boxd's in-machine
  metadata adapter, `GET http://<gateway>:9002/v1/auth/token` on the default
  gateway (`/proc/net/route`), which mints a JWT of the machine's owner,
  fenced to its org, that lives 15 minutes. It is a field of the
  configuration, not inferred from boxd's environment variables, which an
  embedder's process may not have and a driver does not read; and it is
  privileged, since it acts as the host machine's owner. The definition
  declares both, field and adapter (`Definition.InMachine`), so an embedder
  can keep the machine's other processes from the adapter. Unset, no key is
  `errNoCredentials` as before, and the gateway is sent nothing. The
  token is kept until a minute before its `expires_at` and dropped when boxd
  answers `Unauthenticated`. The adapter is dialed through
  `machine.Config.Dial` and never a proxy: it knows the machine by where the
  request comes from. Its `api_url` is not read; the configuration's `addr`
  is where calls go. An isolated machine has no adapter, and one that does
  not answer is `errNoCredentials`. On a private machine the token reaches
  what the owner's `member` key does in the org: every machine of the
  owner's, other pools' included, and the org's env values, but no key
  management (`ErrCannotIssue`). A shared machine's token is `org`-scoped,
  which has no `Whoami`, snapshots or egress fence, so a pool cannot run on
  one. A configured key is used as before, and the adapter never asked.
- **An issued key is a `member` API key** named `dvm-<pool>--<name>`,
  fenced to the engine's key's org (boxd resolves an empty org to it), with
  boxd's expiry. It reaches all its maker's machines in that org: scope
  `org`. boxd refuses key management to every API key, so an issued key
  cannot issue, and the driver issues only from a `boxd login` session; from
  an API key it is `ErrCannotIssue`. `IssuedKeys` and `RevokeKey` read
  `ListApiKeys`, keep only the caller's `member` keys named for the pool,
  and `RevokeKey` deletes nothing else (ADR 26-10-24-562).
- **A bare machine is boxd's `isolated`** (`Capabilities.Bare`: strip).
  `Bare` is `CreateVm`'s and
  `CreateVmFromSnapshot`'s `isolated`, which a restore inherits from its
  snapshot; `Networks` is `CreateVm`'s `networks`, or `SetVmNetworks` after a
  restore, which takes none. A restore given networks must be isolated, so
  it has no peers until they are set rather than the default network's.
- **The egress fence is boxd's allowlist** (ADR 26-10-29-129). Neither
  create takes one, so `SetVmEgressAllow` is the first call once `CreateVm`
  or `CreateVmFromSnapshot` answers, with `["none"]` for an empty `Allow`
  (which still admits the hosts of the host-bound secrets the machine holds,
  as every boxd allowlist does),
  and `GetVm`'s `egress_allow` is read back; a machine that does not report
  it as asked is destroyed, and Create fails. A nil `Egress` sets nothing: a
  machine starts unrestricted, and so does a restore of a snapshot disco-vm
  made, since only a snapshot of a boxd machine spec carries an allowlist.
  `Instance.Egress` is `egress_allow`, nil for none.
- **`Check` reads the org's machine limit and refuses host-bound secrets**
  (ADR 26-11-15-274). The limit (`GetOrgMachineLimit`'s `max_vms`) counts
  every machine billed to the org, stopped and every member's, and boxd
  refuses a create past it, so it is `MaxMachines[linux]`, kept from the
  last `Check` and none before; `MaxRunning` is unset. A create boxd refuses
  reads the limit again, and is `ErrCapacity` when the org is at it, since
  boxd documents no code for it. boxd lets a machine reach a host-bound
  secret's hosts past its egress allowlist, so `Check` lists the
  secrets with no org (`ListVars`), resolved as a create's no org is, to the
  org the credential is fenced to, and fails, naming each, its scope, and
  its hosts, while one reaches the driver's machines: scope `all`,
  `private`, or none. A credential that may not read the limit (a
  machine's token) has no cap, and passes.
- **The key's errors are the caller's.** The bearer is added by client
  interceptors, not gRPC per-RPC credentials, whose errors gRPC flattens: a
  missing key, with no adapter to ask, is `errNoCredentials`, and a key the exchange or a call
  refuses (`Unauthenticated`, at a call's start or on a stream's message) is
  `machine.ErrUnauthorized`, and drops the cached JWT so the next call
  exchanges the key, or mints an in-machine token, again. `--debug` prints a new key's answer without its
  secret. An issued key outliving its TTL is taken back (`ErrCannotIssue`,
  naming it if the delete fails), and a name is issued once while a key of
  it lives (`machine.ErrExists`).
