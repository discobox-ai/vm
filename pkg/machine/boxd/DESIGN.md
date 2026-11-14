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
  machine yet: it lists the committed VM, and Destroy removes both. That a
  VM is committed is the caller's to record (ADR 26-11-03-606); the driver
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
- **The guest's operations are Exec streams.** Each is one command line run
  as boxd's own user: `sudo -n -H -u USER -- env -C DIR -- K=V... ARGV...`,
  every word quoted, root unless the request names a user. Stdin, stdout,
  stderr, the exit code, and a TTY with its size and resizes are Exec's own.
  A copy in is `tar -x` reading the stream, and a copy out writes one ready
  byte once it has found the path and then `tar -c`, so a missing path is an
  error, not an empty tar. Info is `uname -m` and `hostname`. Boot and
  Commit wait until `systemctl is-system-running --wait` answers, so a
  command runs in a booted system. boxd authenticates every stream. An image
  installed before this carries an agent that nothing reaches; it does no
  harm.
- **The URL is the machine's default proxy, unless its public URL is asked
  off.** By default, and asked on, Create pins the default proxy at
  `<machine>.boxd.sh` to 7302 and turns off bot protection, which would put
  a browser challenge in front of a request that wakes the machine. Pinning
  it also stops the proxy forwarding to whichever common port the guest
  listens on (80, 443, 8080, 8000, 3000, 5000, 5173). Asked off, strictly or
  not, Create removes every proxy boxd gave the machine (`DeleteProxy`) and
  reads back that none is left, or destroys the machine and fails; it has no
  URL (`Capabilities.URLs`: public when asked, private always; ADR
  26-11-12-604). A strict private URL on no networks with the public one off
  is refused: peers reach such a machine at nothing. An isolated
  machine has no in-VM CLI to make a proxy again. The private URL of a
  machine on networks is `http://<machine>.boxd:7302`, its name on boxd's
  internal DNS, where its peers reach every port directly; on none it is the
  URL, if it has one (ADR 26-10-29-129).
- **A machine's power is boxd's** (ADR 26-10-15-833). Its state is `GetVm`'s
  status. An orderly shutdown runs `sync` in the guest, since boxd does not
  say whether `StopVm` cuts the power, then `StopVm`; a reboot is `RebootVm`,
  and Kill is `StopVm`. A guest that powers itself off from inside is not
  supported: boxd leaves it `running`, and so does disco-vm, until `disco-vm
  stop`. An image built before this has a shutdown hook that nothing reads.
- **A driver is one region's**: its configuration's `region`, a boxd region
  ID (ADR 26-11-11-215).
  boxd gives each region its own API endpoint, so the region is where calls
  on machines and snapshots go: the home endpoint (`boxd.sh:9443`) for none,
  and otherwise the endpoint `ListRegions` names for the region, asked there
  once it answers (a failure is asked again). A region boxd does not list,
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
- **Isolation is boxd's.** `Isolated` is `CreateVm`'s and
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
- **The key's errors are the caller's.** The bearer is added by client
  interceptors, not gRPC per-RPC credentials, whose errors gRPC flattens: a
  missing key, with no adapter to ask, is `errNoCredentials`, and a key the exchange or a call
  refuses (`Unauthenticated`, at a call's start or on a stream's message) is
  `machine.ErrUnauthorized`, and drops the cached JWT so the next call
  exchanges the key, or mints an in-machine token, again. `--debug` prints a new key's answer without its
  secret. An issued key outliving its TTL is taken back (`ErrCannotIssue`,
  naming it if the delete fails), and a name is issued once while a key of
  it lives (`machine.ErrExists`).
