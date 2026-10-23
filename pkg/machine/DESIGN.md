# pkg/machine

The driver seam: `machine.Driver` and the guest's operations,
`machine.Guest`. Each hypervisor is a package below it, selected by build tags
(`drivers`), and `machinetest` is the conformance suite every driver passes.
boxd, docker, and local have their own design notes:
[boxd/DESIGN.md](boxd/DESIGN.md), [docker/DESIGN.md](docker/DESIGN.md), and
[local/DESIGN.md](local/DESIGN.md).

## The driver contract is the intersection, and differences are reported

`machine.Driver` is one project's (`machine.Config.Project`) layers (Install,
Layer, Layers, DeleteLayer, Commit), tags (Tags, Tag, Untag), bases, and
instances (Create, a clone; Instance, Instances, Destroy), as its provider
keeps them ([the root DESIGN.md](../../DESIGN.md#the-engine-keeps-no-state-and-a-driver-keeps-it-in-its-provider));
and an instance's machine: Boot, in this process; Start, outliving it;
Attach, from any process; and Service
([the service URL](../engine/DESIGN.md#every-instance-has-one-service-at-one-http-url)).
Boot and Start return once the guest answers. A running `Machine` is the
guest's operations (`machine.Guest`: Exec with stdio, a TTY and its resizes,
a user, environment, and directory; CopyTo and CopyFrom, as tar; Info;
Shutdown), and Kill and Done. hcs, vz, and fake are `local.Hypervisor`s under
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
| agent (Install bakes in `disco-vm guest`) | yes | yes | **no**: boxd's Exec | yes | yes |

The engine enforces `MaxRunning`. Every driver clones the same way: cold
([below](#every-clone-is-cold-and-an-image-is-made-fast-by-what-is-in-it)).

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

`run --gui`, `start --gui`, and `build --gui` ask for a window on the guest's
display. It is a property of one boot, not of the instance: the engine passes
`BootOptions.GUI` (and `InstallSpec.GUI` for an install's boots), and the
driver opens the window in the process that called `Boot`, which is the one
that owns the VM. For `run` and `start` that is the shim (`shim --gui`); for a
build it is the CLI. The window closes when the VM stops, and closing it leaves
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
command. Work that every instance needs goes into the image, as a build step
or by shaping a guest and taking a `snapshot` of it, so an instance starts
with it done.

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

Fast clones may come back once every driver's builds and snapshots are proven
(the ADR's Deferred section), with a design of their own.
