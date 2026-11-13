# pkg/guest

The guest agent (`disco-vm guest`), its client, and the shim's relay. Where
the agent listens is in [pkg/machine](../machine/DESIGN.md#ports-not-guids).

## HTTP between host and agent

Where a driver has an agent, the agent protocol is HTTP/1.1
(`pkg/guest/proto.go`), as every discobox guest service already is.
`guest.Client` is that driver's `machine.Guest`, and the shim serves the same
protocol from its machine (`guest.Relay`) to other processes:

| endpoint | purpose |
|---|---|
| `GET /v1/health`, `GET /v1/info` | readiness, OS/arch/hostname |
| `POST /v1/exec` | upgrades to a framed stream: stdin, resize, stdout, stderr, exit |
| `GET`/`PUT /v1/files` | tar out of and into the guest |
| `POST /v1/shutdown` | orderly power-off or reboot |

Errors before response headers carry a machine sentinel's code in
`Disco-Vm-Error-Code`, shared with served's JSON through `machine.ErrorCode`
and `ErrorSentinel`. The client preserves the message and wraps the named
sentinel for ordinary requests and exec refusals before upgrade. Missing or
unknown codes remain message-only errors; older clients ignore the header,
so `Protocol` stays unchanged. Errors after streaming starts remain stream
errors. Relay health calls the machine's `Info`, so an unreachable or stale
machine does not read as healthy.

The guest, not the hypervisor, powers itself off: the agent where there is
one, systemd on boxd. A vz stop request reaches macOS as a power-button press,
and macOS answers it by sleeping. HCS has no graceful path for a stock Windows
guest at all. So Kill is only the fallback.

The agent is a boot-start service (SYSTEM on Windows, root launchd daemon on
macOS). It is up before any user logs on, and a command can drop to a user
with `exec -u`. On Windows it builds each process's environment from the registry
(`CreateEnvironmentBlock`), so a toolchain one `exec` installs is on
PATH for the next.

`/v1/info` reports `guest.Protocol`, the protocol's own version, which moves
whenever a host and an agent on either side of a change cannot work
together. A driver that hands the guest its agent at every boot (vz's Linux
guests) refuses an agent that reports another (`guest.CheckProtocol`). It is
not the build's version, which is the main module's: an embedder's in a
program that embeds the engine.

## The Linux guest image

`pkg/guest/linux` is disco-vm's Linux base (ADR 26-10-31-990): a Dockerfile
whose final `FROM scratch` stage is the artifact set `vmlinux`
(uncompressed), `initrd.img`, and `root.ext4` (raw), for `linux/arm64` (vz)
and `linux/amd64` (hcs; an Intel Mac). It is discobox's `vm-image` pipeline
with disco-vm's content:

- **Debian sid, its kernel too**, at the Taskfile's snapshot (the root
  [DESIGN.md](../../DESIGN.md#debian-is-sid-at-one-snapshot)): vz and hcs boot
  sid's `linux-image-<arch>` (7.3 at the snapshot), and the root's apt reads
  live sid. libkrun boots libkrunfw's kernel instead.
- **A general Debian machine** with systemd, nftables, and nothing else: no
  Docker, nothing of discobox's. A caller shapes it through exec and copy, as
  discobox applies its pool-agent bundle (discobox ADR 26-10-23-551).
- **nftables**, so a caller can fence the guest's network before it comes
  up (discobox ADR 26-11-11-963 §6): the `nft` tool, the kernel's nf_tables
  with connection tracking (a module of Debian's kernel, built into
  libkrun's), and Debian's `nftables.service` enabled, which loads
  `/etc/nftables.conf` before `network-pre.target` on every boot. The image
  keeps Debian's own `nftables.conf`, which admits everything; a caller
  saves its rule set there. The unit only wants `network-pre.target`, so a
  rule set that fails to load leaves the network up with no fence: a caller
  reads it back, and orders what depends on it after `nftables.service` with
  a `Requires=` of its own (discobox ADR 26-10-31-206 §1).
- **No agent baked in.** A boot appends the host's agent to the initrd. The
  init-bottom hook `disco-vm-agent` moves it to `/run/disco-vm`, a tmpfs of
  its own because initramfs-tools mounts `/run` noexec and the root keeps
  that mount, with the `cp` and `chmod` a build hook puts in the initrd, since
  one with no busybox has neither; `disco-vm-guest.service` runs it.
- **A writable root of the instance's own**, grown online into its disk
  (`x-systemd.growfs` on `/`), with an empty `/etc/machine-id` that the
  command line's `systemd.machine_id` fills.
- **DHCP by MAC** on `en* eth*`, and `systemd-networkd-wait-online` enabled
  but skipped when the command line says `disco-vm.nic=0`, so a guest with no
  network device reaches `network-online.target` at once instead of failing
  it 30 seconds in.
- **The clock** stepped from the PL031 RTC every 30 seconds where there is
  one, since a Mac that slept leaves the guest behind.
- **Hardware** is virtio and Hyper-V's synthetic devices: whole driver
  classes neither can attach are deleted, `drivers/scsi` (hv_storvsc) kept,
  and the initrd built from a module list, compressed with zstd, which both
  Debian's kernel and libkrun's unpack.

`assemble` builds the artifact set inside the image build (`mkfs.ext4 -d`
needs no loop device or privilege), extracting the amd64 ELF from its
bzImage and decompressing an arm64 Image only where Debian ships one
compressed, and leaving no kernel in the root (Debian's 7.x packages keep one
beside the modules as well as in `/boot`). `verify` then checks the kernel's
form, that the root carries none, the root's label, units,
fstab, and nftables, and the initrd's hook, tools, and compression, so a broken image fails
its build rather than booting to a blank console. `go tool task build:linux-guest`
builds it for this host (CI builds amd64 on every change), and a `linux/vN`
tag publishes both architectures as `ghcr.io/discobox-ai/disco-vm-linux:vN`
(`release:linux-guest`), whose digest is pinned in `vz.DefaultLinuxBase` by
hand: the image ships on its own line, so a guest fix needs no disco-vm
release. The command line, the hook's path, and the unit are a contract
between the two: a change ships the base first, then the pin.
