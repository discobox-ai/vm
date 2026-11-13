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
3. **The agent goes in on the first boot**, over SSH. An IPSW install leaves
   no way to write to the Data volume, and before macOS 27 the first boot is
   Setup Assistant, which needs a person at a window. So Install refuses a
   restore image older than 27. The first boot is started with guest
   provisioning (the account, auto-login, Remote Login on). The driver finds
   the guest's address in the NAT's DHCP leases (`/var/db/dhcpd_leases`, by
   MAC), logs in with `x/crypto/ssh`, copies `spec.Agent` to
   `/usr/local/libexec/disco-vm-agent`, writes the agent stub to
   `/usr/local/libexec/disco-vm`, and installs
   `/Library/LaunchDaemons/ai.discobox.vm.guest.plist` (root, RunAtLoad,
   KeepAlive, `disco-vm guest`, logging to `/var/log/disco-vm-guest.log`). It
   also sets `pmset -a sleep 0`, because a sleeping guest's agent answers
   nothing. Then it waits for the agent on vsock 7300 and shuts down through
   it. That took 45 seconds.

   **Every boot hands the guest its agent on a disk** (issue #83, discobox
   ADR 26-11-11-963 §9), the macOS twin of the agent a Linux guest's boot
   initrd carries: a Mac's boot loader is the platform's own, so there is no
   initrd to append to. The shim writes `agent.iso` into the instance's
   bundle, an ISO 9660 image named `DISCO_VM_AGENT` holding its own
   executable (`hdiutil makehybrid`, about a second), and attaches it as a
   read-only virtio disk after the guest's own. A commit drops it, as it does
   everything but the layer's files. diskarbitrationd mounts it at
   `/Volumes/DISCO_VM_AGENT` before launchd starts the daemon, or within a
   second or two, with no one logged in: a virtio disk is internal. The
   daemon's program is the stub, a short `sh` script: it waits up to 10 s for
   a read-only `cd9660` mount at that path, copies the agent to
   `disco-vm-agent` unless the copy is already the same (`cmp`), and `exec`s
   it; with no disk it runs the copy it made last. launchd's own program never
   changes, so a new agent meets no launch constraint (a replaced program was
   refused its first start, `OS_REASON_CODESIGNING`, and started again 10 s
   later). A shim that is not disco-vm (its Go build info names another main
   package) has no agent to hand: its boot attaches no disk, the guest runs
   the agent it last had, and one of another protocol is refused, naming why.

   A guest installed before the stub runs the agent it was installed with at
   `disco-vm`, which a committed VM and every VM created from it keep. Once
   its agent answers, Boot asks it which program it runs (`ps` of the exec's
   parent) and what `disco-vm` holds; one not on this stub is put on it with
   only what every agent serves: a file put of `disco-vm.stub` beside it, and
   an exec that keeps a Mach-O agent there as `disco-vm-agent` (the stub's
   copy until a disk hands it another), moves the stub into place, and kills
   the daemon (`launchctl kill SIGKILL`). launchd's KeepAlive starts the stub,
   which runs the disk's agent. Boot returns once the agent answers from
   `disco-vm-agent`, or fails within two minutes, and then checks its
   protocol as every boot does (`guest.CheckProtocol`). A later change to the
   stub changes its mark (`# disco-vm agent stub N`, its second line), and a
   boot puts the guest on the new one the same way.
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
platform, virtio block, NAT with the bundle's MAC, the Mac framebuffer (always
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
need; the bindings keep the connection's Unix-domain socket unexported, so
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
  panic=5 systemd.machine_id=<id>`, plus `disco-vm.nic=0` when the instance
  is fenced to nothing; virtio block, a socket device, the serial console
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
