# disco-vm in discobox

This is how we expect discobox to use disco-vm. It is a plan, not a record:
the parts marked **built** exist in disco-vm today, and the rest is the order
we expect to do them in. It depends on discobox ADR 0126 ("Remote sandboxes
connect out to their pool"), which is still proposed, and it was written
against discobox `main` at ADR 0143.

## The split

disco-vm is the VM layer. discobox keeps everything above it.

| stays in discobox | moves to, or lives in, disco-vm |
|---|---|
| the Docker pool: `dockerworker.Engine`, the pool-agent container, BuildKit, the registry and Git origins | the hypervisors: vz (Virtualization.framework), libkrun, and hcs |
| the docker, digitalocean, wslc, and execvm providers | VM lifecycle: create, boot, stop, remove, and a supervising shim per VM |
| placement, credentials, the pool proxy, the control plane | images and layers, warm stages, guest agent, vsock in both directions |

The Linux pool driver that only runs Docker containers does not move. What
moves is the two VM implementations underneath `dockerworker.Driver`: discobox's
vz and libkrun providers become a thin `dockerworker.Driver` over
`pkg/engine`, and the VM code itself lives in disco-vm.

## How discobox embeds disco-vm

- **As a library.** The discobox server imports `pkg/engine` (and
  `pkg/machine/drivers` for the drivers its OS can run), opens a state root,
  and calls the engine the way the CLI does. No disco-vm binary is shipped.
- **The shim is a hidden subcommand of the discobox binary** (**built**:
  `engine.Engine.ShimCommand` and `engine.ShimMain`). A running VM is owned by
  a supervisor process, one per VM, because a Virtualization.framework VM dies
  with the process that made it. The engine spawns that supervisor as
  `ShimCommand` plus its own arguments, so discobox sets
  `ShimCommand = []string{exe, "vm-shim", ...}` and its `vm-shim` subcommand
  calls `engine.ShimMain`. This is the same move ADR 0101 made for the libkrun
  launcher. What it keeps:
  - one signed binary (the server already carries the virtualization
    entitlement), and nothing else to ship;
  - VMs that survive a server restart, which ADR 0062's "the VM dies with the
    server" gives up;
  - a crash contained to one VM.
- **Not fully in-process.** Running VMs inside the server is possible
  (`engine.Boot` already does it for builds) but would bring back the VM dying
  with the server, and a native window (`--gui`) needs the process's main
  thread, which a server cannot give.

## vsock

discobox talks to processes in its VMs over vsock in both directions (ADR 0013,
ADR 0062), and never opens a TCP listener for it.

- **Host to guest** (**built**): `engine.Dial(inst, port)` returns a
  `net.Conn` to any guest process listening on that vsock port (hvsocket
  service ID on Windows). It goes through the instance's shim, which holds the
  VM: loopback TCP to the shim, then `/dial?port=N`, which splices to the
  framework's `Connect(N)`. discobox's Docker and pool-agent leases dial this
  way.
- **Guest to host** (**built**): a guest process connects out to the host on a
  vsock port (CID 2 on vz), and the shim forwards it to a Unix socket on the
  host that discobox serves: `engine.CreateOptions.Forwards`, a port to a
  socket path, the way libkrun maps a vsock port to a Unix socket
  (`krun_add_vsock_port`). This replaces the `VirtioSocketListener` that feeds
  `carrierhub.Hub` in discobox's vz provider today. A driver offers it with
  `machine.HostListener` and reports it as `Capabilities.Forward`; guest
  programs dial it with `guest.DialHost`, and `disco-vm dial-host PORT` is the
  same from a shell.

## Native macOS and Windows sandboxes (ADR 0126)

A macOS or Windows guest cannot be a Docker pool: it has no Docker daemon for
each sandbox, and a macOS guest has no nested virtualization (measured: a macOS
27 guest reports `kern.hv_support=0`; Virtualization.framework nests only Linux
guests). Under ADR 0126 the pool keeps one environment (pool-agent, proxy,
BuildKit, registry, Git origins) and the pool agent creates **one provider
environment per sandbox**. disco-vm is that provider, one VM per sandbox:

| 0126 backend operation | disco-vm |
|---|---|
| create | `engine.Create(image, {Name: sandboxID, Forwards: …})`, then `Start`. The default mode is auto, so a warm image resumes in about 4 s and falls back to a cold boot |
| inspect / list | `engine.Get` / `List` and `State`; the name carries the sandbox ID, so a lost create is recovered by name, never duplicated |
| start / stop | `engine.Start` / `Stop` (an orderly shutdown through the guest agent) |
| delete | `engine.Remove(force)` |
| transfer | `guest.Client.CopyTo` / `CopyFrom` (tar) |
| connection | `engine.Dial(inst, port)`, and `Forwards` for the guest's side |

The pool agent runs inside the Linux pool VM and cannot call
Virtualization.framework or HCS, so the backend runs on the host side, in the
discobox server, and the pool agent asks the control plane for it.

discobox must add:

1. **A native-guest sandbox kind.** ADR 0126 assumes each environment boots
   the Linux harness image with sandbox-agent as PID 1. A native guest boots a
   disco-vm image (built from `examples/discobox-base.yaml`), runs
   sandbox-agent as a service, and keeps `/.discobox/{data,cache,config,
   sources,secrets}` as plain directories.
2. **sandbox-agent on darwin and windows.** Its exec, PTY, services, and meta
   APIs are what the server and CLI already speak; the Linux-only PID 1 work
   moves behind build tags. disco-vm's own agent stays the lifecycle and
   bootstrap channel: it shuts the guest down, copies files, and installs
   sandbox-agent. sandbox-agent is reached over vsock like any guest process.
3. **Capacity in placement.** `Capabilities.MaxRunning` (two macOS guests per
   Mac, a framework limit) has to reach scheduling, or a third macOS sandbox
   fails at start.
4. **The sandbox user, per guest OS.** ADR 0141 requires a sandbox uid in
   [1000, 60000] and maps a macOS host's 501 to 1000. That range is Debian's
   and right for Linux. On a macOS guest 501 is the ordinary first user: the
   install moves its own account to 600 so that `warm --local-user` can give
   501 to the host's user, and files on a shared directory then have one owner
   on both sides. We would ask 0141's range to be per guest OS (macOS: 501 and
   up); a native macOS sandbox created as 1000 still works, and only loses that
   match.
5. **Warm stages as the fast path.** `engine.Warm(image, {User: …})` stages an
   image logged in as the sandbox user. On vz a stage is two saved templates,
   one per guest the framework can run at once, and serves any number of
   clones; on hcs it is a live fork template.

## Moving vz and libkrun (the next phase)

discobox's vz and libkrun providers run the Linux pool VM: a released guest
image (ADR 0062, ADR 0101) with Docker inside, reached over vsock. Moving them
means disco-vm grows what that VM needs, then discobox's providers shrink to an
adapter:

| the pool VM needs | disco-vm |
|---|---|
| Linux guests: vz's generic platform with a Linux boot loader; libkrun on KVM | macOS guests on vz; Linux planned |
| boot a released image by digest (kernel, initrd, raw root, ADR 0101), not an install from media | layers from `install` or `build`; an import path is needed |
| data and cache disks that persist when the VM is re-created | one disk per instance; attached volumes are needed |
| guest to host vsock | built (above) |
| passt networking with TSI off (libkrun, ADR 0013) | the framework's NAT on vz; a libkrun driver brings its own |
| the guest's lifecycle service (`discobox-vsock-guest`) | disco-vm's agent builds for Linux and speaks vsock; the guest image would carry it |

What it buys: one VM layer for every discobox VM, shims (a pool VM survives a
server restart), and warm stages for pool VMs (a Linux vz guest saves and
restores too). The libkrun launcher (ADR 0013 §1) is already shaped like a
shim: its own process session, a lock, re-adoption.

## Order

1. **Done in disco-vm:** the shim as the embedder's subcommand; vsock in both
   directions.
2. **discobox:** embed the engine for native macOS and Windows sandboxes under
   ADR 0126, with the shim in the discobox binary.
3. **disco-vm:** Linux guests on vz, image import by digest, attached volumes.
   Then discobox's vz provider becomes an adapter.
4. **disco-vm:** a libkrun driver (Linux host), and discobox's libkrun provider
   becomes an adapter.
