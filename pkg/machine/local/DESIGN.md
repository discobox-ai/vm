# pkg/machine/local

The driver whose provider is this host's disk: the store and the shim that
hcs, vz, and fake share (ADR 26-10-23-417). Each of them is a
`local.Hypervisor`, which installs, clones, boots, commits, and destroys a
disk in a directory it is given; this package keeps everything else.

## A directory of its own is the store

`machine.Config.Dir` (`--root`, `$DISCO_VM_ROOT`, or the per-OS data
directory) is the driver's, as `/var/lib/docker` is Docker's:

- `images/layers/<id>/payload` (the hypervisor's disk) and `layer.json`
  (project, parent, guest OS, comment, generations);
- `images/tags/<project>/<ref in hex>.json`;
- `instances/<id>/machine` (the hypervisor's) and `instance.json` (project,
  name, image, layer or base, size, forwards), and while a shim holds it,
  `shim.json` and `shim.log`;
- `instances/.names/<project>/<name>`, the name's claim, holding the ID;
- `cache/<hypervisor>/`, media the hypervisor downloads.

Nothing takes a lock:

- **A layer is published by renaming its directory into place**, written
  under a temporary name with its record inside. Two builds of one key are
  identical by construction; the first rename wins and the other is
  discarded.
- **A tag is a file**, replaced by a rename.
- **A name is claimed by creating its file exclusively.** A claim with no
  instance behind it for ten minutes is what a crash left, and is taken.
- **An instance's record goes in last when it is made, and out last when it
  is removed**, after its disk, so a removal that fails partway leaves the
  instance listed to remove again, and a crash while making one leaves a
  directory nothing lists.

## A layer records its parent's generation

A hcs layer is a differencing disk over its parent's, so it boots only over
the parent it was built on. Each layer gets a random generation when it is
published and records its parent's (`ParentGeneration`). `Layer` reports a
layer not found when an ancestor is gone, or was made again since, or has a
parent and no recorded generation, so a build makes it again, replacing the
broken one; `Layers` still lists it, so `rmi` can remove it.

## A shim per running VM

A Virtualization.framework VM dies with the process that created it, and an
HCS VM belongs to vmcompute, so `Start` spawns the shim command detached
(setsid, or DETACHED_PROCESS): this executable's `shim` subcommand for the
same root, driver, and project, or `machine.Config.ShimCommand`, a
subcommand of an embedder's own binary that makes the same driver and calls
`ShimMain`. The shim boots the VM, holds it for its whole life, and exits when
it stops. Once the machine answers it writes `shim.json` (PID, loopback
address, bearer token, service address) and serves:

- `/status`; `/stop`, an orderly shutdown forced off after a timeout; `/kill`;
- `/guest`, which upgrades to the agent protocol, answered from its
  machine's guest operations (`guest.Relay`);
- the instance's URL, a loopback port spliced into the machine's
  `Dial(service port)`;
- the instance's forwards, each guest port accepted (`machine.HostListener`)
  and spliced into its Unix socket.

`Attach` reaches a running instance through its shim: its guest operations
are the relay's, `Kill` is `/kill` (a shim from before `/kill` is stopped with
no grace), and `Done` closes once the shim's process has exited.
