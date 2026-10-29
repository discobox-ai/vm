# pkg/machine

The driver seam: `machine.Driver` and the guest's operations,
`machine.Guest`. Each hypervisor is a package below it, selected by build tags
(`drivers`), and `machinetest` is the conformance suite every driver passes.
boxd, docker, and local have their own design notes:
[boxd/DESIGN.md](boxd/DESIGN.md), [docker/DESIGN.md](docker/DESIGN.md), and
[local/DESIGN.md](local/DESIGN.md).

## The driver contract is the intersection, and differences are reported

`machine.Driver` is one project's (`machine.Config.Project`) images (Install,
Image, Images, DeleteImage, Commit; each stands alone, ADR 26-10-23-410),
tags (Tags, Tag, Untag), bases (Bases, Base), and
instances (Create, a clone; Instance, Instances, Destroy), as its provider
keeps them ([the root DESIGN.md](../../DESIGN.md#the-engine-keeps-no-state-and-a-driver-keeps-it-in-its-provider));
and an instance's machine: Boot, in this process; Start, outliving it;
Attach, from any process; and Service
([the service URL](../engine/DESIGN.md#every-instance-has-one-service-at-one-http-url-and-a-private-one)).
Boot and Start return once the guest answers. A running `Machine` is the
guest's operations (`machine.Guest`: Exec with stdio, a TTY and its resizes,
a user, environment, and directory; CopyTo and CopyFrom, as tar; Info;
Shutdown), and Kill and Done. And keys: IssueKey, IssuedKeys, and RevokeKey
([below](#a-driver-is-given-its-key-and-issues-keys-that-cannot-issue)).
hcs, vz, and fake are `local.Hypervisor`s under
one driver that keeps their state and runs their shims
([local/DESIGN.md](local/DESIGN.md)). Anything only one hypervisor can do is
a `Capabilities` field, never a faked method:

| capability | hcs | vz | boxd | docker | fake |
|---|---|---|---|---|---|
| guest OS | windows | darwin | linux | linux | host's |
| max running | none | **2 macOS guests** (framework limit) | none (the org's quota) | none | none |
| shared dirs | (Plan9, later) | virtiofs | no | no (needs an idmapped mount) | no |
| display | video console in mstsc (guestfb over hvsocket, later) | framework view in a native window | no | no | no |
| forward (guest to host) | hvsocket to the parent partition | vsock to the host, CID 2 | no | no | loopback |
| agent (`disco-vm guest` in the guest) | yes, from Install | yes, from Install | **no**: boxd's Exec | yes, from Create of a base | yes |
| install (an image from media) | yes: a retail ISO | yes: an IPSW, or `latest` | **no**: boxd's images and snapshots are bases | **no**: registry images are bases | yes |
| disks chain below the seam (`local.Hypervisor.Chains`) | yes: a snapshot is a differencing VHDX | no | no | no | no |
| keys (`Capabilities.Keys`) | none (`ErrNoKey`) | none (`ErrNoKey`) | **issue**, scope `org`, from a `boxd login` session only; `ErrCannotIssue` from an API key | none (`ErrNoKey`) | issue, scope `project` |
| isolation (`Isolated`, `Networks`) | no | no | yes: `isolated`, `networks` | no | no |
| egress (`Egress`) | no | no | yes: the egress allowlist | no | no |
| private URL (`Service.PrivateURL`) | none | none | `http://<machine>.boxd:7302` on networks, else the URL | the container's address on its network | the URL (a shared loopback) |

The engine enforces `MaxRunning`. Every driver clones the same way: cold
([below](#every-clone-is-cold-and-an-image-is-made-fast-by-what-is-in-it)).

## A driver is given its key, and issues keys that cannot issue

A driver calls its provider with the key `Config.Keys` answers, asked on
every call, so one process runs engines with different keys and a key
replaced where its source reads it is used from the next call (ADR
26-10-24-562). A driver given no source has no key and never reads the
environment for one: the command line passes `machine.KeyFromEnv`, which
reads the variables a driver names at `Register` (boxd's `BOXD_API_KEY`,
`BOXD_TOKEN`; fake's `DISCO_VM_FAKE_KEY`). A `Key`'s `Secret` is opaque: it
prints, formats, and logs without it.

`IssueKey` makes the narrowest key the driver can for another host to run the
project's machines. An issued key expires (a TTL is required) and cannot
itself issue keys; a driver that cannot promise both returns `ErrNoKey` (it
has no credential to hand on) or `ErrCannotIssue` (its provider cannot make
one), never the caller's own key. Issued keys are named for the project, so
`IssuedKeys` finds them by name, and `RevokeKey` refuses one the driver did
not issue for the project; `disco-vm key revoke` refuses an ID it does not
list, so a typo never reads as revoked. A provider's refusal of a key is
`ErrUnauthorized`.

`Isolated` and `Networks` at create strip what a provider gives a machine by
default and place it on the caller's networks; a driver without
`Capabilities.Isolation` refuses them.

`Egress` at create fences what an instance reaches beyond its networks, where
its root cannot loosen it (ADR 26-10-29-129). It is in place before Create
returns and read back from the provider, so an instance the provider does not
report fenced as asked is destroyed and Create fails; it holds across stop
and start, and is the spec's, never the image's. An empty `Allow` is
nothing beyond the networks; nil is the provider's default. A driver without
`Capabilities.Egress` refuses it. `Instance.Egress` is the fence the provider
reports.

## Ports, not GUIDs

Where there is an agent, it listens on **port 7300**. On vz that is AF_VSOCK.
On hcs it is the hvsocket service ID `00001C84-FACB-11E6-BD58-64006A7986D3`,
which is the vsock template GUID with the port in its first field
(`winio.VsockServiceID`). The hcs driver must list that ID in the VM's HvSocket
ServiceTable. On docker it listens on a root-only unix socket, which
`disco-vm pipe` reaches through docker exec. boxd has no socket from the host
into the guest and no agent: its guest operations are command lines over
boxd's authenticated Exec stream ([boxd/DESIGN.md](boxd/DESIGN.md)). 7301 is reserved for the
display stream. A port is a driver's own detail: the shim, the engine, and a
discobox provider see only `machine.Guest`, and the shim dials one port, the
service's.

The other direction is a port too. A guest process connects out to the host
with `guest.DialHost(port)` (vsock to CID 2 on vz, hvsocket to the parent
partition on HCS), and an instance created with `Forwards` has its shim accept
those connections (`machine.HostListener`) and splice each into a Unix socket on
the host, passing half-closes through. That is how a guest reaches a host
service with no TCP listener anywhere, as libkrun maps a vsock port to a Unix
socket. `disco-vm dial-host PORT` does the same from a shell in the guest.

## A window is per boot, and lives with the VM

`run --gui`, `start --gui`, and `install --gui` ask for a window on the guest's
display. It is a property of one boot, not of the instance: the engine passes
`BootOptions.GUI` (and `InstallSpec.GUI` for an install's boots), and the
driver opens the window in the process that called `Boot`, which is the one
that owns the VM. For `run` and `start` that is the shim (`shim --gui`); for
an install it is the CLI. The window closes when the VM stops, and closing it leaves
the VM running. A driver without `Capabilities.Display` refuses GUI with
`ErrUnsupported` rather than ignoring it.

A native window on macOS must run on the process's main thread, which only
`main` can give away. So `main` locks its goroutine to the main thread and runs
the program through `machine.Main`. A driver that shows windows installs a
`machine.MainLoop` there; vz brings AppKit up on the main thread only when the
first window is asked for, so no other command (and not the guest agent) ever
touches it.

## Every clone is cold, and an image is made fast by what is in it

Create makes one kind of clone (ADR 26-10-15-851): a copy-on-write clone of
the image's disk, with a machine identity of its own, which Boot starts from
power-off. There are no clone modes, no staged images, and no `warm`
command. Work that every instance needs goes into the image, by shaping a
guest and taking a `snapshot` of it, so an instance starts with it done.

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
  is refused at start: remove it and run the image again.

Fast clones may come back once every driver's snapshots are proven
(the ADR's Deferred section), with a design of their own.
