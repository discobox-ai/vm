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
4. **The account moves off uid 501.** Provisioning always makes the first
   account 501, which is also the first account on the host, so the bootstrap
   moves it to `uid` (default 600) and chowns its files. That frees 501 for a
   warm stage to give to the host's user. It keeps its SecureToken, so it
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
  the install account, creating a warm stage's user) goes over SSH as the
  install account with `sudo`, whose password the layer records.

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
- **resume:** check that the stage in `inst.WarmDir` has a template
  (`machine.ErrNotWarm` otherwise) and mark the instance `resume.pending`. The
  template is taken by the first boot, in the process that will hold the VM.

**Boot(inst, opts):** one configuration for install, every boot, and every
save and restore: the Mac platform, virtio block, NAT with the bundle's MAC,
the Mac framebuffer (always attached, so opening a window never changes the
hardware), keyboard and pointer, a virtio socket device, and optional virtiofs
shares. There is **no memory balloon**: a macOS guest ignores its target, and
any device added later strands every saved state. A pending resume clone locks a
template that no running VM holds (`flock` on its `lock`, held until the VM
stops, and by the process, so a dead shim frees it), clones its files into the
instance, and restores it at the size it was saved at. If no template is free,
or the framework refuses the restore, the instance becomes a cold clone of the
image instead. A resumed clone still has its template's identity, so its next
boot first gives it a new machine identifier and MAC. `Dial` is the socket device's `Connect`, with the caller's
deadline enforced around it, and `Listen` (`machine.HostListener`, for an
instance's forwards) is the socket device's `Listen`: a guest process connects
to the host at CID 2. Connections both ways can half-close, which a request
piped in and its answer read back need; the bindings keep the connection's
Unix-domain socket unexported, so `CloseWrite` reaches it by reflection and
falls back to a full close if their layout changes. `Done` follows the VM's state channel. `Kill` is
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

- **A stage is two templates** under `<stage>/templates/`, each a complete
  bundle: disk, identity, MAC, and saved state. A saved state restores only
  against the disk as it stood when it was saved, and only under the identity
  it was saved with: a clone given a new machine identifier, a new MAC, or
  both, is refused with "invalid argument" (measured). A template is never
  used up, since every resume clone is an APFS clone of one. Two clones of one
  template must not run at once (a shared MAC is a shared address on the NAT,
  and a shared identifier is undefined behavior), and the framework runs at
  most two macOS guests, so two templates always leave one free.
- **Warm** stages the two templates one at a time (`spec.Count` does not
  apply): clone the image with a new identity, boot it cold at the stage's
  size, wait for the agent, close every host connection, pause, save, and stop
  while paused. Each is written under a temporary name and renamed into place.
  That took about 30 seconds each.
- **A user** (`WarmSpec.User`, `disco-vm warm --user NAME[:UID]` or
  `--local-user`): before staging, Warm boots the image once and, over SSH,
  creates the account (an administrator with passwordless sudo and a random
  password recorded in the stage), sets it to log in at boot (`/etc/kcpassword`
  and loginwindow's `autoLoginUser`), and hides the install account. It then
  boots twice more, because the user's first login marks it for Setup
  Assistant's first-login setup (`MiniBuddyLaunch` in the user's loginwindow
  preferences), and every template would resume into Setup Assistant: the
  first boot quits Setup Assistant (which sets the mark again while it runs)
  and clears the mark, and the second checks for a plain desktop. Clearing it
  at creation does not stick, since the login sets it. Staging a template
  refuses to save one with Setup Assistant running. A base records how it was
  made (`loginVersion`), so a stage made an older way is made again. That
  bundle is the stage's `base/`, kept for topping up, and each template is
  cloned from it and saved only once the user owns the console and Finder is up. So a
  resumed clone opens in that user's session. A cold clone of the image does
  not have the user.
- **Warmth** reports any number (-1) while a template's recorded host build
  matches this host's (a host update invalidates saved states), and zero, with
  `Held` saying why, while the screen is locked, so that auto mode clones cold.
- **Cool** removes the stage.

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
