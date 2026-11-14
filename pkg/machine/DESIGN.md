# pkg/machine

The driver seam: `machine.Driver` and the guest's operations,
`machine.Guest`. Each hypervisor is a package below it, selected by build tags
(`drivers`), and `machinetest` is the conformance suite every driver passes.
archil, boxd, docker, local, and served have their own design notes:
[archil/DESIGN.md](archil/DESIGN.md), [boxd/DESIGN.md](boxd/DESIGN.md), [docker/DESIGN.md](docker/DESIGN.md),
[local/DESIGN.md](local/DESIGN.md), and [served/DESIGN.md](served/DESIGN.md):
a driver an embedder serves over a stream, whose client reports the served
driver's capabilities narrowed to the embedder's allow-list, and so has no
column below.

## The driver contract is the intersection, and differences are reported

`machine.Driver` is one pool's (`machine.Config.Pool`) VMs, committed
or not (ADR 26-11-03-606): Create, fresh from the driver's base
(`Config.Base`) or a copy of a committed VM (`InstanceSpec.From`); Install,
a committed VM from media, with the release the media resolved to
(`Instance.Release`, which every VM created from it or upgraded to it
carries, so an embedder keys a base on what `latest` was: discobox ADR
26-10-09-106 §5); Commit, which freezes a stopped VM under its
name; Upgrade, which gives a stopped VM a new root and keeps its volumes
([below](#a-data-volume-is-its-vms-and-upgrade-gives-it-a-new-root)); Instance, Instances, and Destroy, as its provider keeps them ([the root DESIGN.md](../../DESIGN.md#the-engine-keeps-no-state-and-a-driver-keeps-it-in-its-provider)).
That a VM is committed is the caller's to record, since not every provider
can tell (Archil keeps a committed sandbox as a stopped one): a driver
refuses one only where its provider has no machine left to run
(`ErrCommitted`: docker's image, boxd's snapshot), or where a boot would
write to a disk another VM's differences over (`ErrInUse`: hcs). And a VM's
machine: Boot, in this process; Start, outliving it;
Attach, from any process; and Service
([the service URL](../engine/DESIGN.md#every-instance-has-one-service-at-one-http-url-and-a-private-one)).
A machine also dials the agent's port and the service's (`Machine.Dial`)
where the driver lists `Capabilities.Ports` ([below](#ports-not-guids)).
Boot and Start return once the guest answers. A running `Machine` is the
guest's operations (`machine.Guest`: Exec with stdio, a TTY and its resizes,
a user, environment, and directory; CopyTo and CopyFrom, as tar; Info;
Shutdown), and Kill and Done. And keys: IssueKey, IssuedKeys, and RevokeKey
([below](#a-driver-is-given-its-key-and-issues-keys-that-cannot-issue)).
hcs, vz, libkrun, and fake are `local.Hypervisor`s under
one driver that keeps their state and runs their shims
([local/DESIGN.md](local/DESIGN.md)). Anything only one hypervisor can do is
a `Capabilities` field, never a faked method:

| capability | hcs | vz | boxd | docker | libkrun | fake | archil |
|---|---|---|---|---|---|---|---|
| guest OS | windows | darwin, and linux (ADR 26-10-31-990) | linux | linux | linux (ADR 26-11-03-530) | host's | linux, amd64 |
| max running | none | **2 macOS guests** (framework limit); Linux guests do not count | none (the org's quota) | none | none | none | none (the plan's quota) |
| shared dirs | (Plan9, later) | virtiofs | no | no (needs an idmapped mount) | virtiofs | no | no |
| display | the guest's Remote Desktop in mstsc, or the agent's stream on 7301 in a window (ADR 26-11-02-274) | framework view in a native window, at boot only | no | no | no | no | no |
| view (`View`: a window from any process, on a running VM or an install) | yes (ADR 26-11-11-746) | no | no | no | no | no | no |
| ports (`Ports`: `Machine.Dial`, `Forwards` to a socket or a peer) | hvsocket, through the shim | vsock, through the shim | no | no | vsock, through the shim: the agent's and service's ports, and the forwards, mapped at boot; the host's half-close is not passed on | loopback, through the shim | no |
| agent (`disco-vm guest` in the guest) | yes, from Install | yes: handed it at every boot, macOS on a read-only drive its launch daemon copies it off (the configured `macosAgent`, the CLI's own executable), Linux in the initrd | **no**: boxd's Exec | yes, from Create of a base | yes: the running disco-vm (or `$DISCO_VM_AGENT`), handed it in the initrd at every boot | yes | **no**: Archil's process API |
| install (`Install`: the OSes made from media) | windows: a retail ISO | darwin: an IPSW, or `latest` | **none** | **none** | **none** | the host's | **none** |
| base (`Config.Base`: what a fresh VM starts from) | **none** | Linux: a guest image, `DefaultLinuxBase` by default | an image, boxd's default by default | an image; **none** unless configured | Linux: a guest image's libkrun kernel and root, `DefaultBase` by default | a name, `scratch` by default | an OCI image; Archil's default by default |
| commit (`Commit`: `Driver.Commit` and `InstanceSpec.From`) | yes | yes | yes | yes | yes | yes, unless made with `NoCommit` (a test option) | yes |
| a committed VM (`Commit`) | its record and disk, left in place | its record and bundle, left in place | a snapshot `dvm-<pool>--<name>`; the machine deleted | an image `disco-vm-vm:<pool>--<name>`; the container removed | its record and bundle, left in place | its record and tree, left in place | the stopped sandbox `dvm-<pool>--<name>`, its mounts and data disk dropped; nothing marks it committed |
| disks chain below the seam (`local.Hypervisor.Chains`) | yes: a copy is a differencing VHDX over the committed VM's | no | no | no | no: a reflink, else a sparse copy | no | yes: a copy forks the committed VM's sandbox, which Archil keeps while it has forks (`ErrInUse`) |
| keys (`Capabilities.Keys`) | none (`ErrNoKey`) | none (`ErrNoKey`) | **issue**, scope `org`, from a `boxd login` session only; `ErrCannotIssue` from an API key | none (`ErrNoKey`) | none (`ErrNoKey`) | issue, scope `pool` | none (`ErrCannotIssue`: an Archil token never expires) |
| isolation (`Isolated`, `Networks`) | no | no | yes: `isolated`, `networks` | no | no | no | no |
| egress (`Egress`: the fences held) | none | `none`: no network device | `none` and `allow`: the egress allowlist | none | `none`: no network device (else a NIC on passt) | none | `none` and `allow`: `network.egress`, default `deny` |
| private URL (`Service.PrivateURL`) | none | none | `http://<machine>.boxd:7302` on networks, else the URL | the container's address on its network | none | the URL (a shared loopback) | the URL |
| URLs (`URLs`: given never, when asked, or always) | none | none | private always on networks (direct); on none the public one, so none with that off; public when asked (the default proxy, its default) | private always; never public (a loopback port) | none | private always (a shared loopback); never public | private and public always (the exposed port) |
| volumes (`Volumes`: `Data`, `Cache`) | none yet | none yet | none: no disk but the root | `data` and `cache`: host directories under `/var/lib/discobox` | none yet | `data` and `cache`: directories in the guest's root | `data`: a disk, `dvm-<pool>--<name>--data` |
| upgrade (`Upgrade`; listed as `VolumeData`) | refused | refused | refused | a new container under the name, bind-mounting the same directories | refused | a new root beside the old, its volumes moved into it | the sandbox deleted and made again under its name, mounting the same data disk; one stopped between is listed by its disk, and one with no data disk is lost |

A driver enforces `MaxRunning` across every pool it keeps, and refuses a
boot past it with `ErrCapacity`; `local` does for hcs, vz, libkrun, and fake
([pkg/machine/local](local/DESIGN.md#a-boot-is-admitted-across-every-pool)).
The engine counts nothing. Every driver clones the same way: cold
([below](#every-copy-is-cold-and-a-vm-is-made-fast-by-what-is-in-it)).

## Commit is a capability, and a driver without it makes every VM fresh

`Capabilities.Commit` reports `Driver.Commit` and `InstanceSpec.From`
together, since neither is any use without the other (ADR 26-11-03-868). It
is reported, never probed: a pool that would provision a VM to commit it
reads it first, since finding out by trying costs a whole provisioning. A
driver without it keeps `Commit` on its interface and returns
`ErrUnsupported`, leaving the VM as it was, running or stopped, with its
volumes; it refuses `Create` with a `From`, and makes nothing, and `Upgrade`
with a `from`, upgrading only to a fresh root; and no VM it lists has a
`From`. `Destroy` is the same on every driver, and returns only
once the provider no longer has the VM. A driver that installs, or has no
base, makes VMs only by copying, so reports `Commit`; `machinetest` asserts
both. Every driver reports it today; fake's `NoCommit` is the one without,
so the refusals, and the suite's paths that skip a commit, run on every OS.

## A driver's configuration is its own

What differs between providers is not abstracted (ADR 26-11-11-215): each
driver takes `Config.DriverConfig`, a JSON object of fields no other driver
shares. `machine.Register` takes its factory, its `Definition` (a
description, its base, a `ConfigField` per field, in the shape of
discobox's `sandbox.ProviderDefinition`, plus `Default`, `Env`,
`Privileged`, and `Retired`, and its secret), and its `Validator`, all
required. `Config.Base` stays common, described per driver by
`Definition.Base`, nil where the driver takes none (hcs; ADR 26-11-12-461).

- **A driver decodes its own type** (`machine.DecodeConfig[T]`, refusing a
  key T lacks) in one `decode` that applies its defaults and checks its
  values for its factory and validator alike; an error names a field.
- **`New` normalizes before it constructs** (`machine.Normalize`): a key the
  definition does not list, a missing required field, a retired key beside
  its replacement are refused; a retired key is decoded into its
  replacement, or dropped, for good; then the driver's validator.
- **A driver reads no environment for its configuration**; the command line
  does (`ConfigFromEnv`, `ConfigFromValues`), as for a key. What a boot
  reads (an agent) its hypervisor holds, and a local driver's `Start`
  writes the configuration owner-only to the VM's directory; the shim reads
  it there, and one whose driver has another is refused.
- **A configuration is trusted as the key is.** A field that names a host
  resource, or where the key or a guest's data goes, is `Privileged`
  (`TestPrivileged` holds the list); a URL the key goes to is `https`, or
  `http` to loopback (`CheckKeyURL`); an agent is a regular file at an
  absolute path, checked before it is read (`CheckFile`).

## A driver is given its key, and issues keys that cannot issue

A driver calls its provider with the key `Config.Keys` answers, asked on
every call, so one process runs engines with different keys and a key
replaced where its source reads it is used from the next call (ADR
26-10-24-562). A driver given no source has no key and never reads the
environment for one: the command line passes `machine.KeyFromEnv`, which
reads the variables its secret names (`SecretField.Env`: archil's
`ARCHIL_API_KEY`, boxd's `BOXD_API_KEY`, `BOXD_TOKEN`; fake's
`DISCO_VM_FAKE_KEY`). A `Key`'s `Secret` is opaque: it prints, formats, and
logs without it.

A driver declares its secret in its definition (`Definition.Secrets`,
type `secret`; ADR 26-11-12-461), at most one, since `Config.Keys` carries
one: its variables, and every place it is sent (`SecretSend`: the
privileged field naming the host, or the hosts; the protocol, method, and
path; the header or JSON body field), one send per form it takes (boxd's
`bxd_` API key goes only to the token exchange's body, a token is every
gRPC call's bearer). `machine.SecretSends` resolves them for a
configuration, for a proxy that swaps a stand-in for the real key. A secret
is written, never read back: it is no field of a configuration, which
refuses its key by name, and no definition, `info`, help, error, or file
carries its value. Each driver's test drives its real client and checks
the key arrived where `SecretSends` says and nowhere else.

`Config.Dial` dials every connection a driver makes to its service (boxd's
gRPC and token exchange, archil's API and process API, a Docker daemon, the
registry vz and libkrun pull a base or runtime from, the host vz downloads
macOS media from), at the service's own
address: set, a driver reads no proxy variable, so an embedder checks where
it connects as it connects, and proxies within its dialer if at all. Nil is
the process's own way, proxy variables and all. A driver takes it as
`Config.Dialer()` and `Config.HTTPTransport()`, and hands the transport to
`linuximage.Pull`. A shim is never given it, so libkrun's boot re-pulling a
runtime removed from the cache dials the process's own way. Which macOS
media is the latest (install's `latest`) is Virtualization.framework's own
lookup, out of any dialer's reach; vz downloads what it names itself, through
the transport.

`IssueKey` makes the narrowest key the driver can for another host to run the
pool's machines. An issued key expires (a TTL is required) and cannot
itself issue keys; a driver that cannot promise both returns `ErrNoKey` (it
has no credential to hand on) or `ErrCannotIssue` (its provider cannot make
one), never the caller's own key. Issued keys are named for the pool, so
`IssuedKeys` finds them by name, and `RevokeKey` refuses one the driver did
not issue for the pool; `disco-vm key revoke` refuses an ID it does not
list, so a typo never reads as revoked. A provider's refusal of a key is
`ErrUnauthorized`.

`Isolated` and `Networks` at create strip what a provider gives a machine by
default and place it on the caller's networks; a driver without
`Capabilities.Isolation` refuses them.

`Egress` at create fences what an instance reaches beyond its networks, where
its root cannot loosen it (ADR 26-10-29-129). It is in place before Create
returns and read back from the provider, so an instance the provider does not
report fenced as asked is destroyed and Create fails; it holds across stop
and start, and is the spec's, never a committed VM's. An empty `Allow` is
nothing beyond the networks; nil is the provider's default.
`Capabilities.Egress` lists the fences a driver holds (`Capabilities.Fences`):
`none`, an empty `Allow`, and `allow`, a list (ADR 26-10-31-990). A driver
refuses a fence it does not hold. vz holds `none` alone, as an instance with
no network device, whose only reach is the vsock its spec forwards; its
read-back is the instance's record, which Boot reads to attach no NIC.
`Instance.Egress` is the fence the provider reports.

`URLs` at create asks for each of the service's URLs, internal, private and
public, on or off, strictly or not; unset is the driver's default (ADR
26-11-12-604; the scopes are ADR 26-11-04-412 §4's). `Capabilities.URLs` is
each scope's reach (`URLNever`, `URLOptional`, `URLAlways`). A driver honors
a request where it can; a strict one it cannot meet, a URL it never gives
asked on or one it always gives asked off, it refuses (`URLRequests.Check`, in
every driver's Create and the engine's). A reach that depends on the rest of
the spec is reported as the common case, and the driver's Create refuses what
it cannot meet (boxd: a strict private URL on no networks, the public one
off). Internal has no field on `Service`
yet, so no driver gives one. A URL on the engine's host alone, a loopback
port, is not public. The provider reports no request on `Instance`:
`Service` says which URLs an instance has.

## Two volumes, at fixed paths, and a cache is saved only on request

An instance can have a data volume and a cache volume (`InstanceSpec.Data`,
`Cache`; ADR 26-11-02-180), and nothing else beside its root disk. Every
driver mounts each at `machine.VolumePath`: `/.discobox/data` and
`/.discobox/cache`, and `C:\.discobox\...` on Windows, before Boot or Start
returns. A driver lists what it attaches in `Capabilities.Volumes` and
refuses the rest with `ErrUnsupported`; nothing falls back to a directory on
the root disk. A size is a cap where the provider sizes storage and a hint
where it grows on demand (docker, fake).

- **A data volume is the instance's**: made empty at Create, kept across stop
  and start and across Upgrade, removed by Destroy, and never in a committed
  VM, since Commit keeps the root disk alone. It is never listed, named,
  attached, or removed apart from its VM (ADR 26-11-03-782).
- **A cache key points at a generation**, an immutable snapshot of a cache
  volume; each pool has its own keys. Create
  clones the key's current generation into the instance's own volume (empty
  where the key has none), which no other machine mounts and nothing
  refreshes; `Instance.Cache.Generation` names it.
- **Only `SaveCache` publishes**, from a stopped instance, to the key it
  names, keeping the instance. A guarded save (`CacheSave.Guarded`) refuses
  with `ErrCacheMoved` once the key holds another generation than the one it
  expects, checked before it copies and again before it moves the key; two
  saves in the same moment may both pass, and one of them wins (local: the
  last to move the key); docker holds a lock on the key on the daemon's host
  from the second check to the move, so a guarded save it passes is never
  lost. The engine
  guards a save to the key the instance started from, by the generation it
  was cloned from, unless forced (`SaveOptions.Force`, `cache save --force`).
- **A key moved off a generation removes it**, once nothing needs it;
  `DeleteCache` drops the key and its generation, and instances keep their
  clones.

### A data volume is its VM's, and Upgrade gives it a new root

A data volume's only purpose is to carry one VM's state onto a new root
(ADR 26-11-03-782). `Upgrade(inst, from)` gives a stopped VM a new root
disk, a copy of the committed VM `from` or a fresh one from the base (`from`
nil), and removes the old root for good. The VM keeps its name, its data
volume and its cache clone as they are (the clone is not cloned again from
its key), and what its spec set; its `ID`, `From`, and `Created` may change,
and an `Instance` from before still names it. It refuses a running VM, a
`from` of another guest OS, gone (`ErrNotFound`) or running, a fresh root
with no base, a `from` where `Capabilities.Commit` is false (ADR
26-11-03-868: such a driver upgrades only to a fresh root), and any upgrade
where `Capabilities.Volumes` lacks `VolumeData` (`ErrUnsupported`): a data
volume and its upgrade are one capability. On a driver that lists
`VolumeData`, a VM with no data volume upgrades too.

An upgrade interrupted at any step leaves the VM listed under its name with
its volumes, refused by Boot, Start, and Commit until `Upgrade` again with
the same `from` finishes it or `Destroy` removes it; no volume is ever left
that no listed VM owns. Finishing reads that committed VM only if the new
root is not made yet, so one removed in between does not strand the VM
(the engine passes a `from` it does not find to the driver, to say).

A driver lists every guest OS it boots in `Capabilities.GuestOS`, and the
ones it installs from media in `Capabilities.Install`; a caller asks for a
guest OS by creating from a committed VM of it, or fresh from a base of it
(ADR 26-10-31-990). vz boots macOS, installed, and Linux, from its base.

## Ports, not GUIDs

Where there is an agent, it listens on **port 7300**. On vz that is AF_VSOCK.
On hcs it is the hvsocket service ID `00001C84-FACB-11E6-BD58-64006A7986D3`,
which is the vsock template GUID with the port in its first field
(`winio.VsockServiceID`). The hcs driver must list that ID in the VM's HvSocket
ServiceTable. On docker it listens on a root-only unix socket, which
`disco-vm pipe` reaches through docker exec. boxd has no socket from the host
into the guest and no agent: its guest operations are command lines over
boxd's authenticated Exec stream ([boxd/DESIGN.md](boxd/DESIGN.md)). 7301 is a Windows
guest's display stream (`guest.ServeDisplay`).

A driver that lists `Capabilities.Ports` (hcs, vz, libkrun, fake) reaches
guest ports in three directions, all through the process that holds the
machine (ADR 26-10-31-990, which brings back the raw guest port ADR 0001 §3
took off the seam). Every port is known at boot, since libkrun maps a guest
port only before its VM starts (ADR 26-11-03-247):

- **Caller to guest:** `Machine.Dial(port)` of the agent's port (7300) or the
  service's (7302) alone (`guest.DialablePort`), through the shim's `/dial`
  for an attached machine, which dials the service where the hypervisor
  serves it (fake's loopback port); `Engine.Dial` and
  `disco-vm dial INSTANCE PORT`. Any other port is `ErrUnsupported` on every
  driver. The service stays one port at one URL (ADR 0005). boxd and docker
  return `ErrUnsupported`.
- **Guest to host:** a guest process connects out with `guest.DialHost(port)`
  (vsock to CID 2 on vz, hvsocket to the parent partition on HCS), and an
  instance created with a `Forward` to a `Socket` has its shim accept those
  connections (`machine.HostListener`) and splice each into the Unix socket,
  passing half-closes through (libkrun passes the guest's, and keeps the
  host's: it resets a stream the host half-closes). A hypervisor that maps
  ports at boot (libkrun) is given the forwards in `local.InstanceSpec`.
  `disco-vm dial-host PORT` does the same from a shell in the guest.
- **Guest to peer:** a `Forward` to an `Instance` and `InstancePort`, the
  peer's agent or service port (any other is refused at Create), has the
  shim splice each connection into that instance's guest port, through the
  peer's shim, found by name at every connection. It is the spec's, fixed at
  Create and reported in `Instance.Forwards`. While the peer is stopped each
  connection is closed at once (the shim has already accepted it, so the
  guest connects and reads EOF), and a peer that starts again is followed;
  the peer sees the connection come from the host. A NIC-less guest's way
  to its peer outlives the caller's process, as the shims do. The peer may
  be another pool's ([below](#a-vm-reaches-another-pool-only-as-its-embedder-says)). A relay that
  changes after Create, or carries a policy per connection, is the caller's,
  built from `Dial` and a `Socket` forward. On fake every guest port but the
  agent's is the host's loopback, so a peer forward there proves the plumbing
  through the agent's port.

## A VM reaches another pool only as its embedder says

A pool's VMs are its own (`Instance.Pool`), and a driver lists and names
only its pool's. A driver that keeps every pool's VMs where each pool's
driver reaches them, one root, lists `Capabilities.AcrossPools` (hcs, vz,
libkrun, fake), and two things there may name a VM of another pool
(discobox's ADR 26-11-11-963 §§5, 9): `InstanceSpec.From`, a committed VM
another pool's driver found (its `Pool`), and a peer `Forward`'s `Pool`. A
VM made so reports `Instance.FromPool`, and its forwards as given; an
upgrade's `from` may be another pool's the same way. Neither is a pool's
own request: the engine takes both from its embedder alone
([pkg/engine](../engine/DESIGN.md#a-pool-scopes-everything-and-is-in-every-name)),
and a local driver called with a key it issued, one pool's, refuses both
with `ErrUnauthorized`. boxd, docker, and archil make each pool's names
apart in their provider and refuse both with `ErrUnsupported`; boxd peers
a sandbox on its pool's network instead. On hcs, whose disks chain, a VM
of any pool created from a committed VM keeps it from removal
(`ErrInUse`), as one of its own pool does.

## A window shows a guest's desktop, and closing it leaves the VM running

A window is never the VM's owner: it closes when the VM stops, and closing it
leaves the VM running. Two kinds open one:

- **A boot.** `run --gui`, `start --gui`, and `install --gui` ask for a window
  on that boot: the engine passes `BootOptions.GUI` (and `InstallSpec.GUI`
  for an install's boots), and the driver opens it in the process that
  called `Boot`, which owns the VM: the shim (`shim --gui`) for `run` and
  `start`, the CLI for an install. A driver without `Capabilities.Display`
  refuses GUI with `ErrUnsupported` rather than ignoring it.
- **Any process, later** (`Capabilities.View`, ADR 26-11-11-746).
  `disco-vm gui INSTANCE` (`Engine.GUI`, `Driver.View`) opens a window on a
  running instance, and `disco-vm gui --install [NAME]`
  (`Engine.GUIInstall`, `Driver.ViewInstall`) one on an install another
  process runs, named by the VM it makes, or the only one. Every driver has
  both methods; one without `View` returns `ErrUnsupported`, and the engine
  refuses before calling it. hcs reports it; vz's window needs the VM object
  the shim holds, so it opens only at boot.

The local driver gives `View` through the shim to the hypervisor's
`Hypervisor.View`, with a function that opens the guest's display stream:
the shim's `/display`, a route of its own that takes no port, since an
attached machine's `Dial` reaches the agent's and the service's ports alone.
It marks an install's VM directory with `installing.json` (its record and the
installing process) while it runs, skips one whose process is gone, and
hands its machine directory to `Hypervisor.ViewInstall`, which finds the
install's machine itself: hcs by a record its install writes beside the
disk.

A native window on macOS must run on the process's main thread, which only
`main` can give away. So `main` locks its goroutine to the main thread and runs
the program through `machine.Main`. A driver that shows windows installs a
`machine.MainLoop` there; vz brings AppKit up on the main thread only when the
first window is asked for, so no other command (and not the guest agent) ever
touches it.

## Every copy is cold, and a VM is made fast by what is in it

Create makes one kind of copy (ADR 26-10-15-851): a copy-on-write copy of
the committed VM's disk, with a machine identity of its own, which Boot
starts from power-off. There are no clone modes, no staged VMs, and no
`warm` command. Work that every VM needs goes into a committed VM, by
shaping a guest and committing it, so a VM created from it starts with it
done.

What warming left behind is upgraded, not used:

- **`warm/` under a root** is no longer read, and nothing stops a warm shim
  any more. On hcs, first `stop` the running fork clones of the template: a
  running clone may live on the template's memory, while a stopped one boots
  from its own disk. Then end the `disco-vm
  shim --warm <layer>` process (Task Manager, `Stop-Process`), which ends the
  template system with its last handle, and delete `warm/` by hand.
- **A boxd stage's own snapshot** is no longer anything disco-vm lists. Delete
  it with `boxd snap rm`; a machine resumed from it outlives it.
- **An instance cloned fast** that has booted once is a cold instance on its
  own disk: a vz resume clone gets an identity of its own at its next boot,
  and an hcs fork clone's disk differences over the template disk in its own
  directory, which is why hcs still refuses to commit one. An instance
  prepared fast and never booted (vz's `resume.pending`, an hcs fork ticket)
  is refused at start: remove it and create it again.

Fast clones may come back once every driver's snapshots are proven
(the ADR's Deferred section), with a design of their own.

## Sentinels on the wire

`ErrorCode` and `ErrorSentinel` own the stable code mapping for machine
sentinels. Both served's JSON errors and guest's error response headers use
it, so `errors.Is` survives either transport. Transport-specific errors stay
with their transport; an unknown code has no machine sentinel.
