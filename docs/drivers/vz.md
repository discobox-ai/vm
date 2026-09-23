# vz driver (macOS host, macOS guest)

**Scope:** `pkg/machine/vz` and the darwin-specific parts of `pkg/guest`.
Above the `machine.Driver` seam, the driver needed only neutral additions:
`InstallSpec.CacheDir` (see Install), and for windows `BootOptions.Title`,
`InstallSpec.GUI`, and `machine.Main` (see GUI).

**Done means:**

1. `machinetest.Run` passes against `vz.Driver` on an Apple silicon Mac
   (`make test-vz`).
2. `internal/e2e` passes with `DISCO_VM_DRIVER=vz` (`make test-e2e-vz`).
3. `examples/macos.yaml` builds.

Where each stands is in the proof table in [../design.md](../design.md).

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
  `make build` builds and ad-hoc signs `bin/disco-vm` with
  `cmd/disco-vm/disco-vm.entitlements`. Tests that create VMs run through
  `scripts/codesign-exec`, a `go test -exec` wrapper that signs the test
  binary. `Check` asks the process for its own entitlement
  (`SecTaskCopyValueForEntitlement`), so an unsigned binary is named as the
  cause up front instead of failing deep in the framework.

## Layout

A macOS guest is a **bundle**: files that belong together in one directory.

| file | layer | instance | staged clone |
|---|---|---|---|
| `disk.img` | ✓ | ✓ | ✓ (as saved) |
| `aux.img` (NVRAM) | ✓ | ✓ | ✓ |
| `hardwaremodel` | ✓ | ✓ | ✓ |
| `vz.json` (version, size, account) | ✓ | ✓ | ✓ plus the saved size and host build |
| `machineidentifier` | | its own | the one it was saved with |
| `macaddress` | | its own, from its first boot | the one it was saved with |
| `state.bin` | | only until its first boot | ✓ |

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
4. Options (`from.install.options`, with build args substituted): `username`
   (default `admin`), `password` (default random, recorded in the layer's
   `vz.json`, which is private to the owner), `fullname`, `autologin`
   (default true), and `disk-format`. An unknown key is an error.

Learned the hard way:

- **A new guest restarts itself during first-boot setup, and to the framework
  a restart is a stop.** A guest that stops cleanly before the agent is in is
  booted again, with provisioning asked for again. A guest that has already
  applied it ignores it.
- **A VM holds its aux storage locked until the VM object is freed, not merely
  stopped.** The bindings free it from a finalizer, so booting a bundle again
  in the process that last booted it fails with "Failed to lock auxiliary
  storage" (EAGAIN) until a garbage collection. Examples are the install's
  provisioning boot and a build's `reboot` step. Start and restore retry
  behind `runtime.GC()` with a **new VM object each time**: a VM whose start
  failed goes straight to the error state if it is started again.

**Prepare(inst):**

- **cold:** clone the parent layer's files, then write a new machine
  identifier. The MAC is created on the first boot.
- **resume:** take one whole staged bundle from `inst.WarmDir` by renaming it
  into `inst.Dir`. A rename is atomic, so two clones can't take the same one.
  Return `machine.ErrNotWarm` when none is left.

**Boot(inst, opts):** one configuration for install, every boot, and every
save and restore: the Mac platform, virtio block, NAT with the bundle's MAC,
the Mac framebuffer (always attached, so opening a window never changes the
hardware), keyboard and pointer, a virtio socket device, and optional virtiofs
shares. There is **no memory balloon**: a macOS guest ignores its target, and
any device added later strands every saved state. A bundle with `state.bin` is
restored at the size it was saved at, then resumed. The state is consumed
whether or not that works, and a refused restore falls back to a cold boot of
the same bundle. `Dial` is the socket device's `Connect`, with the caller's
deadline enforced around it. `Done` follows the VM's state channel. `Kill` is
`Stop`.

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
`dst.Dir`. The identifier, MAC, and any state stay behind.

**Destroy:** remove the bundle.

**Warm, Warmth, Cool** (`machine.Warmer`, design decision 7):

- **A stage is a pool of complete bundles** under `<stage>/states/`, not a
  pool of `state.bin` files. A saved state only restores against the disk as
  it stood when the state was saved, so each staged clone carries its own
  disk, identity, and MAC. APFS makes that cheap.
- **Warm** stages one clone at a time until `spec.Count` are staged: clone the
  image with a new identity, boot it cold at the stage's size, wait for the
  agent, close every host connection, pause, save, and stop while paused. Each
  clone is written under a temporary name and renamed into place. That took
  about 30 seconds per clone.
- **Warmth** counts the staged bundles whose recorded host build matches this
  host's (a host update invalidates saved states), and reports zero while the
  screen is locked, so that auto mode clones cold.
- **Cool** removes the pool.

## Measured on real hardware

- **Resume works.** With the screen unlocked, the conformance suite's warm
  subtest and e2e's `TestWarm` pass, and a resumed clone's agent answers
  3.5 seconds after Boot is called, against about 28 seconds for a cold boot.
- **A restore needs the console session unlocked.** On a locked Mac a save
  succeeds, a restore fails with "permission denied", and Boot's cold fallback
  brings the staged disk up with the agent answering about 28 seconds after
  Boot is called (`TestResumeOrFallBack`). `Warmth` reads
  `CGSSessionScreenIsLocked` from the console session. A process with no
  console session is not called locked; it relies on the fallback instead.
- **A restore needs the saved configuration exactly**, memory size included.
  The engine already clones a stage only at the stage's size.
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
