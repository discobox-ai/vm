# pkg/machine/local

The driver whose provider is this host's disk: the store and the shim that
hcs, vz, and fake share (ADR 26-10-23-417). Each of them is a
`local.Hypervisor`, which installs, clones, boots, commits, and destroys a
disk in a directory it is given, and says whether its disks chain; this
package keeps everything else.

## A directory of its own is the store

`machine.Config.Dir` (`--root`, `$DISCO_VM_ROOT`, or the per-OS data
directory) is the driver's, as `/var/lib/docker` is Docker's:

- `images/layers/<id>/payload` (an image's disk) and `layer.json` (project,
  guest OS, comment, and, for a hypervisor that chains, the parent), their
  names from when images were layers;
- `images/tags/<project>/<ref in hex>.json`;
- `instances/<id>/machine` (the hypervisor's) and `instance.json` (project,
  name, image, image ID or base, size, forwards, fence), and while a shim
  holds it, `shim.json`, `shim.sock`, and `shim.log`;
- `instances/.names/<project>/<name>`, the name's claim, holding the ID;
- `cache/<hypervisor>/`, what the hypervisor downloads: media it installs
  from, and, for vz, Linux guest images pulled per digest
  (`cache/vz/linux/<digest>`). `Bases`, `Base`, and `Prepare` are given it;
- `keys/<hash>.json`, a key the driver issued, named for the SHA-256 of its
  secret (ID, name, project, expiry, and that hash, never the secret), for a
  hypervisor whose capabilities issue keys. A call reads its own key's
  record and no other; one that cannot be read authorizes nothing and is
  not listed.

Nothing takes a lock:

- **An image is published by renaming its directory into place**, written
  under a temporary name with its record inside, under a random ID.
- **A tag is a file**, replaced by a rename.
- **A name is claimed by creating its file exclusively.** A claim with no
  instance behind it for ten minutes is what a crash left, and is taken.
- **An instance's record goes in last when it is made, and out last when it
  is removed**, after its disk, so a removal that fails partway leaves the
  instance listed to remove again, and a crash while making one leaves a
  directory nothing lists.

## A chain is the driver's, below the seam

Above the seam an image has no parent (ADR 26-10-23-410). An hcs snapshot,
though, is a differencing disk over the disk of the image its instance ran
from, so a hypervisor that `Chains` gets the parent recorded: Commit writes
the instance's image ID as the snapshot's `parent`, Prepare, Boot, and
Destroy get the whole chain (`InstanceSpec.Chain`, base first), and
`DeleteImage` refuses with `machine.ErrInUse` while any image's record, in
any project, names the image as its parent, so a disk another still needs is
never deleted. `Image` reports an image not found when an ancestor is gone
(deleted behind the driver's back); `Images` still lists it, so `rmi` can
remove it. vz and fake do not chain: their images record no parent, and each
is a copy of its own.

## Keys are the fake's, and checked on every call

A hypervisor that lists `Capabilities.Keys` as `issue` (fake) has its
driver issue keys bound to the project, and check `Config.Keys` at every
call: the key must be one it issued for the project that has not expired or
been revoked (its record removed). A name is issued once per project while
a key of it lives (`machine.ErrExists`); expired records go when the next key
is issued. A method that takes an instance refuses one whose record is
another project's. Only the owner, the driver given no
key, issues, lists, or revokes keys. A shim the driver starts runs as the
owner, and neither a shim nor a fake guest is handed the variables a key is
read from (`machine.WithoutKeys`). hcs and vz issue none (`machine.ErrNoKey`): whoever can read the
directory owns it, and a key source is not asked.

## A shim per running VM

A Virtualization.framework VM dies with the process that created it, and an
HCS VM belongs to vmcompute, so `Start` spawns the shim command detached
(setsid, or DETACHED_PROCESS): this executable's `shim` subcommand for the
same root, driver, and project, or `machine.Config.ShimCommand`, a
subcommand of an embedder's own binary that makes the same driver and calls
`ShimMain`. The shim boots the VM, holds it for its whole life, and exits when
it stops. A shim is detached on purpose, and outlives whoever started it, an
embedder's process included: a NIC-less guest's way to its peers must not
depend on its caller (ADR 26-10-31-990). Once the machine answers it writes
`shim.json` (PID, socket, bearer token, service address), private to the
owner, and serves its control API on `shim.sock`, an owner-only Unix socket
in the instance's directory, behind the token. A socket path too long for a
Unix socket (macOS holds 104 bytes) is one in a private temporary directory
instead, which `shim.json` names. The API is:

- `/status`; `/stop`, an orderly shutdown forced off after a timeout; `/kill`;
- `/guest`, which upgrades to the agent protocol, answered from its
  machine's guest operations (`guest.Relay`);
- `/dial?port=N`, which upgrades to a stream spliced into the machine's
  `Dial(N)`: any guest port, which only the owner can reach.

It also serves the instance's URL on a loopback TCP port spliced into the
machine's `Dial(service port)`, since ADR 0005 makes that an HTTP URL any
process reaches; and the instance's forwards, each guest port accepted
(`machine.HostListener`) and spliced into its Unix socket, or into its peer's
guest port through the peer's shim's `/dial`.

`Attach` reaches a running instance through its shim: its guest operations
are the relay's, `Dial` is `/dial`, `Kill` is `/kill` (a shim from before
`/kill` is stopped with no grace), and `Done` closes once the shim's process
has exited. A shim from before `shim.sock` named a loopback address in
`shim.json` instead, and is reached there until it exits.
