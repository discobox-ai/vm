# vz driver (macOS host, macOS and Linux guests)

**Scope:** `pkg/machine/vz` and the darwin-specific parts of `pkg/guest`.
Above the `machine.Driver` seam, the driver needed only neutral additions:
for windows `BootOptions.Title`, `InstallSpec.GUI`, and `machine.Main` (see
GUI). `vz.Hypervisor` is a `local.Hypervisor`: `pkg/machine/local` keeps its
VMs, committed or not (ADR 26-11-03-606), in its directory (`--root`,
`$DISCO_VM_ROOT`, or `~/Library/Application Support/disco-vm`), gives it a
cache there for media and Linux guest images (`<root>/cache/vz`, see Install
and [Linux guests](#linux-guests)), and runs each started VM under a shim,
since a VM dies with the process that made it (ADR 26-10-23-417).

**Done means:**

1. `machinetest.Run` passes against the vz driver on an Apple silicon Mac
   (`go tool task test:vz`: `DISCO_VM_VZ_BASE` names a committed macOS VM
   that `disco-vm install` made, and `DISCO_VM_TEST_BINARY` the signed
   disco-vm each started VM's shim runs).
2. `internal/e2e` passes with `DISCO_VM_DRIVER=vz` (`go tool task test:e2e:vz`).
3. `disco-vm install NAME` makes a committed macOS VM (media `latest`), and a
   VM created from it runs, is shaped through `exec`, and is committed.
4. A Linux guest boots beside a macOS one, with no network device, and its
   caller reaches a vsock port in it (issue #27): `go tool task test:vz` with
   `DISCO_VM_VZ_LINUX_BASE` naming the base fresh Linux VMs start from
   (`default`, a guest image's reference, or a directory) runs
   `TestConformanceLinux`, and with `DISCO_VM_VZ_BASE` too,
   `TestLinuxBesideMacOS`.
5. A guest fenced to nothing has no route off the machine on either OS, and
   is reached only over vsock and its `Forwards` (issue #5):
   `TestNoRouteOffTheMachine`, in `go tool task test:vz`, runs on each OS
   whose base is named.

Where each stands is in the proof table in [DESIGN.md](../../DESIGN.md).

**Prior art:** discobox's proof of concept, `server/internal/macvm` and
`server/cmd/discobox-macvm` (commit a6bedda3). The behavior is ported; the
structure is not.

## Build and signing

- The bindings are cgo: `github.com/Code-Hex/vz/v3`, replaced by discobox's
  fork, which adds macOS 27 guest provisioning and fixes socket descriptor
  ownership. Keep the pin in `go.mod` in step with discobox's `server/go.mod`.
- The driver is the `darwin && cgo` files. A `darwin && !cgo` build (any cross
  compile) gets `stub_darwin.go`, whose every method says to rebuild with
  `CGO_ENABLED=1`.
- Creating a VM needs the `com.apple.security.virtualization` entitlement.
  `go tool task build` builds and ad-hoc signs `build/disco-vm` with
  `cmd/disco-vm/disco-vm.entitlements`. Tests that create VMs run through
  `scripts/codesign-exec`, a `go test -exec` wrapper that signs the test
  binary. `Check` asks the process for its own entitlement
  (`SecTaskCopyValueForEntitlement`), so an unsigned binary is named as the
  cause up front instead of failing deep in the framework.

## Layout

A macOS guest is a **bundle**: files that belong together in one directory.

| file | committed VM | VM |
|---|---|---|
| `disk.img` | ✓ | ✓ |
| `aux.img` (NVRAM) | ✓ | ✓ |
| `hardwaremodel` | ✓ | ✓ |
| `vz.json` (version, size, account) | ✓ | ✓ |
| `machineidentifier` | | its own |
| `macaddress` | | its own, from its first boot |
| `agent.iso` | | made by each boot ([The macOS agent](#the-macos-agent)) |

There are no chains (`Chains` is false). A VM created from a committed VM is
a full APFS clone (`clonefile`) of its layer files (`layerFiles`), and a
commit freezes the VM's bundle in place, so each bundle's files are its own
and removing a committed VM never touches a VM created from it. A clone
across volumes is an error that says so, never a silent 100 GiB copy.

## Method by method

**Check:** Apple silicon, and an entitled binary.

**Install(spec, dst):** a committed macOS VM, in `dst.Dir`, the VM's own
directory:

1. The restore image is `spec.Media`, or for `latest` the newest one this Mac
   supports, downloaded once into `spec.CacheDir` under the name Apple gives
   it (`UniversalMac_27.0_26A428_Restore.ipsw`). Virtualization.framework
   names the image; the driver downloads it through `Config.HTTPTransport()`,
   so an embedder's dialer dials it. A partial download is resumed, started
   again if the server answers a range with the whole file, and the image is
   renamed into place only once it is complete. The local driver sets
   `CacheDir` to `<root>/cache/vz`. The image's macOS version and build are
   what Install returns as the committed VM's `Instance.Release` (`27.0.0
   (26A428)`), the RELEASE column of `disco-vm ls -a` (issue #6). A VM
   installed before Install returned it has it too: the local driver fills a
   record that lacks one from the bundle's `vz.json`, which every clone copies
   (`Hypervisor.Release`).
2. The hardware model is the restore image's. The machine identifier, the aux
   storage, and a disk are created (raw by default, `disk-format: asif` for
   ASIF), and the installer runs. That took 3 minutes on an M-series Mac.
3. **The agent's launch daemon goes in on the first boot**, over SSH. An
   IPSW install leaves no way to write to the Data volume, and before macOS
   27 the first boot is Setup Assistant, which needs a person at a window. So
   Install refuses a restore image older than 27. The first boot is started
   with guest provisioning (the account, auto-login, Remote Login on) and,
   as every boot, the agent's drive ([The macOS agent](#the-macos-agent));
   the installer before it has no drive. The driver finds the guest's
   address in the NAT's DHCP leases (`/var/db/dhcpd_leases`, by MAC), logs
   in with `x/crypto/ssh`, and installs
   `/Library/LaunchDaemons/ai.discobox.vm.guest.plist`, whose program is
   `/bin/sh` with the stub. It also sets `pmset -a sleep 0`, because a
   sleeping guest's agent answers nothing. Then it waits for the agent the
   stub runs off the drive on vsock 7300, and shuts down through it. Install
   refuses to start without a valid `macosAgent`, and does not read
   `spec.Agent`. With the agent baked in, this step took 45 seconds; with
   the drive it is not timed yet.
4. **The account moves off uid 501.** Provisioning always makes the first
   account 501, which is also the first account on the host, so the bootstrap
   moves it to `uid` (default 600) and chowns its files. That freed 501 for
   the host's user in a warm stage; disco-vm makes no such account since warm
   stages were removed (ADR 26-10-15-851), and one made by `exec` gets
   another uid (see below). It keeps its SecureToken, so it
   stays the disk's volume owner.
5. Options (`disco-vm install --option KEY=VALUE`): `username` (default
   `admin`), `password` (default random, recorded in the VM's `vz.json`,
   which is private to the owner), `fullname`, `uid` (default 600),
   `autologin` (default true), and `disk-format`. An unknown key is an error.
   `--disk` is required.

Learned the hard way:

- **Only SSH may change an account.** macOS lets only a session with Full
  Disk Access write a user record. The agent's launchd daemon has none, so
  from it `dscl` fails with `-14120` and `sysadminctl -addUser -UID` silently
  picks another uid; SSH has it. So everything that touches accounts (moving
  the install account) goes over SSH as the
  install account with `sudo`, whose password the VM's `vz.json` records.

- **A new guest restarts itself during first-boot setup, and to the framework
  a restart is a stop.** A guest that stops cleanly before the agent is in is
  booted again, with provisioning asked for again. A guest that has already
  applied it ignores it.
- **A VM holds its aux storage locked until the VM object is freed, not merely
  stopped.** The bindings free it from a finalizer, so booting a bundle again
  in the process that last booted it fails with "Failed to lock auxiliary
  storage" (EAGAIN) until a garbage collection. Examples are the install's
  provisioning boot after its first. A start retries
  behind `runtime.GC()` with a **new VM object each time**: a VM whose start
  failed goes straight to the error state if it is started again.

**Prepare(inst):** clone the committed VM's layer files, then write a new
machine identifier. The MAC is created on the first boot. A fresh VM (no
`Chain`) is a Linux guest from the base ([Linux guests](#linux-guests)).

**Boot(inst, opts):** one configuration for install and every boot: the Mac
platform, virtio block for the disk and then the agent's drive (read-only;
see [The macOS agent](#the-macos-agent)), both cached with fsync (see
[Disks](#disks)), NAT with the bundle's MAC, the Mac framebuffer (always
attached, so opening a window never changes the hardware), keyboard and
pointer, a virtio socket device, and optional virtiofs shares. There is **no
memory balloon**: a macOS guest ignores its target. Every boot starts from
power-off. A resume clone from before resume clones were removed (ADR
26-10-15-851) is handled as its own: one that resumed once still has its
template's identity, so its next boot first gives it a new machine identifier
and MAC, and one never booted (`resume.pending`) is refused. `Dial` is the
socket device's `Connect`, with the caller's deadline enforced around it, and
`Listen` (`machine.HostListener`, for an instance's forwards) is the socket
device's `Listen`: a guest process connects to the host at CID 2. Connections
both ways can half-close, which a request piped in and its answer read back
need; once the agent answers, a guest whose `guest.Protocol` is not this
host's is killed and refused, on either OS; the bindings keep the connection's Unix-domain socket unexported, so
`CloseWrite` reaches it by reflection and falls back to a full close if their
layout changes. `Done` follows the VM's state channel. `Kill` is `Stop`.

**GUI** (`BootOptions.GUI`, `InstallSpec.GUI`; [a window per boot](../../pkg/machine/DESIGN.md#a-window-is-per-boot-and-lives-with-the-vm)): after the
VM starts, the driver opens an `NSWindow` whose content is a
`VZVirtualMachineView` on it, capturing system keys and resizing the guest's
display to the window. It is our own window (`window_darwin.go`), not the
bindings' `StartGraphicApplication`, because theirs calls `[NSApp terminate:]`
when its VM stops, which ends the process: an install boots several VMs in a
row, and a shim must outlive its window. AppKit is started lazily on the main
thread that `machine.Main` lends. The app refuses to quit, since quitting
would take every VM in the process down with it. With `install --gui` the
installer VM shows too, and its window closes when the installer finishes,
before the first boot. The window closes when the VM stops, which also
releases the view's hold on the VM, so the next boot can lock the bundle
again. The bindings don't export the `VZVirtualMachine` behind
a `vz.VirtualMachine`, so `vmObject` reads it by reflection, and the C side
checks its class before using it. The clean fix is an exported accessor in
discobox's fork.

**Commit(inst):** freeze the stopped VM's bundle in place: its layer files
stay, and everything else goes, the identifier and MAC among them. The end of
Install freezes the same way.

**Destroy:** remove the bundle.

**No fast clones** (ADR 26-10-15-851). Two saved templates per image, each
resuming any number of APFS clones, were built here and removed with warm
stages; they are in the history before that ADR. What they measured, for a
return: a resumed clone's agent answered 3.5 s after Boot against about 28 s
cold; a saved state restores only against the disk as it was saved, under
the same machine identifier and MAC ("invalid argument" otherwise), at the
same memory size, on the same host build; and a restore needs the console
session unlocked ("permission denied" on a locked Mac, while a save still
works).

## The macOS agent

ADR 26-11-13-318. A macOS guest keeps no agent: every boot, the install's
first among them, attaches the host's on a drive, so a VM made from any
base runs the host's agent from its next boot, and an agent change needs no
reinstall.

- **The agent** is vz's privileged `macosAgent` field
  (`--vz-macos-agent`, `$DISCO_VM_MACOS_AGENT`): a disco-vm built for darwin
  on this Mac's architecture, which a boot refuses unless it is a Mach-O
  executable for it, naming the field. The command line sets the field to
  its own executable (`ConfigField.DefaultExecutable`); an embedder whose
  program is not disco-vm names one. A shim is given it in the driver's
  configuration.
- **The drive** is an ISO 9660 image with Rock Ridge names and modes, label
  `DISCO_VM_AGENT`, holding `disco-vm` (0755) and `disco-vm.sha256`, its
  digest in hex. It is built in Go (`writeISO`), with every timestamp fixed,
  so the same agent gives the same bytes, and kept in
  `<root>/cache/vz/agent/<the agent's SHA-256>.iso`, beside a
  `<name>.sha256` of the image itself; an image that does not match it is
  built again. Each boot clones it (`clonefile`) into the bundle as
  `agent.iso` and attaches that, read-only, after the disk, which stays the
  boot disk. A commit never keeps it.
- **The base** holds only the launch daemon `ai.discobox.vm.guest` (root,
  RunAtLoad, KeepAlive, logging to `/var/log/ai.discobox.vm.guest.log`),
  whose program is `/bin/sh -c` with the stub inline (`daemonPlist`), so no
  executable of disco-vm's is on the disk. The stub waits up to a minute for
  the drive at `/Volumes/DISCO_VM_AGENT`, read-only `cd9660`, asking
  `diskutil mount readOnly` for it each second where Disk Arbitration has
  not mounted it; copies `disco-vm` into `/var/run/disco-vm` (0700), checks
  it against the drive's digest, unmounts the drive, and execs the copy as
  `disco-vm guest`. Any failure exits, and launchd starts it again.
- **A base installed before** runs `/usr/local/libexec/disco-vm`, the agent
  it was installed with, and ignores the drive. It works while its protocol
  is the host's; once that moves, the boot refuses it, saying to install it
  again. Nothing moves it forward.
- **Signing:** the agent is ad hoc signed, as Go's linker signs every
  darwin/arm64 executable, and needs no notarization, entitlement, or TCC
  grant: nothing copied off a local volume by a root shell is quarantined.
- **Not yet booted on a real guest** (the proof table's row): this stub,
  this Go-built image, its `diskutil mount`, automatic login off, and a
  protocol mismatch refused. A spike's hdiutil ISO and a stub much like
  this one did boot (2026-11-13, macOS 27.0 guest): Disk Arbitration had
  mounted the drive before launchd first ran the stub, or 1.2 s later, with
  no one logged in; the drive may take `disk0`, pushing the boot disk to
  `disk1`, so nothing assumes the boot disk's number.

## Linux guests

ADR 26-10-31-990. A Linux guest is a bundle too, booted directly, not
through EFI:

| file | committed VM | VM |
|---|---|---|
| `vmlinux` (uncompressed) | ✓ | ✓ |
| `initrd.img` | ✓ | ✓ |
| `disk.img` (raw ext4, the root) | ✓ | ✓ |
| `vz.json` (`guestOS: linux`, size) | ✓ | ✓ |
| `machineid`, `macaddress` | | its own, from Create |
| `boot-initrd.img`, `console.log` | | made by each boot |

`vz.json`'s `guestOS` says which kind a bundle is; one from before Linux
guests has none and is macOS.

- **A base, not media** (ADR 26-11-03-606). A fresh VM is a Linux guest
  from the driver's one base, `machine.Config.Base`: a registry reference,
  or the absolute path of a directory holding the three artifacts, used as
  it is (how a guest built with `go tool task build:linux-guest` boots
  unpublished); empty is `vz.DefaultLinuxBase`, disco-vm's own image, a
  placeholder tag until its first `linux/v*` release is pinned by digest.
  The driver refuses a base that is neither when it is made. Nothing lists
  bases. Install of a Linux guest is refused (`Capabilities.Install` is
  darwin alone).
- **Prepare** of a fresh VM pulls the base's image with go-containerregistry,
  with no daemon, refusing one whose platform is not linux on this Mac's
  architecture or that lacks an artifact, into `<root>/cache/vz/linux/<manifest
  digest>` once (a tag the registry cannot answer for runs from its last
  pull, and a digest the cache has needs no network). It clones the kernel
  and initrd, clones `root.ext4` as the VM's `disk.img` and extends it,
  sparse, to 64 GiB, and writes a new machine ID and MAC. A VM created from
  a committed Linux VM clones its layer and gets a new identity the same
  way.
- **Boot** refuses a window, checks the configured agent (`--vz-agent`,
  `$DISCO_VM_AGENT`; ADR 26-11-11-215, which the shim is given) names a linux
  executable of this Mac's architecture (`go tool task build` builds
  `build/linux-arm64/disco-vm` beside the darwin one), and writes
  `boot-initrd.img`: the image's initrd, padded to four bytes, then a newc
  archive holding the agent at `/.disco-vm/disco-vm`. The configuration is
  `VZLinuxBootLoader` with `console=hvc0 root=/dev/vda rw rootfstype=ext4
  panic=5 nmi_watchdog=panic,1 systemd.machine_id=<id>`, plus
  `disco-vm.nic=0` when the instance is fenced to nothing (`panic=5` resets
  a panicked kernel, and `nmi_watchdog=panic,1` makes a hard lockup a panic,
  which Debian's kernel leaves a warning, so a wedged guest resets rather
  than reporting as running; soft lockups and RCU stalls stay warnings, since
  the host can deschedule a vCPU past their thresholds; on an Intel Mac,
  Debian's amd64 kernel detects hard lockups through a guest PMU that
  Virtualization.framework may not give, which is unverified); virtio block
  (cached, fsync: see [Disks](#disks)), a socket device, the serial console
  appended to `console.log`, entropy, a balloon, the NAT unless fenced, and
  tagged virtiofs shares. Once the agent answers, a Linux guest whose
  `guest.Protocol` is not this host's is killed and refused.
- **No network device** is `Egress{}`: Create records the fence and Boot
  attaches no NIC, to a Linux or a macOS guest. A Linux guest then has `lo`
  alone. A macOS guest still lists Apple silicon's peripheral interfaces
  (`anpi0` and its `en2`) and the system's `utun` tunnels, none of them
  active or holding an address beyond link-local, and nothing off the machine
  is routed: there is no route to `1.1.1.1`, and neither IPv4 nor IPv6
  reaches out (measured 2026-11-12, macOS 27.0 guest). Its agent, a
  `Dial` through `shim.sock`, and a `Forward` to a peer all go over vsock.
- **No warm stage** is saved or resumed: warm stages were removed (ADR
  26-10-15-851), and every clone boots cold, so there is no saved state whose
  devices have to match a fenced boot.
- **Not counted** against the two macOS guests: `MaxRunning` names darwin
  alone. A Linux guest's size is 2 vCPUs and 2 GiB unless asked.

The base is [pkg/guest/linux](../../pkg/guest/DESIGN.md#the-linux-guest-image).

## Disks

Every writable disk the driver attaches, a macOS guest's and a Linux
guest's, at install and at every boot, is a virtio block device attached
**cached, with fsync synchronization** (`diskDevice`), never the framework's
default of automatic caching with full synchronization. That default
corrupts ext4 on Apple silicon and was traced to guest hard lockups
(lima-vm/lima#1957, crc-org/vfkit#447, applicative-systems/vzvm#5,
NixOS/nixpkgs#557576); Lima, UTM, and vfkit each moved to cached with fsync.
The cost is durability against the host: a guest's flush is an `fsync`,
not the `F_FULLFSYNC` full synchronization issues, so what the guest flushed
survives the shim or the guest dying but not the host losing power or
panicking before the drive's own cache is written.

A macOS guest's agent drive goes through `diskDevice` too, read-only, so
every disk is attached one way; it is never written, so its synchronization
does nothing.

## Measured on real hardware

- **Two macOS guests at once** is the framework's limit, advertised as
  `MaxRunning{darwin: 2}`, which `local` enforces across every pool in the
  root and every process (`machinetest` max-running). Every suite here keeps
  to it.
- **The framework's stop request makes macOS sleep**, so shutdown always goes
  through the agent (`/sbin/shutdown -h now`, as root).

## Guest side (`pkg/guest`, darwin)

- The vsock listener (`listen_unix.go`) works in a macOS 27 guest. It is how
  every test here reaches the agent.
- A launchd daemon's PATH is only the system's. So the agent rebuilds each
  process's PATH the way `path_helper` does, from `/etc/paths` and
  `/etc/paths.d`, and a toolchain one `exec` installs there is on PATH for
  the next (`env_darwin.go`). This mirrors the Windows agent reading the
  registry.
- `runAs` with Homebrew, which checks both the uid and HOME, is not verified
  yet.
