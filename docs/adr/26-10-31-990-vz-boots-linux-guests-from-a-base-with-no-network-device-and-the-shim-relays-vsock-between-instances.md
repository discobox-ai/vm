# 26-10-31-990 — vz boots Linux guests from a base beside macOS ones, with no network device when asked, and an instance's guest ports reach its caller and its peers over vsock

- **Status**: Proposed
- **Date**: 2026-10-31

## Context

disco-vm's local drivers launch only the host's own OS: macOS on vz, and
Windows on hcs. `Capabilities.GuestOS` is a list, and `MaxRunning` is per
guest OS, but every local driver lists one OS, installs it from media, and
bakes into it the running binary as its agent.

On 2026-10-31 the owner decided that a local disco-vm pool runs its pool
agent and proxy in a dedicated Linux VM that discobox's server creates, as it
creates every sandbox VM. A sandbox VM then has no network card, and vsock to
the pool VM's proxy is its only way out, which makes "zero egress except the
proxy" true by construction (issue #27; discobox-ai/discobox#418, which
decides discobox's side). So disco-vm must boot a Linux guest on a Mac beside
macOS ones, with or without a network device, and let its caller and its
other guests reach that guest's vsock ports.

discobox already boots Linux on Virtualization.framework for its Docker
pools, and proved there what works (discobox ADRs 0062, 0101 and 0013):

- **Boot**: `VZLinuxBootLoader` with an uncompressed arm64 kernel, the
  distribution's initrd, and a command line carrying `console=hvc0` and
  `panic=`. Without `panic=` a panicked kernel halts in place and the
  framework reports a VM running with nothing inside. The kernel is
  decompressed when the image is built, not on the Mac. Devices are virtio:
  block, the NAT attachment (which serves DHCP and DNS), a socket device, a
  serial console to a log file, entropy, and a balloon, which a Linux guest
  respects.
- **Disks** are raw: the framework has no QCOW2 path. The root is an ext4
  filesystem that `mkfs.ext4 -d` fills inside the image build, with no loop
  device and no privileges, so CI, a Linux developer and BuildKit in a running
  VM all build the same artifacts.
- **Artifacts** are one OCI image whose final `FROM scratch` stage holds
  `vmlinux`, `initrd.img` and `root.ext4`, published on a release line of its
  own and pinned by digest (`guestimage.DefaultVMImage`). `guestimage` pulls
  it with go-containerregistry, with no daemon, checks the image's declared
  architecture, caches it per manifest digest, and prefers a complete local
  build. A Mac needs only a signed binary and the registry.
- **The guest** takes its address by DHCP, matching both `enp0s*` and `eth0`,
  so one image boots on vz and libkrun. It steps its clock from the PL031 RTC
  every 30 s, because a Mac that slept leaves the guest hours behind and every
  time-bounded token then fails both ways. Its initrd is built from a module
  list, and driver classes virtio never needs are deleted.
- **Not carried over**: discobox's guest is Docker's host and nothing else.
  Its root is read-only and shared by every pool, its writable state is on
  data and cache disks at a fixed `vda`/`vdb`/`vdc` order, its vsock helpers
  are discobox's, and it has no disco-vm agent. libkrun boots only
  libkrunfw's patched kernel. wslc boots WSL's own VM, and streams a relay
  into it over a guest process's stdin.

What disco-vm adds is its own model. An image is a snapshot of a stopped
instance (ADR 26-10-23-410). An instance is a cold, copy-on-write clone with
an identity of its own (ADR 26-10-15-851). A guest is shaped through exec and
copy and snapshotted, not converged (ADR 26-10-15-703). A pool machine is a
base that a bundle is applied to through those operations (discobox ADR
26-10-23-551). The agent ships with the binary that talks to it, so a
protocol change reaches both ends at once.

On vz today an instance's vsock reaches its caller only as its one service,
which its shim splices to a loopback URL, and as `Forwards`, guest ports the
shim splices into Unix sockets on the host. Its `PrivateURL` is empty (ADR
26-10-29-129), and discobox ADR 26-10-29-481 lets no driver carry a machine
pool without one. ADR 0001 §3 took raw guest ports off `machine.Machine`
because no caller needed a raw stream once the service URL existed, and ADR
26-10-29-129 §2 decided that vz lists no `Egress`. A guest with no network
device undoes both premises.

This ADR supersedes in part ADR 0001 §3 (raw guest ports leave the seam: §4
brings `Dial` back) and ADR 26-10-29-129 §2 (vz lists no `Egress`, and
`Capabilities.Egress` is a bool: §5). Both are marked so when it is accepted.
ADR 0005's one service at one URL stands: `Dial` is a stream, not a URL.

## Decision

### 1. A driver lists every guest OS it boots, and the image or base picks one

`Capabilities.GuestOS` is every OS the driver boots. On a Mac, vz lists
`darwin` and `linux`. A caller asks for a Linux guest by running a Linux
image or base, whose `GuestOS` the instance takes, as it does today. There is
no OS flag on `run`.

`Capabilities.Install` becomes the list of guest OSes the driver installs
from media, in place of a bool: vz `[darwin]`, hcs `[windows]`, fake the
host's, and boxd and docker none. `engine.Install` defaults to the only OS
in that list. Listing an OS in `GuestOS` no longer means the driver installs
it.

### 2. vz boots Linux directly: a kernel, an initrd, and a raw ext4 disk of the instance's own

A Linux guest on vz is a bundle, as a macOS one is. The local driver keeps it,
APFS-clones it, renames it into an image at Commit, and holds it under a shim:

| file | image | instance |
|---|---|---|
| `vmlinux` (uncompressed) | ✓ | ✓ |
| `initrd.img` | ✓ | ✓ |
| `disk.img` (raw ext4, the root) | ✓ | ✓ |
| `vz.json` (version, guest OS, size) | ✓ | ✓ |
| `machineid`, `macaddress` | | its own, from Create |

- **The boot loader is `VZLinuxBootLoader`** on the generic platform, with
  `console=hvc0 root=/dev/vda rw rootfstype=ext4 panic=5` and the instance's
  identity (below). It is not EFI.
- **The kernel and initrd are the image's**, carried from base to instance
  to snapshot. A guest that installs a kernel package still boots the image's
  kernel, so the base holds its kernel package.
- **The root is the instance's own and writable.** Create clones the image's
  `disk.img` and extends it, sparse, to the instance's disk ceiling (64 GiB,
  or the base's `disk` option). The base's fstab grows the filesystem online
  at boot (`x-systemd.growfs`). There is no read-only shared root and no data
  disk. APFS clones already share what instances have in common, and a
  snapshot is then one disk.
- **Devices** are discobox's: virtio block, a socket device, a serial
  console appended to `console.log` in the instance's directory (the one
  record a guest that never answered leaves), entropy, a balloon, the NAT
  attachment unless §5 removes it, and the virtiofs shares a boot asks for.
  There is no framebuffer, so a Linux guest has no `--gui`.
- **Identity**: Create gives the instance a MAC and a machine ID. Boot
  passes the ID as `systemd.machine_id=`, so a clone of a snapshot is never
  its source's twin. The base's network takes its DHCP lease by MAC
  (`ClientIdentifier=mac`), so two clones never share an address on the NAT.
- **The agent arrives at every boot.** Boot appends to the image's initrd a
  `newc` cpio archive holding the disco-vm agent for linux on the host's
  architecture. The base's initramfs hook moves it into `/run`, and its
  `disco-vm-guest.service` runs it, listening on vsock 7300. Nothing of
  disco-vm's is written into the disk, so an image made by an older disco-vm
  boots the running one's agent.
- **Where the agent comes from**: `$DISCO_VM_AGENT`, read by the driver, as
  docker's is. A darwin binary carries no linux one, so `go tool task build`
  cross-compiles `build/linux-arm64/disco-vm` beside it, and a release ships
  both. An embedder sets the variable for its shim command, as it would for
  docker. Booting a Linux guest with the variable unset, or naming a file
  that is not a linux ELF of the host's architecture, fails at Boot and
  names the fix, before the VM starts. Once the agent answers, Boot compares
  the protocol version its `Info` reports with the host's: `pkg/guest`
  declares that constant, and it moves when the protocol does. A different
  one is killed and refused, so the protocol ships to both ends at once by
  check, not by convention. It is not `version.String()`, which is the main
  module's: it never matches in an embedder such as discobox, and it is
  `dev` for any two builds with no VCS data. An embedder ships a linux
  disco-vm built from the disco-vm module version it embeds.
- **Shutdown** goes through the agent (`systemctl poweroff`); `Kill` is the
  framework's stop.

### 3. A Linux guest is a base: disco-vm's own image, pinned by digest, pulled by Create

Linux guests come from bases, not media. vz's `Base(ref)` knows an OCI
reference to a disco-vm Linux image without having pulled it, as docker's
knows any image a registry answers for. `Bases` lists the compiled-in default
(`linux`, pinned by digest) and every image already cached. Create pulls a
base the cache lacks with go-containerregistry, with no daemon. It refuses an
image whose declared platform is not linux on the host's architecture, or
that lacks one of the three artifacts. It caches the artifacts per manifest
digest under `<root>/cache/vz/linux/`, and clones the instance from there. A
reference may instead be a directory holding the three files, used as it is.
That is how a guest built from the checkout is booted without publishing it.

The image is disco-vm's: a Dockerfile under `pkg/guest` built on discobox's
`vm-image` pipeline. It has a `FROM scratch` artifact stage, assembly inside
the build, the kernel decompressed at build time, the initrd built from a
module list, DHCP matching both NIC names, the RTC clock step, and
`systemd-networkd-wait-online` left enabled. Boot adds `disco-vm.nic=0` to
the command line of a guest it attaches no network device (§5), and a
drop-in conditions the unit on its absence
(`ConditionKernelCommandLine=!disco-vm.nic=0`). So a guest booted with none
skips the unit rather than failing it 30 s into every boot, and reaches
`network-online.target` at once, which is true of a guest with no network. A
guest with a NIC still waits for its lease, however late udev loads
`virtio_net`, because the condition reads what Boot decided, not a device
that may not exist yet. The agent's unit orders after no network target, so it
answers on vsock whatever the network does. Its content is a general Debian
machine with systemd and nothing else: no Docker, no discobox. On top of that
it carries the agent's unit and initramfs hook, `x-systemd.growfs`, and the
DHCP client identifier. It is published as `linux/arm64` and `linux/amd64`
on a release line of its own (`linux/v*` tags,
`ghcr.io/discobox-ai/disco-vm-linux`). Like discobox's, a guest fix then
needs no disco-vm release, and a disco-vm release implies no new guest. The
pin moves by hand.

discobox's pool VM is this base with the pool-agent bundle applied through
copy and exec, as discobox ADR 26-10-23-551 applies it on boxd. It is not
discobox's `vm-image` guest, which carries no agent and so is no base here.

### 4. A guest's vsock ports reach its caller, the host, and its peers

All three directions go through the shim. `Capabilities.Forward` becomes
`Capabilities.Ports`, which covers all three: hcs, vz and fake list it, for
every guest OS, and boxd and docker do not.

- **Caller to guest:** `machine.Machine` gains `Dial(ctx, port)`, a stream to
  any guest port. An attached machine's goes through its shim (`/dial`, an
  upgraded connection that carries the shim's bearer token).
  `Engine.Dial(ctx, inst, port)` and `disco-vm dial INSTANCE PORT` (stdio) are
  how callers use it. The service URL stays as it is: one port, at a
  loopback URL.
- **Guest to host:** `Forwards`, guest ports spliced into Unix sockets, as
  today.
- **Guest to peer:** a `Forward` may target another instance of the
  project's guest port (`Instance`, `InstancePort`) instead of a `Socket`.
  The engine's check takes exactly one of them: an absolute `Socket`, or a
  valid instance name and a port.
  The instance's shim accepts the guest's connections on `Port` and splices
  each into the target's shim `/dial`. A target that is not running refuses
  the connection, and the guest sees a refused connection. A peer forward is
  the spec's, fixed at Create, and reported in `Instance.Forwards`. It holds
  across stop and start, and it carries no identity: the target sees the
  host. disco-vm decides nothing else about it.

So disco-vm relays between two of its guests, by the spec its caller wrote,
and the relay outlives the caller's process just as the shim does. A caller
that needs a relay to change after Create, or that needs a policy per
connection, builds that relay from `Dial` and a `Socket` forward. vz's
`PrivateURL` stays empty: no address reaches a vz guest.

Peer forwards are static. They carry what a NIC-less guest must reach and
cannot be allowed to lose when its caller restarts: a sandbox to its pool's
proxy. Traffic toward guests made after the source started (a pool to the
sandboxes it asks for) reaches them through their service URL or `Dial`,
from the caller. discobox-ai/discobox#418 decides whether that is the server
alone or the pool agent too. If it keeps pool-to-sandbox traffic in the pool
VM, this ADR is amended before acceptance, not after (Deferred).

### 5. A guest with no network device is the empty egress fence

`Egress{}` (fenced to nothing beyond its networks) on vz is an instance with
no network device, on either guest OS. vz has no provider networks. Its
guest's only reach is the vsock its spec forwards, so nothing in the guest
can loosen the fence. Create records it in `instance.json`, which is vz's
provider (ADR 26-10-23-417), so ADR 26-10-29-129's read-back is that record:
Boot attaches a NIC only to an instance whose record holds no fence, and
`Instance.Egress` reports the record. A non-empty `Allow` on vz is
`ErrUnsupported`, because the NAT is all or nothing. `Capabilities.Egress` becomes the fences a
driver holds, in place of a bool: `none` alone (vz) or `none` and `allow`
(boxd).

### 6. Linux guests do not count against vz's two macOS guests

`MaxRunning` stays `{darwin: 2}`, which the engine enforces per guest OS, so
Linux guests are bounded only by the Mac's memory. A Linux guest's default
size is 2 vCPUs and 2 GiB, not the host-scaled size of a discobox pool VM.
The balloon gives back what the guest frees.

### 7. What hcs inherits

The same design, with hcs's mechanisms in place of vz's:

- **Inherited unchanged:** §1, §3's base (its `linux/amd64` variant), §4
  (the shim, the hvsocket service ID of a port as for the agent), §5 (an HCS
  document with no network adapter), and §6.
- **hcs's own:** the boot path is HCS's direct kernel boot
  (`Chipset.LinuxKernelDirect`: kernel, initrd, command line), which WSL2
  and LCOW use. The root is a dynamic VHDX, made at pull. Appending a footer
  makes the raw image a fixed VHD. `CreateVirtualDisk` copies that into a
  new VHDX (its `SourcePath`), which is resized to the disk ceiling, and the
  guest grows the filesystem as on vz. Clones and snapshots then chain over
  a VHDX parent, as hcs's do today.
- **What hcs needs from the base:** its amd64 variant publishes an
  uncompressed `vmlinux`, which direct boot loads (LCOW's
  `KernelDirect`). discobox's pipeline publishes none for amd64, so the base
  build extracts one from the bzImage. The variant also keeps Hyper-V's drivers (vmbus, storvsc,
  netvsc, hv_sock, hv_utils) in the module list. Its clock comes from
  Hyper-V's time sync, not a PL031.
- **vz-only:** `VZLinuxBootLoader`, raw disks and APFS clones, the NAT
  attachment, the PL031 clock step, virtiofs, and the two-macOS-guest limit.

## Alternatives rejected

- **EFI boot (`VZEFIBootLoader`) of a partitioned disk.** The guest would own
  its kernel, and a distribution's raw cloud image (Debian publishes one)
  would boot unchanged, which is what it would win. But discobox proves
  direct boot, not EFI. EFI needs a GPT disk with an ESP and a variable store
  per instance, boots firmware and GRUB before the kernel, and leaves hcs's
  direct boot path, which shares §2's kernel, initrd and command line.
- **discobox's `vm-image` guest as disco-vm's Linux base.** It carries no
  agent, so disco-vm cannot exec in it or copy into it. Its read-only root
  and data-disk contract are a Docker host's, not a machine that is shaped
  and snapshotted, and its scope rule forbids exactly what a base needs. Its
  pipeline is taken, not its content.
- **One image for discobox and disco-vm now.** The two answer different
  scope rules. Converging them is revisited if discobox's Docker pools ever
  move onto disco-vm (Deferred).
- **Install Linux from media, as macOS is.** A Debian installer run
  unattended on a Mac needs a console or preseeding and takes minutes. A
  digest-pinned image is the same bytes on every Mac, and Create pulls it in
  seconds.
- **The agent baked into the base at image build.** Its version would be
  the base's, so a protocol change would need a guest release, and the agent
  would skew from the host binary in both directions.
- **The agent over a read-only virtiofs share.** virtiofs is vz's alone, and
  hcs's direct boot takes an initrd too. Appending a cpio archive costs one
  writer in Go and no device.
- **The agent written into the disk at Create.** A Mac has no ext4 writer,
  and the agent would then be the image's, not the boot's.
- **The caller relays guest-to-guest vsock, and disco-vm only exposes
  ports.** It would work from `Dial` and `Socket` forwards, which disco-vm
  offers anyway. But every sandbox's way out would then live in the caller's
  process and die with it. And a spec could no longer say, nor `Instance`
  report, what a NIC-less guest reaches, so machinetest on vz and hcs could
  not check the fence against the spec.
- **Routing by name through one gateway port** (a guest names the peer and
  port at the start of each connection). It is dynamic, but it puts a policy
  inside disco-vm and a protocol inside every guest (Deferred).
- **A `NoNetwork` flag beside `Egress`.** A caller would have two ways to
  say "no egress", and `Egress{}` on vz would be refused although vz holds
  it more strongly than any allowlist.
- **A read-only root shared by every instance, as discobox's pools share
  one.** A disco-vm instance is shaped as a whole and snapshotted. APFS
  clones already share the bytes, and a second disk would make a snapshot
  two.

## Consequences

- The seam changes: `Capabilities.Install` and `Capabilities.Egress` change
  type, and `Capabilities.Ports`, `Machine.Dial`, `Engine.Dial` and
  `Forward`'s instance target are added. Every driver and fake follow:
  boxd's and docker's `Dial` are `ErrUnsupported`. Fake's dials the host's
  loopback, so its guests share one port space: on fake, a peer forward
  proves the engine's and shim's plumbing (the record, the splice into the
  target's `/dial`, a stopped target refused), and only vz and hcs prove
  which instance answered. `disco-vm dial` is a new command.
- vz's bundle code handles two layouts, read from `vz.json`'s guest OS. A
  bundle from before records none and is macOS.
- disco-vm gains a release line (`linux/v*`), a Dockerfile and its verify
  step in CI, a linux/arm64 cross-compile of the agent in `go tool task
  build`, and the pin to move by hand.
- machinetest runs a Linux base on vz, gated on `DISCO_VM_VZ_LINUX_BASE`,
  beside a macOS instance, so issue #27 is done when that runs on a Mac:
  booted with no NIC, its vsock reached through `Dial` and from a peer
  forward. The proof table gains rows for a Linux guest on vz, an instance
  with no network device, `Dial`, and a peer forward. Claims about
  `systemd.machine_id=` winning over a persisted `/etc/machine-id`, the
  DHCP lease by MAC, `x-systemd.growfs` on a root with no partition table,
  the initrd's cpio reaching `/run`, the wait-online condition with a NIC and
  without one (no unit failed, and how long the boot took), and a macOS
  guest booting and answering with no NIC are checked in a real guest before
  their rows are.
- vz still gives no `PrivateURL`, so discobox ADR 26-10-29-481's rule that a
  driver with no private URL cannot carry a machine pool must give way to a
  vsock path for vz in discobox-ai/discobox#418.
- A Linux guest's command line, initrd hook and unit are a contract between
  disco-vm and its base. Changing them is a coordinated release: the base
  first, then the pin.

## Deferred

- **Rosetta** (`VZLinuxRosettaDirectoryShare`) for `linux/amd64` binaries on
  Apple silicon. Revisit when a caller needs an amd64-only image.
- **Changing an instance's forwards after Create, or routing to peers by
  name.** Revisit when a guest must reach peers made after it started,
  through disco-vm rather than its caller, which discobox-ai/discobox#418
  settles for the pool VM. If it does, this ADR is amended before it is
  accepted.
- **One guest image with discobox's `vm-image`.** Revisit if discobox's
  Docker pools move onto disco-vm's vz driver.
- **EFI boot.** Revisit when a caller needs a guest that manages its own
  kernel, or a distribution's image unchanged.
- **Intel Macs.** The amd64 variant is where they would start, and whether
  `VZLinuxBootLoader` there takes its kernel is unproven. Revisit on demand.
