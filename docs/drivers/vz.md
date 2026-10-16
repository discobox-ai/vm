# vz driver (macOS host, macOS guest)

**Scope:** `pkg/machine/vz` and the darwin-specific parts of `pkg/guest`.
Above the `machine.Driver` seam, the driver needed only neutral additions:
`InstallSpec.CacheDir` (see Install), and for windows `BootOptions.Title`,
`InstallSpec.GUI`, and `machine.Main` (see GUI).

**Done means:**

1. `machinetest.Run` passes against `vz.Driver` on an Apple silicon Mac
   (`go tool task test:vz`).
2. `internal/e2e` passes with `DISCO_VM_DRIVER=vz` (`go tool task test:e2e:vz`).
3. `docs/examples/macos.yaml` builds.

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

| file | layer | instance |
|---|---|---|
| `disk.img` | ✓ | ✓ |
| `aux.img` (NVRAM) | ✓ | ✓ |
| `hardwaremodel` | ✓ | ✓ |
| `vz.json` (version, size, account) | ✓ | ✓ |
| `machineidentifier` | | its own |
| `macaddress` | | its own, from its first boot |

There are no chains. Every bundle is a full APFS clone (`clonefile`) of its
parent, so a driver only ever reads a chain's last layer. A clone across
volumes is an error that says so, never a silent 100 GiB copy.

## Method by method

**Check:** Apple silicon, and an entitled binary.

**Install(spec, dst):**

1. The restore image is `spec.Media`, or for `latest` the newest one this Mac
   supports, downloaded once into `spec.CacheDir` under the name Apple gives
   it (`UniversalMac_27.0_26A428_Restore.ipsw`). A partial download is
   resumed, and the image is renamed into place only once it is complete. The
   builder sets `CacheDir` to `<root>/cache/<driver>`.
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
   `/usr/local/libexec/disco-vm`, and installs
   `/Library/LaunchDaemons/ai.discobox.vm.guest.plist` (root, RunAtLoad,
   KeepAlive, `disco-vm guest`, logging to `/var/log/disco-vm-guest.log`). It
   also sets `pmset -a sleep 0`, because a sleeping guest's agent answers
   nothing. Then it waits for the agent on vsock 7300 and shuts down through
   it. That took 45 seconds.
4. **The account moves off uid 501.** Provisioning always makes the first
   account 501, which is also the first account on the host, so the bootstrap
   moves it to `uid` (default 600) and chowns its files. That freed 501 for
   the host's user in a warm stage; disco-vm makes no such account since warm
   stages were removed (ADR 26-10-15-851), and one made by a build step or
   `exec` gets another uid (see below). It keeps its SecureToken, so it
   stays the disk's volume owner.
5. Options (`from.install.options`, with build args substituted): `username`
   (default `admin`), `password` (default random, recorded in the layer's
   `vz.json`, which is private to the owner), `fullname`, `uid` (default 600),
   `autologin` (default true), and `disk-format`. An unknown key is an error.

Learned the hard way:

- **Only SSH may change an account.** macOS lets only a session with Full
  Disk Access write a user record. The agent's launchd daemon has none, so
  from it `dscl` fails with `-14120` and `sysadminctl -addUser -UID` silently
  picks another uid; SSH has it. So everything that touches accounts (moving
  the install account) goes over SSH as the
  install account with `sudo`, whose password the layer records.

- **A new guest restarts itself during first-boot setup, and to the framework
  a restart is a stop.** A guest that stops cleanly before the agent is in is
  booted again, with provisioning asked for again. A guest that has already
  applied it ignores it.
- **A VM holds its aux storage locked until the VM object is freed, not merely
  stopped.** The bindings free it from a finalizer, so booting a bundle again
  in the process that last booted it fails with "Failed to lock auxiliary
  storage" (EAGAIN) until a garbage collection. Examples are the install's
  provisioning boot and a build's `reboot` step. A start retries
  behind `runtime.GC()` with a **new VM object each time**: a VM whose start
  failed goes straight to the error state if it is started again.

**Prepare(inst):** clone the parent layer's files, then write a new machine
identifier. The MAC is created on the first boot.

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

**GUI** (`BootOptions.GUI`, `InstallSpec.GUI`; design decision 3a): after the
VM starts, the driver opens an `NSWindow` whose content is a
`VZVirtualMachineView` on it, capturing system keys and resizing the guest's
display to the window. It is our own window (`window_darwin.go`), not the
bindings' `StartGraphicApplication`, because theirs calls `[NSApp terminate:]`
when its VM stops, which ends the process: a build boots several VMs in a row,
and a shim must outlive its window. AppKit is started lazily on the main thread
that `machine.Main` lends. The app refuses to quit, since quitting would take
every VM in the process down with it. With `build --gui` an install shows the installer VM too, and closes that
window when the installer finishes, before the first boot. The window closes
when the VM stops,
which also releases the view's hold on the VM, so a build's `reboot` step can
lock the bundle again. The bindings don't export the `VZVirtualMachine` behind
a `vz.VirtualMachine`, so `vmObject` reads it by reflection, and the C side
checks its class before using it. The clean fix is an exported accessor in
discobox's fork.

**Commit(inst, dst):** rename the stopped instance's layer files into
`dst.Dir`. The identifier and MAC stay behind.

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

## Measured on real hardware

- **Two macOS guests at once** is the framework's limit, advertised as
  `MaxRunning{darwin: 2}`. Every suite here keeps to it.
- **The framework's stop request makes macOS sleep**, so shutdown always goes
  through the agent (`/sbin/shutdown -h now`, as root).

## Guest side (`pkg/guest`, darwin)

- The vsock listener (`listen_unix.go`) works in a macOS 27 guest. It is how
  every test here reaches the agent.
- A launchd daemon's PATH is only the system's. So the agent rebuilds each
  process's PATH the way `path_helper` does, from `/etc/paths` and
  `/etc/paths.d`, and a toolchain one build step installs there is on PATH for
  the next (`env_darwin.go`). This mirrors the Windows agent reading the
  registry.
- `runAs` with Homebrew, which checks both the uid and HOME, is not verified
  yet.
