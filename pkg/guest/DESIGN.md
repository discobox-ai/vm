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

- **A general Debian machine** with systemd and nothing else: no Docker,
  nothing of discobox's. A caller shapes it through exec and copy, as
  discobox applies its pool-agent bundle (discobox ADR 26-10-23-551).
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
  and the initrd built from a module list.

`assemble` builds the artifact set inside the image build (`mkfs.ext4 -d`
needs no loop device or privilege), extracting the amd64 ELF from its
bzImage and decompressing an arm64 Image only where Debian ships one
compressed. `verify` then checks the kernel's form, the root's label, units,
and fstab, and the initrd's hook and tools, so a broken image fails its build
rather than booting to a blank console. `go tool task build:linux-guest`
builds it for this host (CI builds amd64 on every change), and a `linux/vN`
tag publishes both architectures as `ghcr.io/discobox-ai/disco-vm-linux:vN`
(`release:linux-guest`), whose digest is pinned in `vz.DefaultLinuxBase` by
hand: the image ships on its own line, so a guest fix needs no disco-vm
release. The command line, the hook's path, and the unit are a contract
between the two: a change ships the base first, then the pin.
