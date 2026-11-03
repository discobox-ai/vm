# 26-11-03-530 — libkrun runs Linux VMs through a launcher, in a runtime pulled by digest that brings its own libc

- **Status**: Proposed
- **Date**: 2026-11-03

## Context

disco-vm has no VM driver on Linux. `pkg/machine/default_other.go` makes
`fake` the default off macOS and Windows, and the drivers that run a guest
there are docker (a container, no VM boundary) and boxd (remote). discobox
defaults to Docker on Linux, so a Linux sandbox has no VM boundary
(discobox-ai/discobox#34). discobox-ai/discobox#221 put a libkrun provider in
discobox's own server instead, and was closed in favour of doing it here
(issue #37).

What #221 learned, as issue #37 records it:

- libkrun boots only a kernel carrying libkrunfw's patches. #221 built its own
  and used upstream libkrun 1.19 without libkrunfw.
- Upstream libkrun 1.19 needs host glibc 2.34 or later.
- Networking needs passt, which #221 shipped as a static build.
- It ran only on amd64 with KVM, and checked both before use. Its Go binding
  was amd64-only.
- To install nothing on the host, #221 put the kernel, `libkrun.so.1`,
  passt, and the guest's root disk in one digest-pinned image that the CLI
  staged, and loaded `libkrun.so.1` in-process from that digest's directory.
  A runtime that will not load (a wrong glibc, a junk `.so`) has to be
  detected, not crash the process that loads it.

What is true here today:

- **The Linux binary is static.** `go tool task build` and `install` set
  `CGO_ENABLED=0` off macOS (`Taskfile.yml`), because disco-vm is also the
  guest agent, which goes into guests with another libc or none. On a Linux
  amd64 host the running binary is therefore already a Linux amd64 agent.
- **libkrun takes over the process that starts a VM.** `krun_start_enter`
  returns only if the VM fails to start. Otherwise libkrun owns the process
  and calls `exit()` when the guest powers off. The shim (`pkg/machine/local`)
  must outlive its VM long enough to report it stopped, and serves the VM's
  guest operations, ports, and URL while it runs.
- **A local hypervisor is a `local.Hypervisor`** (`pkg/machine/local/local.go`):
  it prepares, boots, commits, and destroys a disk in a directory, and the
  local driver keeps the store and runs a shim per running VM (ADR
  26-10-23-417).
- **disco-vm has its own Linux guest** (ADR 26-10-31-990 §3): `pkg/guest/linux`,
  published as `ghcr.io/discobox-ai/disco-vm-linux` for `linux/arm64` and
  `linux/amd64` on its own `linux/v*` line. vz pulls it per manifest digest
  into `cache/vz/linux/<digest>` (`pkg/machine/vz/linuxguest.go`, pure Go),
  and boots its `vmlinux`, its `initrd.img` with the agent appended as a cpio
  archive, and its `root.ext4` as the instance's own writable root. Its
  network matches `eth0` as well as `enp0s*` so that it boots on libkrun too.
  Its kernel is Debian's, which libkrun does not boot.
- **The seam already has what libkrun's vsock offers.** `machine.Forward`'s
  comment says a forward works "as libkrun maps a vsock port to a Unix
  socket" (`pkg/machine/machine.go`), and the shim's guest-to-host forwards
  pass half-closes through the same way (`pkg/machine/DESIGN.md`).
- **`Capabilities.Commit`** is proposed in ADR 26-11-03-868. A driver must
  report it honestly.

## Decision

### 1. A `libkrun` driver, a `local.Hypervisor` with no cgo

`pkg/machine/libkrun` is a `local.Hypervisor` named `libkrun`, built on
`linux` only and linked by `pkg/machine/drivers/drivers_linux.go`. It is pure
Go. It never loads libkrun into a disco-vm process: the binary stays static,
and stays the guest agent.

### 2. A launcher process runs each VM, as a child of its shim

The runtime (§3) carries `krun-launch`, a small C program linked against
libkrun. Its source is `pkg/machine/libkrun/runtime/launch.c`, built in the
runtime's image build and nowhere else. One launcher is one VM:

- **The shim starts it.** Boot in the shim writes the VM's configuration (the
  kernel, initrd, command line, disks, vsock ports and their sockets, the
  network's file descriptor, CPUs, memory, shares, and the console log) to a
  file in the instance's directory, and execs the launcher on it. The
  launcher makes the libkrun calls and ends in `krun_start_enter`. The format
  of that file is the launcher's and disco-vm's alone, versioned with the
  runtime, and is left to implementation.
- **The VM's life is the launcher's.** The machine's `Done` is the launcher's
  exit, `Kill` is `SIGKILL` to it, and `Shutdown` goes through the agent, as
  on vz. The launcher is started with `PR_SET_PDEATHSIG`, so a VM dies with
  its shim, as a vz VM does.
- **A failure stays in the launcher.** A runtime that will not load, a
  configuration libkrun refuses, or a crash in libkrun is the launcher's
  exit status and its standard error, which Boot returns. Nothing of libkrun
  runs in the shim.

The launcher is runtime content, like the kernel and passt: it ships in the
runtime image, never in a disco-vm release. disco-vm is still one binary in
its three roles (CLI, agent, shim).

### 3. The runtime is one image, pinned by digest, pulled into the driver's cache

The host side of libkrun is one OCI image, `ghcr.io/discobox-ai/disco-vm-krun`,
whose final `FROM scratch` stage is a directory:

| path | what |
|---|---|
| `bin/krun-launch` | the launcher (§2) |
| `bin/passt` | passt, static |
| `lib/` | `libkrun.so.1`, every library it needs, glibc, and its dynamic loader (`ld-linux-x86-64.so.2`) |
| `runtime.json` | the runtime's version, libkrun's and passt's versions, and the launcher's configuration version |

- **It is pinned by digest** in the driver (`libkrun.DefaultRuntime`), as
  ADR 26-10-31-990 §3 pins the guest image. The pin moves by hand.
- **Prepare pulls it** when the cache lacks it, with go-containerregistry and
  no daemon, as vz pulls a guest image: it checks the declared platform
  (`linux/amd64`) and that every file above is there, unpacks it under a
  temporary name in `cache/libkrun/runtime/`, runs `krun-launch --version`
  from it (§4), and only then renames it to `cache/libkrun/runtime/<digest>`.
  Boot uses that directory and pulls only if it was removed behind the
  driver's back.
- **A directory may stand in for it.** `$DISCO_VM_KRUN_RUNTIME`, read by the
  driver as `$DISCO_VM_AGENT` is, names a runtime directory used as it is: a
  runtime built from the checkout (`go tool task build:krun-runtime`).
- **It is built from source** in `pkg/machine/libkrun/runtime` (a Dockerfile
  and `launch.c`), with libkrun and passt at pinned upstream releases, and is
  verified in its build: the image build runs the launcher's `--version`
  through its own loader, so a broken runtime fails there. It is released on
  its own line (`krun/v*` tags), as the guest image is: a libkrun or passt
  update needs no disco-vm release, and a disco-vm release implies no new
  runtime.
- **A runtime release boots the guest.** The runtime's CI job boots the
  pinned guest image's libkrun kernel and root on it (§9) before it
  publishes.

The libkrun version is at least 1.19, #221's, and is pinned at
implementation. Which libkrun calls the launcher makes (an external kernel,
disks, vsock ports mapped to Unix sockets in both directions, a vsock device
with no TSI, a network on a Unix stream to passt, virtiofs) is left to
implementation, within §6.

### 4. The runtime brings its own libc, so the host's glibc sets no floor

The launcher is never run through the host's dynamic loader. The shim runs
it as `<runtime>/lib/ld-linux-x86-64.so.2 --library-path <runtime>/lib
<runtime>/bin/krun-launch`, so libkrun, glibc, and every other library come
from the runtime and none from the host. libkrun 1.19's glibc 2.34 floor
becomes a fact about the runtime's build, not about the host. A host needs a
Linux kernel with KVM, and nothing else of its userland. passt is static and
needs nothing.

So there is no glibc check. A runtime that will not load anyway (a corrupt
pull, a library missing from `lib/`) fails `krun-launch --version` at pull,
or the launch at Boot, and the error carries the launcher's output.

### 5. The kernel is our build of libkrunfw's, and the guest image carries it

- **The kernel is built from libkrunfw's kernel source and patches**, at a
  pinned libkrunfw release, as an uncompressed `vmlinux` that the launcher
  boots as an external kernel. libkrunfw's configuration is the start, with
  what the guest image's systemd needs added; the exact delta is left to
  implementation. libkrunfw's own library (`libkrunfw.so`, a kernel wrapped
  in a shared object that libkrun loads) is not used.
- **The guest image carries it.** `pkg/guest/linux` builds it in a stage on
  the build platform, cross-compiling, and publishes it as `krun/vmlinux`
  beside Debian's `vmlinux`. ADR 26-10-31-990 §2 makes a guest's kernel the
  image's, carried from base to VM to committed VM, and that holds here: a
  libkrun VM's bundle holds the libkrun kernel, and a committed VM keeps the
  kernel it was made with.
- **A kernel update is a guest release.** Bumping the libkrunfw pin in the
  Dockerfile is a `linux/v*` release of the guest image. Moving the
  driver's base pin (§6) gives fresh VMs the new kernel. VMs made from
  committed ones keep theirs.
- **The contract between kernel and runtime is virtio**: a block device, a
  vsock device, a network device, a console, and virtiofs, with no TSI
  (§7). Neither side relies on a libkrun feature only a matching kernel has,
  so a runtime update boots kernels already in committed VMs. §9's boot test
  checks that for the pinned guest.

### 6. The guest is disco-vm's Linux image, its kernel swapped, and the agent is the running binary

A fresh libkrun VM starts from the same base vz's Linux guests do: the
`disco-vm-linux` image (ADR 26-10-31-990 §3), its `linux/amd64` variant,
pulled per digest into `cache/libkrun/linux/<digest>`. It is not discobox's
guest (discobox ADR 0101), which carries no agent, for the reasons 990
rejected it. The pull and its cache are vz's code, moved out of
`pkg/machine/vz` into a package both drivers use. `Config.Base` names it as
on vz: empty for the default, a reference, or a directory.

A VM is a bundle:

| file | committed VM | VM |
|---|---|---|
| `vmlinux` (the image's `krun/vmlinux`) | ✓ | ✓ |
| `initrd.img` | ✓ | ✓ |
| `disk.img` (raw ext4, the root) | ✓ | ✓ |
| `krun.json` (version, size) | ✓ | ✓ |
| `machineid`, `macaddress` | | its own, from Create |
| `launch.json`, `console.log` | | its own, from Boot |

- **The root is the VM's own and writable**, extended sparse to its disk
  ceiling and grown online at boot, as on vz.
- **A copy is its own disk.** Prepare copies a committed VM's files with
  `FICLONE` where the filesystem has reflinks (btrfs, XFS), and otherwise as a
  sparse copy of the blocks in use. `Chains` is false, so a committed VM can
  be removed while copies of it live.
- **Boot hands the kernel the image's initrd with the agent appended**, as vz
  does, and the same command line (`console=hvc0 root=/dev/vda rw
  rootfstype=ext4 panic=5`, `systemd.machine_id=`, `disco-vm.nic=0` with no
  NIC). The libkrun kernel has its drivers built in, so the initrd's module
  loads are no-ops there; it is still what runs the agent's initramfs hook.
- **The agent is the running binary.** On a Linux amd64 host disco-vm is a
  static Linux amd64 executable, so Boot appends `os.Executable()`, as hcs
  and vz's macOS install bake in theirs, and needs no `$DISCO_VM_AGENT`. The
  protocol check of 990 §2 still runs, so an embedder's binary of another
  disco-vm version is refused by check.

### 7. What it reports

| capability | libkrun |
|---|---|
| guest OS | linux |
| max running | none |
| install | **none** |
| base | the Linux guest image, `DefaultLinuxBase` by default |
| commit (ADR 26-11-03-868) | yes: its record and bundle, left in place |
| chains | no |
| agent | yes, handed it in the initrd at every boot |
| ports | vsock, through the shim: the agent's port and `Dial` on Unix sockets the launcher listens on and maps into the guest; `Forwards` on Unix sockets the shim listens on, which the guest's vsock connections to the host reach |
| egress | `none`: no network device |
| private URL | none |
| shared dirs | virtiofs |
| display | no |
| keys | none (`ErrNoKey`) |
| isolation | no |
| volumes | none yet |

- **The network is passt's, or none.** A VM with no fence gets a virtio-net
  device on a Unix stream to a passt process the shim starts beside the
  launcher and stops with it. passt serves DHCP and DNS, which the guest's
  network takes, as it does vz's NAT. `Egress{}` is no network device and no
  passt, as on vz (990 §5). A non-empty `Allow` is `ErrUnsupported`.
- **TSI is off.** libkrun's transparent socket impersonation would turn the
  guest's sockets into the host's through vsock. The vsock device is made
  with no TSI features, so a VM with no network device has no way out but
  the vsock ports its spec forwards, and the fence holds by construction.

`machinetest.Run` passes against it, gated on `DISCO_VM_LIBKRUN_BASE` (a guest
image's reference, `default`, or a directory), as vz's Linux suite is on
`DISCO_VM_VZ_LINUX_BASE`.

### 8. Check refuses a host that cannot run it, and names why

`Check` refuses, each with its own error naming the cause and what would fix
it:

- a host that is not `linux/amd64` (§10);
- no `/dev/kvm`, or one this user cannot open read-write (the `kvm` group);
- a cached runtime whose `krun-launch --version` fails, with its output.

Check does not pull. Prepare refuses a runtime or a guest image that will
not pull or verify, naming which. Boot refuses a launch that exits before the
agent answers, with the launcher's standard error and the tail of
`console.log`.

### 9. CI builds, publishes, and boots it

- The runtime and the guest image's libkrun kernel build, and verify
  themselves, on every pull request that touches them, as the guest image
  does today (`linux-guest.yml`).
- A `krun/v*` tag publishes the runtime and prints the digest to pin, as
  `release:linux-guest` does.
- A CI job on a GitHub-hosted Linux amd64 runner, which exposes `/dev/kvm`,
  runs `machinetest` on libkrun with the runtime and guest image built in the
  same run, so a change to the driver, the runtime, or the kernel is booted
  before it lands.
- Taskfile targets for these: `build:krun-runtime`, `release:krun-runtime`,
  and `test:libkrun`.

### 10. amd64 now, arm64 later

The driver's code has no architecture in it: the runtime is chosen by the
host's platform, and the launcher's configuration names the kernel's format.
The runtime and the libkrun kernel are built and published for `linux/amd64`
only, and Check refuses arm64, naming it (Deferred).

### 11. libkrun is the Linux default, with no fallback

`pkg/machine/default_linux.go` makes `libkrun` the default on Linux.
`default_other.go` keeps `fake` for every other OS. A Linux host that cannot
run libkrun is refused by §8's error, which also names `--driver docker` and
`--driver boxd`; disco-vm never falls back to another driver silently, since a
caller that asked for a VM boundary must not get a container. Tests are
unaffected: `internal/e2e` and the engine tests name their driver.

## Alternatives rejected

- **libkrun in the disco-vm process, through cgo.** The Linux binary would
  need cgo and the host's glibc, and could no longer be the static agent it is
  in every guest. `krun_start_enter` would also take over the shim, which
  must outlive the VM to report it stopped and serves its operations while it
  runs, and a crash in libkrun would take the shim with it.
- **libkrun in-process through `dlopen` with no cgo** (purego's approach).
  It still links the host's dynamic loader and glibc into the process, which
  is the glibc floor and an unstatic agent again, and leaves
  `krun_start_enter` owning the shim.
- **The shim itself as the launcher, forking into libkrun.** A static Go
  process cannot load a glibc library, so the fork would have to exec
  something anyway, which is the launcher.
- **Accept the glibc 2.34 floor.** It shuts out every host older than it, and
  turns a host's libc into a load failure disco-vm can only report. Bringing
  our own libc costs a few megabytes in the runtime.
- **Build libkrun against an older glibc.** It lowers the floor without
  removing it, and ties the runtime's build to an old distribution's
  toolchain, which libkrun's Rust toolchain outruns.
- **Link the launcher and libkrun statically, against musl.** It would make
  the runtime one file with no loader. libkrun's build against musl is
  unproven, #221 did not try it, and the bundled loader removes the floor
  without it (Deferred).
- **Install libkrun from the host's packages.** The host would need a
  distribution that packages libkrun at a version we support, and a person to
  install it. Issue #37 asks for nothing on the host but disco-vm.
- **Ship the runtime in disco-vm's release, or embed it in the binary.**
  disco-vm is the agent, appended to every Linux guest's initrd and copied
  into Docker guests, so tens of megabytes of runtime embedded in it would go
  into every guest. Beside the binary in a release archive, it would no
  longer be a `go install`, and a libkrun or passt fix would need a disco-vm
  release. A digest-pinned image is the model vz's guest already uses.
- **Use libkrunfw's library.** It wraps the kernel in a shared object libkrun
  loads, so the kernel would be the host runtime's: it would change under
  committed VMs whenever the runtime's pin moved, against ADR 26-10-31-990 §2,
  where a kernel is the image's and carried to every copy. It is also a
  second library to load, for no gain once libkrun takes an external kernel.
- **The kernel in the runtime image, as #221 had it.** The same objection:
  the kernel would be the host's, not the guest's, and a committed VM would
  boot whatever kernel the running disco-vm pins.
- **Boot the guest image's Debian kernel.** libkrun boots only a kernel with
  libkrunfw's patches (#221; ADR 26-10-31-990's context), so it does not boot.
- **The guest's root from discobox's guest image (discobox ADR 0101), or a
  libkrun-specific image.** discobox's carries no agent, as 990 found. A
  libkrun-only image would be a second guest to build, release, and keep in
  step, when one image already boots on vz and is built to boot on libkrun.
  Only the kernel differs, and the image carries both.
- **TSI for the guest's network.** The guest's sockets would be the host's,
  so the guest's egress would be the host's own, with no device to remove for
  `Egress{}`, and the kernel would need TSI's patches to match the runtime's
  libkrun. passt keeps the network a device that is there or is not.
- **A qcow2 overlay over the committed VM's disk**, so copies chain as hcs's
  do. Copies on ext4 would be instant, but a committed VM would have to
  outlive every copy (`ErrInUse`), and reflinks already make copies instant on
  btrfs and XFS (Deferred).
- **A nix flake for the runtime, as #221 had.** It would be a second build
  system beside the Docker builds the guest image already uses, in CI and on a
  developer's machine.
- **Fall back to docker or fake where libkrun cannot run.** A caller that
  asked for a VM would silently get a container with no VM boundary, or a
  guest that is a host process.

## Consequences

- **The seam does not change.** libkrun is a `local.Hypervisor` with the
  capabilities above, and nothing in `pkg/engine` or `internal/cli` learns
  of it beyond the default's name.
- **vz's guest image pull moves** to a package vz and libkrun share, with its
  cache under each driver's own `cache/<driver>/linux/`.
- **The guest image gains a kernel build**, `krun/vmlinux`, cross-compiled
  on the build platform, which lengthens its build. Its `verify` step checks
  the file and its architecture; only §9's job boots it.
- **disco-vm gains a second artifact line**, `krun/v*` for
  `ghcr.io/discobox-ai/disco-vm-krun`, a Dockerfile and C source under
  `pkg/machine/libkrun/runtime`, a CI workflow that needs a runner with KVM,
  and a pin to move by hand. A runtime release must boot the pinned guest.
- **The launcher is C code in this repository**, built only into the runtime
  image. It is the one place libkrun's API is called, so a libkrun upgrade
  touches it and the runtime's version, and the configuration version the
  shim writes must match the runtime's `runtime.json`; Boot refuses a
  mismatch, naming both.
- **A Linux host's default changes** from fake to libkrun. A user on a host
  without KVM (most containers, many CI runners) who relied on the default
  now gets §8's error and names a driver.
- **`docs/drivers/libkrun.md`** is the driver's brief; `DESIGN.md`'s stack,
  `pkg/machine/DESIGN.md`'s capability table, `pkg/machine/local/DESIGN.md`'s
  cache layout, and the proof table gain libkrun in the change that
  implements it. Claims that the libkrun kernel runs the guest image's initrd
  and its agent hook, that the agent answers on vsock with TSI off, that a VM
  with no network device reaches nothing, that passt's lease and DNS work for
  `eth0`, and that a VM dies with its shim are checked in a real guest before
  their rows are.
- **discobox-ai/discobox#34** can be closed by discobox using this driver,
  in an issue of its own.

## Deferred

- **arm64.** Revisit when a Linux arm64 host needs a VM boundary and CI has
  an arm64 runner with KVM to boot it. The guest image already builds arm64;
  the runtime and the kernel would add the variant.
- **A static, musl-linked launcher.** Revisit if libkrun builds against musl
  upstream; it would drop the bundled loader and glibc.
- **Data and cache volumes** (ADR 26-11-02-180), as extra virtio-blk disks or
  virtiofs. Revisit with vz's, which has none yet either.
- **qcow2 overlays for copies on filesystems without reflinks.** Revisit if a
  sparse copy on ext4 makes Create too slow for a pool.
- **An egress allowlist** (`EgressAllow`). passt has none. Revisit when a
  caller needs one on Linux that a peer forward to a proxy does not give.
