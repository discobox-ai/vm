# pkg/machine/local

The driver whose provider is this host's disk: the store and the shim that
hcs, vz, libkrun, and fake share (ADR 26-10-23-417). Each of them is a
`local.Hypervisor`, which installs, copies, boots, freezes, and destroys a
disk in a directory it is given, says whether its disks chain, and has a
base or none (`Base`, from `machine.Config.Base`); this package keeps
everything else. It refuses what the hypervisor's `Capabilities` leave
out, before the hypervisor is asked: a commit and a copy where it does not
report `Commit` (fake's `NoCommit`; ADR 26-11-03-868).

## A directory of its own is the store

`machine.Config.Dir` (`--root`, `$DISCO_VM_ROOT`, or the per-OS data
directory) is the driver's, as `/var/lib/docker` is Docker's:

- `instances/<id>/machine` (the hypervisor's) and `instance.json` (pool,
  name, the name and ID of the VM it was created from, guest OS, size,
  forwards, fence, volumes), and while a shim holds it, `shim.json`,
  `shim.sock`, and `shim.log`. A committed VM (ADR 26-11-03-606) is the
  same, left where it is with its volumes gone: nothing marks it committed,
  which is the caller's to record. A record from before that ADR, of an
  image or a base (`imageID`, `layer`, `base`), is not read: its disk may
  difference over an image this driver no longer knows. While a VM is
  created from another, `from` in its directory names that one's ID, before
  its chain is read and its record written;
- while an upgrade runs, `instances/<id>/machine.next` (the new disk),
  `next.ready` (written once it is whole), and `machine.old` (the old disk,
  once the new one is in place), [below](#an-upgrade-swaps-a-vms-disk-in-its-directory);
- `instances/.names/<pool>/<name>`, the name's claim, holding the ID;
- `caches/<pool>/<key>.json`, a cache key, naming its current
  generation, and `caches/generations/<id>/payload` (the hypervisor's copy
  of a cache volume, which `Hypervisor.SaveCache` writes and `Prepare` clones
  from `InstanceSpec.CacheFrom`) and `generation.json` (pool, key, size,
  when it was saved). A generation is published by renaming it into place,
  and a key moves by replacing its file; the generation it moved off is
  renamed out of reach and removed, and a Create cloning it in that moment
  is retried once with the new one. Every instance's cache volume is its own
  copy, so nothing but a key needs a generation;
- `cache/<hypervisor>/`, what the hypervisor downloads: media it installs
  from, and, for vz and libkrun, Linux guest images pulled per digest
  (`cache/<hypervisor>/linux/<digest>`, `pkg/machine/linuximage`), and
  libkrun's runtime (`cache/libkrun/runtime/<digest>`). `Prepare` is given
  it, and `Driver.CacheDir` names it;
- `keys/<hash>.json`, a key the driver issued, named for the SHA-256 of its
  secret (ID, name, pool, expiry, and that hash, never the secret), for a
  hypervisor whose capabilities issue keys. A call reads its own key's
  record and no other; one that cannot be read authorizes nothing and is
  not listed.

Nothing takes a lock:

- **A record is replaced by a rename**, as Commit drops its volumes.
- **A name is claimed by creating its file exclusively.** A claim with no
  instance behind it for ten minutes is what a crash left, and is taken.
- **An instance's record goes in last when it is made, and out last when it
  is removed**, after its disk, so a removal that fails partway leaves the
  instance listed to remove again, and a crash while making one leaves a
  directory nothing lists.

## A chain is the driver's, below the seam

A VM created from a committed one is a copy: an APFS clone on vz, a reflink
or sparse copy on libkrun, and a tree
on fake, made at Create and its own from then on, so the committed VM can be
removed and the copy lives on. An hcs copy, though, is a differencing disk
over the committed VM's, so for a hypervisor that `Chains` the record keeps
the ID of the VM it was created from (`fromID`); Prepare, Boot, Commit, and
Destroy get the whole chain (`InstanceSpec.Chain`, base first); and a VM
another VM's record names, in any pool, is refused removal and boot with
`machine.ErrInUse`, since a boot would write to a disk another differences
over. A directory with no record yet whose `from` names the VM holds it too, for
ten minutes, as a name's claim does: a Create writes `from` before it reads
the chain, so a removal sees the copy being made, though a removal and a
create in the same instant may both pass, since nothing takes a lock. An
older `from` is what a crashed Create left, and holds nothing. Nothing marks a VM committed, so this is the one
refusal `local` derives on its own. A VM whose chain was removed behind the driver's back is
not found at boot, and is still removed. vz, libkrun, and fake get the committed VM
alone, at Prepare only.

## An upgrade swaps a VM's disk in its directory

`Upgrade` (ADR 26-11-03-782), for a hypervisor that lists `VolumeData`
(fake), keeps the VM's ID, directory, record, and name's claim, so nothing
else is ever listed. Its record names the new root (`upgrading`: the
committed VM's name and ID, or none for a fresh one) before any disk is
touched, and while it does Boot, Start (through `runnable`), and Commit
refuse the VM, and a removal of that committed VM is `ErrInUse` on a
hypervisor that chains. Then each step not yet taken:

1. `Prepare` makes the new disk in `machine.next` with no volumes, after a
   `Destroy` of whatever an interrupted upgrade left there, and `next.ready`
   is written;
2. `Hypervisor.MoveVolumes` moves the volumes from the old disk to the new,
   keeping what an interrupted call moved;
3. `machine` is renamed to `machine.old`, then `machine.next` to `machine`;
4. the old disk is destroyed, `next.ready` removed, and the record written
   with its new `from` and no `upgrading`.

`next.ready` with no `machine.next` is a swap already done. Upgrading again
from the recorded root reads its committed VM only to make a new disk that
is not whole yet, so one removed since does not keep it from finishing. An
upgrade to another root replaces the recorded one while the new disk is not
whole (the old disk is untouched until then), and is refused after, until
this one is finished;
`Destroy` removes `machine.next` and `machine.old` with the rest. hcs and vz
attach no volume yet, so they refuse it.

## Keys are the fake's, and checked on every call

A hypervisor that lists `Capabilities.Keys` as `issue` (fake) has its
driver issue keys bound to the pool, and check `Config.Keys` at every
call: the key must be one it issued for the pool that has not expired or
been revoked (its record removed). A name is issued once per pool while
a key of it lives (`machine.ErrExists`); expired records go when the next key
is issued. A method that takes an instance refuses one whose record is
another pool's. Only the owner, the driver given no
key, issues, lists, or revokes keys. A shim the driver starts runs as the
owner, and neither a shim nor a fake guest is handed the variables a key is
read from (`machine.WithoutKeys`). hcs, vz, and libkrun issue none (`machine.ErrNoKey`): whoever can read the
directory owns it, and a key source is not asked.

## A shim per running VM

A Virtualization.framework VM dies with the process that created it, and an
HCS VM belongs to vmcompute, so `Start` spawns the shim command detached
(setsid, or DETACHED_PROCESS): this executable's `shim` subcommand for the
same root, driver, and pool, or `machine.Config.ShimCommand`, a
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
  `Dial(N)`: the agent's port or the service's (`guest.DialablePort`), the
  service's dialed where `Hypervisor.Service` says, which only the owner can
  reach; any other port is refused.

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
