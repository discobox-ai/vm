# vz driver (macOS host, macOS guest): implementation brief

**Scope:** `pkg/machine/vz` and the darwin-specific parts of `pkg/guest`.
Nothing above the `machine.Driver` seam should need to change. If it does, that
is a design finding: raise it rather than work around it.

**Done means:**

1. `machinetest.Run` passes against `vz.Driver` on an Apple silicon Mac.
2. `internal/e2e` passes with `DISCO_VM_DRIVER=vz`.
3. `examples/macos.yaml` builds.

Add `pkg/machine/vz/vz_test.go`, gated on an env var naming an installed base
layer (so CI skips it), that calls `machinetest.Run` with `Config.Base`, so the
20 GB IPSW install isn't redone on every run.

**Prior art:** discobox's proof of concept, branch `poc/macos-guest`:

- `server/internal/macvm/macvm.go`: bundle, options, validation
- `server/internal/macvm/macvm_darwin.go`: install, run, save/restore, clone,
  and shutdown over vsock
- `server/cmd/discobox-macvm`: the CLI
- `pool-agent/vsock/transport_darwin.go`: the guest vsock listener (already
  ported to `pkg/guest/listen_unix.go`)

Port the behavior, not the structure.

## Build and signing

- The bindings are cgo: `github.com/Code-Hex/vz/v3`, **replaced by discobox's
  fork**, which adds macOS 27 guest provisioning and fixes socket descriptor
  ownership:
  `replace github.com/Code-Hex/vz/v3 => github.com/discobox-ai/vz/v3 v3.7.2-0.20260922054351-c8cee82c6395`
  (take the current pin from discobox's `server/go.mod`).
- Put the real implementation in `//go:build darwin && cgo` files. Keep a
  `darwin && !cgo` stub whose error says to rebuild with `CGO_ENABLED=1`. The
  current `vz_darwin.go` shell becomes that stub.
- Creating a VM requires the `com.apple.security.virtualization` entitlement.
  Add a `Taskfile`/`Makefile` target that codesigns `disco-vm` the way
  discobox's `task sign` does, and make `Check` detect an unsigned binary and
  say so.

## Method by method

**Layer layout.** A macOS guest is four files that belong together: the disk,
aux storage (NVRAM), the hardware model, and the machine identifier (macvm's
`Bundle`). Those four are a layer's payload. A saved state (`state.bin`) plus
the MAC address form the resume half.

**Check:** confirm virtualization is supported, the binary is entitled, and the
host is Apple silicon.

**Install(spec, dst):**

1. Take the restore image: `spec.Media` is a path, or `"latest"`, which means
   `FetchRestoreImage`, cached under the state root. Only one download of about
   20 GB should ever happen.
2. Create the hardware model, machine identifier, aux storage, and a sparse
   disk (raw, or ASIF on macOS 26+), as macvm's `Install` does.
3. Run the installer.
4. **Bake in the agent.** This is the open problem. An IPSW install leaves no
   hook for writing to the Data volume. Options, in order of preference:

   - **(a) Provisioning plus a first-boot bootstrap.** On macOS 27+, boot once
     with `GuestProvisioning` (`spec.Options["username"]` and password,
     `autologin`, `RemoteLogin: true`). Over NAT and SSH, copy `spec.Agent` to
     `/usr/local/libexec/disco-vm` and install
     `/Library/LaunchDaemons/ai.discobox.vm.guest.plist` (root, RunAtLoad,
     KeepAlive, `disco-vm guest`).
   - **(b) Host mount.** `hdiutil attach` the disk image, write the binary and
     plist into the Data volume, and detach. This works when the volume isn't
     encrypted. Verify that before relying on it.

   Either way, finish with an orderly shutdown through the agent (port 7300)
   and commit.

**Prepare(inst):** clone the parent's files into `inst.Dir` with `clonefile`
(APFS, copy-on-write, milliseconds). There are no chains on vz: every layer is
a full clone, and APFS shares the blocks. So Prepare only needs
`inst.Parent()`. Clone modes:

- **cold:** new machine identifier, new MAC, and no state file. The guest cold
  boots.
- **resume:** carry `state.bin`, the machine identifier, and the MAC. See the
  caveats below.

**Boot(inst, opts):** build the configuration as macvm's `buildConfiguration`
does:

- the same devices at install and at boot
- no memory balloon (a device change strands saved states)
- a virtio socket device

Use `RestoreMachineStateFromURL` plus `Resume` for resume mode. Otherwise
`Start`, with provisioning options on the first boot.

`Machine.Dial(port)` is `SocketDevices()[0].Connect(port)`. `Done` comes from
the VM's state-change channel. `Kill` is `Stop()`.

The VM lives in the calling process, which is always the shim. GUI (`opts.GUI`)
needs the main thread, locked (`runtime.LockOSThread` in `main`) with
`StartGraphicApplication` blocking it. So the shim is the process that opens
the window.

**Commit(inst, dst):** the guest has already been shut down through the agent.
Move (rename within the volume) or clonefile the bundle into `dst.Dir`. A
committed layer has no `state.bin` unless the driver deliberately produces a
resume layer.

**Destroy:** remove the bundle.

## Caveats the design already accounts for

- **Two macOS guests at once.** This is enforced by the framework and
  advertised as `MaxRunning{darwin: 2}`. The engine refuses a third, and the
  conformance suite adapts. A build VM counts.
- **The stop request makes macOS sleep, not shut down.** Shutdown goes through
  the agent. `pkg/guest/power.go` already runs `/sbin/shutdown -h now` on
  darwin.
- **Resume is once per identity.** A saved state belongs to the machine
  identifier that wrote it, and two running guests with one identifier is
  undefined. Treat resume clones as single-use until proven otherwise.
- **A restore needs the console session unlocked.** The key that protects saved
  state sits behind the login session, so a locked Mac gives "permission
  denied" on restore. A save still works. Decide whether `Check` or a
  resume-mode `Prepare` should detect this and fall back to cold.
- **Saved state is tied to the host**, and a host OS update can invalidate it.
  A rejected restore must fall back to a cold boot, never fail the run.

## Guest-side work (`pkg/guest`, darwin)

- The vsock listener is ported. Verify it inside a macOS 13+ guest.
- `runAs` (Credential plus HOME/USER/LOGNAME) is implemented for unix. Verify
  it with Homebrew, which checks both the uid and HOME.
- `power.go`'s `shutdown -h now` needs root, which the launchd daemon has.
