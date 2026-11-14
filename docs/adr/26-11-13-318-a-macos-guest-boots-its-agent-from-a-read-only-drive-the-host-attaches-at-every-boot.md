# 26-11-13-318 — A macOS guest boots its agent from a read-only drive the host attaches at every boot, and its base carries only a launchd stub

- **Status**: Accepted
- **Date**: 2026-11-13

Accepted by the owner by merging #111, 2026-11-14.

## Context

A macOS guest on vz gets its agent once, at install. The install's first
boot copies `InstallSpec.Agent` (the engine's `os.Executable()`) over SSH to
`/usr/local/libexec/disco-vm` and installs the launch daemon
`ai.discobox.vm.guest`, which runs it (`pkg/machine/vz/bootstrap_darwin.go`,
`installScript`). The committed VM keeps that binary, and every VM created
from it, committed from those, or upgraded to it keeps it too. Boot does not
check a macOS agent's `guest.Protocol` (`Hypervisor.Boot` checks Linux
guests only), so a base that outlives a protocol change fails later, in
whatever request first meets the difference.

discobox ADR 26-11-11-963 §9 and step 1.9 of the #42 plan
(discobox-ai/discobox `docs/tasks/42-one-vm-pool-shape.md`) asked disco-vm
to check a macOS agent's protocol at boot and move an old one forward without
reinstalling. vm#83 and PR #107 did that by copying the shim's own binary
into the guest through the old agent. It only works when the shim is
disco-vm (an embedder's shim, `discobox-server __vm-shim`, is not), and it
needs the old agent to speak enough of the new protocol to be replaced. On
2026-11-13 the owner chose a different direction: "we are switching to the
"boot agent in attached drive" approach for macOS so we shouldn't need the
move guest agent to host protocol". vm#83 was closed and PR #107 left
unmerged.

The other agent-based drivers already boot the host's agent and keep none:

- vz's Linux guests and libkrun's get the agent appended to the initrd on
  every boot (ADR 26-10-31-990, ADR 26-11-03-530). The base has no agent.
- Docker guests take it from the configured `agent` at Create
  (`pkg/machine/docker`).
- hcs's Windows guests bake it at install, as macOS does today.

On 2026-11-13 macos-worker showed the core of the new approach on a real
macOS guest: a read-only ISO disk attached to the VM, and a stub launchd
daemon that copied the agent off it and ran it.

ADR 26-11-13-679 (Proposed, PR #105) renames the project `discovm`, drops
support for bases made before the rename, and signs the darwin binary ad hoc,
leaving Developer ID and notarization for later. This ADR uses its terms
where they overlap, and depends on neither outcome.

## Decision

### 1. Every boot of a macOS guest attaches the host's agent on a read-only drive

vz attaches a second virtio block device to every macOS boot, including the
install's first boot. It is read-only (`NewDiskImageStorageDeviceAttachment(path, true)`)
and holds an ISO 9660 image with the agent. It comes after the root disk,
which stays the boot disk. The guest runs the agent it finds there. Nothing
in the guest's disk holds an agent, so nothing has to be moved, and every VM,
whatever it was made from, runs the host's agent on its next boot.

The drive is not part of the bundle's layer (`layerFiles`), so a commit never
carries it. It is attached whether or not the VM is fenced to nothing, since
it is not a network device.

### 2. The drive is built once per agent build and named by the agent's digest

- **What is on it**: the agent as `disco-vm` and its SHA-256 as `disco-vm.sha256`,
  with Rock Ridge names and modes, under one fixed volume label (the
  implementation picks the label; it follows ADR 26-11-13-679's naming if
  that ADR lands).
- **How it is built**: in Go, in process, with no host tool (not
  `hdiutil`), so the builder is tested on every OS in CI. Every timestamp
  is fixed, so the same agent always gives the same bytes.
- **Where it is kept**: `<cache>/vz/agent/<sha256 of the agent>.iso`. It is
  written to a temporary file and renamed into place, under the cache's
  owner-only permissions, like the Linux base. A digest already in the
  cache is used as it is. A cached image whose own content digest is not
  the one recorded beside it is rebuilt.
- **How a boot uses it**: each boot clones the cached image into the VM's
  bundle as `agent.iso` (`clonefile`, which costs nothing) and attaches the
  clone. The cache can then be pruned while VMs run, and no two VMs share a
  disk-image file.

The version that matters is the agent build, by digest. The protocol is
checked separately (§5).

### 3. The agent is vz's configured macOS agent

vz's configuration gets a `macos-agent` field: an absolute path to disco-vm
built for darwin on this Mac's architecture. Like the Linux `agent`, it is
privileged (ADR 26-11-11-215) and given to the shim.

- The command line sets it to its own executable, which is disco-vm built
  for darwin/arm64, so a standalone user configures nothing.
- An embedder whose program is not disco-vm pulls the agent by digest, the
  way it already pulls the Linux agent (discobox ADR 26-11-11-963 §10). The
  published agent index, `ghcr.io/discobox-ai/disco-vm-agent:<module version>`,
  gains a `darwin/arm64` platform. The pull (`linuximage.PullAgent`, renamed
  to suit) refuses anything that is not a Mach-O arm64 executable.

vz refuses to boot or install a macOS guest without a valid macOS agent, and
names the field in the error. This removes the problem PR #107 left with
the owner: an embedder's macOS guests get the same agent as the CLI's.

### 4. The base carries only a launch daemon that runs `/bin/sh`

Install writes these to the guest once, over SSH as it does now:

- **The launch daemon**: `/Library/LaunchDaemons/<label>.plist`, owned by
  root, with `RunAtLoad` and `KeepAlive`, logging to
  `/var/log/<label>.log`. Its `ProgramArguments` are `/bin/sh -c <stub>`.
  The stub is inline in the plist, so the base holds no executable of
  disco-vm's, and `/bin/sh` is Apple's own signed binary.
- **The stub**:
  1. Waits for the volume with the drive's label to be mounted. If Disk
     Arbitration has not mounted it after a short wait, the stub mounts it
     read-only itself with `diskutil mount readOnly`. This works whether or
     not anyone logs in.
  2. Copies `disco-vm` into a root-only (0700) directory under
     `/var/run`. It writes a temporary file, checks it against
     `disco-vm.sha256`, sets mode 0755, and renames it into place.
  3. Unmounts the volume.
  4. `exec`s the copy with the `guest` argument.

  Under `KeepAlive`, an agent that exits restarts the stub, which copies the
  agent again. The agent runs from a copy, so it never pages from a volume
  that can go away underneath it, and it has a stable path of its own on the
  root disk.
- **Unchanged from today**: `pmset -a sleep 0`, and the install account
  moved off uid 501.

`/usr/local/libexec/disco-vm` is no longer written. Install keeps SSH, since
only SSH can change an account (docs/drivers/vz.md). Once the stub is in,
install no longer waits for an agent it copied. It waits for the agent the
first boot's drive carries, as every later boot does, and then shuts down
through it. `InstallSpec.Agent` stays for hcs, and vz does not read it.

### 5. A macOS boot checks the agent's protocol, as a Linux one does

`Hypervisor.Boot` runs `guest.CheckProtocol` for every guest OS. It kills a
guest whose agent reports a protocol other than the host's, and refuses it.

On a base built with this design, a mismatch is a bug, because the agent
came from this boot's drive. A base installed before this design still
runs its baked agent and ignores the drive. While its protocol matches, it
works. Once the protocol moves, it is refused with an error saying to
install it again. disco-vm does not move it forward: that was vm#83, which
the owner dropped. Under ADR 26-11-13-679 such bases are unsupported
anyway.

### 6. The agent is ad-hoc signed and not notarized

Apple silicon runs only signed code. Go's linker ad-hoc signs every
darwin/arm64 executable, including a cross-compiled `CGO_ENABLED=0` one, and
the agent needs no cgo (nothing in `pkg/guest` uses it). That is all the
agent needs:

- **No notarization or Gatekeeper.** Gatekeeper and notarization check only
  files with the quarantine attribute. An application that downloads a file
  sets that attribute, but a root shell's `cp` off a local volume does not.
  Nothing on the drive is downloaded inside the guest.
- **No entitlement.** The agent needs no entitlement in the guest. The
  host's Virtualization entitlement is the host binary's own, and is not
  needed in the guest.
- **No TCC grant.** The agent holds no TCC grant today: account changes go
  over SSH (docs/drivers/vz.md). An ad-hoc identity is its cdhash, so a
  grant made to one agent build would not survive the next build anyway.

The host binary's signing stays as ADR 26-11-13-679 §6 sets it, and
Developer ID stays deferred there.

### 7. Linux guests do not change

Linux guests already boot the host's agent and keep none, by a better path
than a drive:

- vz and libkrun append the agent to the initrd. It is there before any
  disk is mounted.
- The base needs no daemon to find it. A unit in the image runs it from a
  tmpfs (pkg/guest DESIGN.md, "The Linux guest image").
- It uses no extra device or mount.

A drive would add a device, a mount, and a stub, and would remove nothing.
macOS takes a drive only because vz cannot hand a macOS guest an initrd: it
boots macOS through its own boot loader from the guest's disk. Docker guests
take the agent from configuration at Create. Their container is the
instance, and has no boot to attach a drive to.

## Alternatives rejected

- **Move an out-of-date agent forward at boot through the old agent (vm#83,
  PR #107).** It needs the old agent to speak enough of the new protocol to
  replace itself. It writes into every guest's disk, so a commit carries
  whatever was moved. It worked only when the shim is disco-vm. The owner
  dropped it.
- **Point the launch daemon straight at the agent on the mounted volume.**
  launchd starts daemons before Disk Arbitration mounts the drive, so the
  daemon would start, fail, and be throttled. The agent would also page from
  a volume the guest user can eject. A stub that waits, copies, and execs
  avoids both, and the spike showed it works.
- **A small signed launcher binary baked into the base instead of a shell
  stub.** It is code of disco-vm's in every base. It would need its own
  signing story, and changing it would need a reinstall, which is the
  problem this ADR removes. `/bin/sh` is already in every macOS.
- **Attach the agent as a virtiofs share.** It would also work in principle
  (`mount_virtiofs`, or the framework's automount tag). But it is a host
  directory open to the guest for the whole boot, and not an immutable image
  whose digest is its name. Its mount timing in a macOS guest before login is
  unproven, while a read-only block device is what the spike booted.
- **A FAT or APFS image instead of ISO 9660.** FAT has no execute bit and
  loses the file mode. APFS needs `hdiutil` or `newfs_apfs` on a Mac to
  build, so the builder could not be tested on Linux and Windows CI. An
  ISO 9660 image is read-only by format, carries modes through Rock Ridge,
  can be built in pure Go, and is what the spike booted.
- **Build the drive with `hdiutil makehybrid`.** The output depends on the
  host tool's version and is not reproducible, and the builder could be
  tested only on a Mac.
- **One cached image attached to every VM, with no per-boot clone.** It works
  only if the framework lets two running VMs open one image file at once,
  which is unproven, and the cache could not be pruned while a VM runs. A
  `clonefile` costs nothing.
- **Notarize the agent, or sign it with Developer ID now.** Nothing in the
  guest checks either (§6). Developer ID is deferred with the host binary's
  (ADR 26-11-13-679).

## Consequences

- An agent change no longer needs a macOS reinstall or a base rebuild. A
  host upgrade reaches every existing VM on its next boot. A base needs a
  reinstall only when the stub or the plist changes.
- Every macOS boot checks the protocol. A pre-drive base is refused once the
  protocol moves, rather than failing deep in a request.
- vz's configuration gains a privileged `macos-agent` field. The published
  agent index gains `darwin/arm64`, so an embedder ships no disco-vm binary
  for macOS either.
- Install changes: it writes the stub's plist instead of copying a binary,
  and its first boot attaches the drive. `docs/drivers/vz.md` (Install
  step 3, Boot), `pkg/machine/DESIGN.md`'s agent row, the root `DESIGN.md`'s
  "One binary, three roles", and the proof table change with the
  implementation.
- What changes in discobox-ai/discobox:
  - This supersedes step 1.9 of the #42 plan, and the parts of its ADR
    26-11-11-963 §9 about moving a macOS guest's agent.
  - discobox-ai/discobox#59 (the darwin agent and its launch daemon) becomes
    "pin the darwin agent's digest beside the Linux one". The launch
    daemon is disco-vm's stub.
  - discobox-ai/discobox#63 (darwin bundles) and discobox-ai/discobox#126
    (the host's darwin base VM) take a base made once with the stub.
  - discobox-ai/discobox#128 does not need to reinstall a base for an agent
    change.
  The discobox-side ADR or plan edits are discobox's to make.
- The proof this needs, on a real Mac:
  - a base installed with the stub boots the drive's agent with automatic
    login on and with it off;
  - a protocol mismatch is refused;
  - an agent change reaches a committed VM's clone with no reinstall;
  - a VM fenced to nothing still boots its agent;
  - a cross-compiled `CGO_ENABLED=0` agent from the published index runs.
  On every OS, a unit test shows the ISO builder produces the same bytes
  for the same agent and that macOS's own reader reads it back (on a Mac).

## Deferred

- **Windows guests (hcs)** still bake the agent at install, and have the same
  problem macOS had. Revisit when a Windows base outlives an agent protocol
  change in practice. A drive would work there the same way: a service
  stub, with the agent copied from a read-only VHD or ISO.
- **Developer ID signing of the agent**: revisit when the agent needs a TCC
  grant, such as Full Disk Access or screen recording for a stream. The grant
  has to survive agent updates, and that needs a stable designated
  requirement.
- **Left to implementation**, none of it decided here:
  - the volume label;
  - the stub's wait before it mounts the volume itself;
  - the directory under `/var/run`;
  - the Go ISO 9660 library;
  - the field's final name, if ADR 26-11-13-679's rename lands first;
  - whether the engine stops passing `InstallSpec.Agent` to a driver that
    does not read it.
