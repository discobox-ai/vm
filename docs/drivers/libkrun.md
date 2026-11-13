# libkrun driver (Linux host, Linux guests)

`pkg/machine/libkrun` runs disco-vm's Linux guest as a libkrun microVM on KVM
(ADR 26-11-03-530). It is the Linux default. It is a `local.Hypervisor`, so
the store, the shim per running VM, and the guest-port plumbing are
`pkg/machine/local`'s ([DESIGN.md](../../pkg/machine/local/DESIGN.md)). It is
pure Go and linux-only: no disco-vm process loads libkrun.

## What a host needs

- **linux/amd64 with KVM**, and `/dev/kvm` open to the user, read and write
  (the `kvm` group). arm64 is deferred (§10).
- **Nothing installed.** The runtime brings its own libc, so the host's
  glibc sets no floor, and the guest's kernel comes in the guest image.

`Check` (`disco-vm info`) refuses a host that cannot run it and names why:
another architecture, no `/dev/kvm`, one the user cannot open, or a cached
runtime whose launcher does not run. Each error also names `--driver docker`
and `--driver boxd`. disco-vm never falls back to another driver on its own
(§11). Check does not pull; Prepare does, and Prepare and Boot check the host
again.

## The runtime

The runtime is one directory (`pkg/machine/libkrun/runtime`, built by
`go tool task build:krun-runtime`):

| path | what |
|---|---|
| `bin/krun-launch` | the launcher, `launch.c`: one process per VM |
| `bin/passt` | passt, static |
| `lib/` | `libkrun.so.1` (1.19.7, built with `BLK=1 NET=1 INIT_BLOB=0`, no libkrunfw), the libraries it needs, glibc, and `ld-linux-x86-64.so.2` |
| `runtime.json` | the runtime's version, libkrun's and passt's, and the launch configuration version the launcher reads |

- The launcher always runs as
  `lib/ld-linux-x86-64.so.2 --inhibit-cache --library-path lib
  bin/krun-launch CONFIG`. The loader would still fall back to the host's
  `/lib` for a library `lib/` lacks, so the driver checks that the
  launcher's whole `DT_NEEDED` closure is in `lib/` (at Check, at pull, and
  at Boot). That covers what is linked, not what is loaded at run time
  (NSS, gconv). A booted, running VM's launcher has mapped nothing else:
  the loader, `libc.so.6`, `libgcc_s.so.1`, and `libkrun.so.1`, all from
  `lib/` (by hand, 2026-11-03). The image build runs its
  `--version` that way in an empty filesystem.
- `libkrun.DefaultRuntime` names the runtime image,
  `ghcr.io/discobox-ai/disco-vm-krun`. Prepare pulls it into
  `cache/libkrun/runtime/<digest>`, with go-containerregistry and no daemon.
  It checks the platform and the files, and runs the launcher's
  `--version`, all before renaming the pull into place. A cached digest needs
  no network. A tag falls back to its newest pull when the registry cannot be
  reached.
- The configuration's `runtime` (`--libkrun-runtime`, `$DISCO_VM_KRUN_RUNTIME`;
  ADR 26-11-11-215) names an absolute runtime directory to use as it is.
- A runtime whose `runtime.json` names another launch configuration version
  than the driver writes is refused, naming both.
- Releases are a `krun/vN` tag (`.github/workflows/krun-runtime.yml`), in
  one job. It builds the runtime and boots the pinned guest image on it
  (`test:libkrun` with `DISCO_VM_LIBKRUN_BASE=default`). Then it pushes that
  same build from the cache and prints the digest to pin. Until the first
  release, `DefaultRuntime` is the `v1` tag, as `DefaultBase` is. Prepare
  asks the registry for it, and Boot uses the cached pull.
- **Release order.** The default base and runtime work only once both are
  published. First comes a `linux/v*` guest release carrying `krun/vmlinux`,
  pinned as `linuximage.DefaultBase`. Then comes a `krun/v*` release, whose
  boot needs that guest, pinned as `DefaultRuntime`. Until then a fresh VM of
  the defaults fails at Create, naming the image that is missing or that
  lacks `krun/vmlinux`. `DISCO_VM_BASE` and `DISCO_VM_KRUN_RUNTIME` name local
  builds in the meantime.
- Every source the runtime and the kernel are built from (libkrun, passt,
  libkrunfw, Linux) is pinned by its SHA-256 in its Dockerfile. Both build on
  Debian sid at the Taskfile's snapshot, the runtime with sid's own `rustc`,
  and the runtime bundles that sid's glibc.

## The guest

The guest is the Linux guest image (`pkg/guest/linux`, pulled through
`pkg/machine/linuximage` into `cache/libkrun/linux/<digest>`, as vz pulls it).
libkrun boots the image's `krun/vmlinux`, not Debian's (sid's) kernel, the
one exception to the guest booting Debian's own:

- It is built from libkrunfw's kernel source and patches (5.6.2, Linux
  6.12.109) and libkrunfw's x86_64 configuration, with `krun-kernel.config`
  merged over it: an initrd (zstd), autofs, ext4 ACLs, the full PID space
  (`BASE_SMALL` off), no `dummy0`, nf_tables with connection tracking, and
  the buddy hard-lockup detector, which libkrunfw leaves out and the command
  line's `nmi_watchdog=panic,1` needs to reset a wedged guest. The build fails
  if olddefconfig changes any of those.
- One patch is disco-vm's, `krun-kernel-poweroff.patch`. libkrun's x86
  machine has no ACPI, so nothing can power it off, and a halt spins. The
  patch makes a power-off or a halt a reset, which libkrun (`reboot=k`, its
  i8042) takes as the VM's exit.
- A kernel update is a `linux/v*` guest release. A VM keeps the kernel it was
  made with, as its bundle's `vmlinux`.

A VM is a bundle in its instance's directory (see the package doc):
`vmlinux`, `initrd.img`, `disk.img` (the root, raw ext4, grown sparse to
64 GiB and grown online at boot), `krun.json`, `machineid`, `macaddress`, and
per boot `launch.conf`, `boot-initrd.img`, `console.log`, and the boot
before's `console.prev.log`. libkrun truncates the console it writes.

A copy of a committed VM is a reflink (`FICLONE`, on btrfs and XFS), or else
a sparse copy of the blocks in use. `Chains` is false.

## A boot

1. The host is checked, the runtime found (or pulled, if it was removed behind
   the driver's back), and the agent appended to the image's initrd. The
   agent is the running disco-vm, a static linux/amd64 executable, or
   the configured one (`--libkrun-agent`, `$DISCO_VM_AGENT`; a test's). The initrd is padded to a page, since
   libkrun ends it on one and the kernel frees it from its start.
2. The vsock sockets are made in the instance's directory, or in a private
   temporary one when that path is too long for a Unix socket:
   - `vsock-in-7300.sock` and `vsock-in-7302.sock` are host to guest, and
     libkrun listens on them;
   - `vsock-out-<port>.sock` is one per forward, guest to host, and the shim
     listens on them before the VM starts.

   Nothing else is mapped (ADR 26-11-03-247). libkrun maps a port one way,
   so a forward of 7300 or 7302 is refused at Create. The vsock device has no
   TSI, so the guest reaches the host only through these ports.
3. Without a fence, passt starts on one end of a socketpair and the launcher
   gets the other as a virtio-net device. passt serves DHCP and DNS from the
   host's own configuration. A host with no default route gets no lease. passt
   maps no address to the host's loopback and forwards no port
   (`--map-host-loopback none --no-map-gw --tcp-ports none --udp-ports
   none`), so a guest reaches no service the host binds to its loopback, and
   is reached only over vsock (discobox ADR 26-11-11-963 §11). So a host
   whose resolver is on its loopback (systemd-resolved's `127.0.0.53`) gives
   the guest no DNS server; one off it is handed to the guest as before.
   `--dns-forward` to the host's resolver would keep DNS there, and is not
   used: it is a path to the host's loopback, which that ADR says carries no
   pool. If passt exits while the VM runs, the console (the shim's log) says
   why. With `Egress{}` there is no device and no passt. A non-empty `Allow`
   is refused at Create.
4. `launch.conf` is written (format in `launch.c`), and the launcher starts.
   It is a child of the shim, forked on a locked thread with
   `PR_SET_PDEATHSIG`, so it dies with the shim. The kernel command line is
   vz's (`linuximage.Cmdline`) plus `reboot=k`.
5. Boot waits for the agent and checks its protocol. A launcher that exits
   first fails the boot, with the launcher's standard error and the tail of
   the console.

`Kill` sends SIGKILL to the launcher. `Shutdown` goes through the agent, and
the guest's power-off is the patched reset. `Done` is the launcher's exit.

### Half-closes

libkrun 1.19's vsock proxy takes the host's half-close of a stream as its
close, and resets the guest's end before the guest has read what came
before it. The guest's half-close does reach the host. So the host side
never half-closes:

- **A caller's stream into the guest** (`Dial`): `CloseWrite` does nothing.
  The guest never reads EOF from the caller, and the stream ends when the
  guest closes it. A request and its answer (HTTP/1.0, the agent's own
  protocol) work. A guest service that waits for EOF on its input does not.
  Through a shim, whose relay cannot tell a caller's half-close from its
  close, a caller that closes first leaves the guest's end, and a goroutine
  of the shim's, open until the guest writes or closes.
- **A guest's stream out through a forward**: `CloseWrite` closes it. When
  the far end has sent all it will, the guest reads EOF. A guest still
  sending then is cut off, as libkrun would cut it.

## Testing

`go tool task test:libkrun` builds the static binary, the runtime, and the
amd64 guest image. It then runs `machinetest` (`TestConformance`, gated on
`DISCO_VM_LIBKRUN_BASE`) and `internal/e2e` with `DISCO_VM_DRIVER=libkrun`.
CI's `libkrun` job runs it on a GitHub-hosted `ubuntu-24.04` runner, which
exposes `/dev/kvm`. Depot's runners are not known to, so that job and the
release's boot run outside Depot (a `ci.runners` waiver in `.repocheck.yaml`).
