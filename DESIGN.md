# disco-vm design

disco-vm runs VMs, fresh from a driver's base or from a committed VM, and
commits them to make more (ADR 26-11-03-606), on a driver that reports it
can (ADR 26-11-03-868). It runs
Windows guests on Windows (Host Compute Service), macOS and Linux guests on
macOS (Virtualization.framework; Linux from a base, ADR 26-10-31-990), and
Linux guests in the cloud on boxd from any host, or as Docker containers that
boot systemd in a user namespace of their own. There is one codebase, one binary per host OS, and one command line.
It is used two ways:

- **Standalone:** `disco-vm install | run | exec | cp | stop | commit | upgrade | rm ...`.
- **As a discobox sandbox provider:** discobox embeds `pkg/engine` as a
  library.

## The stack

```
  disco-vm CLI (internal/cli)           discobox provider (embeds the engine)
                 \                         /
                  pkg/engine  ── VMs, lifecycle, install, commit; no state
  ─────────── machine.Driver seam: the provider's VMs, committed or not; Machine is machine.Guest ───────────
     machine/local (store + shim on this host's disk)   machine/docker (any)   machine/boxd (any)
   machine/hcs (windows)  machine/vz (darwin)  machine/fake       │                     │
          │                     │                     │           │                     │
 HCS VM (vmcompute)    VZ VM (its shim's)       host process   container + userns    boxd microVM
          │                     │                     │          (Engine API)         (gRPC API)
      hvsocket                vsock                  tcp         exec + `pipe`        Exec: sudo, tar,
          └──────── port 7300 ──┴─────────────────────┴───────────────┘               systemctl; no
            disco-vm guest (the agent), reached with pkg/guest's client               agent at all
```

Everything above the seam is OS-neutral and is tested on every OS through the
fake driver, and keeps no state ([below](#the-engine-keeps-no-state-and-a-driver-keeps-it-in-its-provider)).
Above the seam a running guest is only `machine.Guest`: exec, copy in and
out, info, and an orderly shutdown (ADR 0001). How a driver provides them is
its own: the agent over a private channel, or boxd's API. Everything below it
is one package per hypervisor, selected by build tags, so each binary
contains only the drivers its OS can run (`pkg/machine/drivers`). hcs, vz, and
fake keep their state on this host's disk, through `pkg/machine/local`, which
they share. boxd's hypervisor is in the cloud, behind an API, and a Docker
daemon may be on another host or in Docker Desktop's VM, so every binary
contains both.

## Where the design lives

Each package's `DESIGN.md` holds the decisions it owns:

- [pkg/machine](pkg/machine/DESIGN.md): the driver contract and capabilities,
  ports, windows, and cold clones. Its drivers:
  [boxd](pkg/machine/boxd/DESIGN.md) and [docker](pkg/machine/docker/DESIGN.md),
  and [local](pkg/machine/local/DESIGN.md), the store and shim of hcs, vz,
  and fake.
- [pkg/guest](pkg/guest/DESIGN.md): the agent protocol, HTTP between host and
  agent.
- [pkg/engine](pkg/engine/DESIGN.md): who holds a running VM, the service URL,
  VMs and how they are made, projects, and why disco-vm does not converge
  guests.

## One binary, three roles

`disco-vm` is the CLI. It is also the guest agent (`disco-vm guest`) and each
VM's supervisor (`disco-vm shim <id>`). The agent is the same program built for
the guest's GOOS. On a Windows host the guest is Windows, and on a macOS host
the guest is macOS, so the running binary is also the agent, and install bakes
in `os.Executable()`. A Docker guest is Linux, so from another host
`$DISCO_VM_AGENT` names a disco-vm built for it, and so does a Linux guest on
vz, which every boot hands that agent in its initrd and refuses if it speaks
another `guest.Protocol`; `go tool task build` builds it beside the darwin
binary. Protocol changes therefore
ship to both ends at once. A driver that reaches its guest without the agent
(boxd) does not list `Capabilities.Agent`, and puts nothing of disco-vm's in
the guest.

## The engine keeps no state, and a driver keeps it in its provider

disco-vm's core keeps nothing: no database, no root, and no files (ADR
26-10-23-417). `engine.Open(driver, project)` is the whole of an engine, and
every VM is read from the driver, which reads it from its provider every
time, so any process with the provider's credentials sees
the same ones, on any host. The driver keeps what it knows on the provider's
own resources, in the provider's way:

- **docker:** labels, set when a container is made or a VM committed
  (`ai.discobox.vm.*`: project, name, the VM it was created from, size), and
  names: a VM's container `dvm-<project>--<name>`, a committed VM's Docker
  image `disco-vm-vm:<project>--<name>`. Nothing is kept on the host.
- **boxd:** names, since boxd keeps no labels: a VM's machine, or a
  committed VM's snapshot, `dvm-<project>--<name>`, at most 63 characters,
  so a project and a name are 57 together; and what the API reports: a
  snapshot's creation, a machine's size, image, and creation. boxd does not
  say which snapshot a machine was restored from, so a boxd VM has no
  `From`; a restored machine outlives its snapshot (by hand, 2026-10-23).
  Nothing is kept on the host.
- **hcs, vz, fake:** this host's disk is the provider, kept by
  [pkg/machine/local](pkg/machine/local/DESIGN.md) in a directory of its own.

A VM's name is unique in its project, committed or not, the way its provider
makes names unique (a container's and a committed image's, a machine's and a
snapshot's, a claim file), and is 1 to 63 lowercase letters, digits, and
single hyphens (`machine.ValidateName`). That a VM is committed is the
caller's to record: not every provider can tell (Archil keeps a committed
sandbox as a stopped one).

**Nothing from before is read** (ADR 26-10-23-417): disco-vm is pre-alpha,
so a root's `disco-vm.db` and the layers and instances it described, the
images and tags of before ADR 26-11-03-606, and what a driver made under its
old names, are not taken in. Remove them, or
keep using the disco-vm that made them.

## A driver is given its key

A driver calls its provider with the key its `machine.Config.Keys` source
answers, asked on every call, and never reads the environment for one; the
command line's source does (`machine.KeyFromEnv`), and only the command
line's. A driver issues keys for another host to run the project's machines
(`IssueKey`, `IssuedKeys`, `RevokeKey`; `disco-vm key issue | ls | revoke`)
that expire and cannot issue keys, or issues none (ADR 26-10-24-562;
[pkg/machine](pkg/machine/DESIGN.md#a-driver-is-given-its-key-and-issues-keys-that-cannot-issue)).

`--debug` (`machine.SetDebug`) has a driver report every call it makes to a
service outside the process, boxd's API or the Docker daemon, and how long the
answer took.

## What is proven, and where

| | fake (all OSes, CI) | hcs | vz | boxd | docker |
|---|---|---|---|---|---|
| machine conformance suite (`pkg/machine/machinetest`): since ADR 26-11-03-606 it starts from a committed VM, an install, or a fresh VM of the driver's base; commits a VM and lists it, its name taken; creates VMs from it that carry its writes and no one else's; makes a fresh VM, or finds the driver refuses one; and a committed copy of a copy outlives the first's removal, or the removal is refused with `ErrInUse`; since ADR 26-11-03-868 only those cases commit, every other subtest starts from the start, and a driver that does not report `Commit` is checked to refuse a commit and a copy and change nothing; since ADR 26-10-23-417 it finds and lists VMs by a name no second one can take, and attaches to a started machine from another call | ✅ from an install, fresh, and with no commit (`NoCommit`, 2026-11-03) | ✅ Win 11 Pro guest, before ADR 26-10-23-417; ⬜ since (ported to `From`, a committed install: `DISCO_VM_HCS_BASE`) | ✅ macOS 27 guest, before ADR 26-10-23-417; ⬜ since (ported, cgo files reviewed by hand only) | ✅ fake API (2026-11-03): every subtest but a-guest-port, a-peer-forward, and the volumes', which boxd skips, nothing left behind; before ADR 26-11-03-606, `TestConformance` on boxd itself (2026-10-29, `computer:0.1.53`); ⬜ on boxd itself since | ✅ Docker 29.9.2 on Linux 7.1, fresh from debian:trixie (`DISCO_VM_DOCKER_IMAGE`, 2026-11-03, since ADR 26-11-03-868's restructure too): every subtest but the fenced, guest-port, and peer-forward ones, which docker skips; nothing left behind |
| nothing local: run, then exec/cp/stop/start/rm with a fresh credential each, and from another engine | n/a | n/a | n/a | ✅ by hand in a discobox (2026-10-08): exec and cp 5.5 minutes after `run`, once its sentinel had expired; stop, start, rm; a guest's own `poweroff`, then found through the shutdown hook, is not supported since ADR 26-10-15-833; ✅ by hand in a discobox (2026-10-23): one empty root ran boxd's default image and wrote a file, a second stopped the instance and snapshotted it as a tagged image, a third listed the image, ran it and read the file back, and `rmi` deleted the snapshots; every root stayed empty; ✅ fake API (`TestEngineRunsInstances`: a second engine made from nothing but the API lists the first's image and instance, stops it, and starts it) | ✅ by hand (2026-10-09): no shim process after `run`; exec, a guest's own `systemctl poweroff` found stopped by the next `ls`, start, stop, rm; ✅ `machinetest` start-attach-and-stop (2026-10-23) |
| e2e CLI lifecycle (`internal/e2e`) | ✅, including `drivers` and a `--driver` override, `commit`, `create --from`, and a fresh VM of `$DISCO_VM_BASE` (TestFresh); and the whole suite again on a fake that does not commit (`DISCO_VM_FAKE_NO_COMMIT`, in `task test`), every VM fresh and TestCommit skipped | ✅ `DISCO_VM_DRIVER=hcs`, before ADR 26-10-23-417; ⬜ since | ✅ `DISCO_VM_DRIVER=vz`, before ADR 26-10-23-417; ⬜ since | ⬜ (no layout for `DISCO_VM_DRIVER=boxd` yet; the same lifecycle passed by hand) | ✅ `DISCO_VM_DRIVER=docker` (Docker 29.9.2, 2026-11-03, in project `e2e`, the static binary as `DISCO_VM_TEST_BINARY` and `DISCO_VM_AGENT`): e2e-base a fresh VM of debian:trixie, committed; every test but TestKeys and TestFresh, which skip; nothing left behind |
| guest operations, `machine.Guest`: exec, files, shutdown, info (`machinetest` checks exec's stdin, stdout, stderr, environment, directory, and exit code) | ✅ through the agent, and from another process through the shim's relay (`pkg/engine` tests) | ✅ in-guest, through the agent; ⬜ the shim's relay | ✅ in-guest (vsock, launchd daemon), through the agent; ⬜ the shim's relay | ✅ boxd by hand in a discobox (2026-10-15), with no agent: `docs/examples/boxd.yaml` built with no `--agent` (apt as root, an orderly shutdown, a commit), and `exec` with `-e`, `-w`, `-i`, separate stderr, and an exit code; `cp` in (root's, a directory's mode kept) and out, and a missing path refused; `inspect`'s info; `stop` in order (then through the shutdown hook; since ADR 26-10-15-833 `sync` and `StopVm`, 2026-10-16: a 512 MiB file written just before `stop` read back after `start` with the same SHA-256), `start`, `rm`, `rmi`; an image the agent-based binary built ran, stopped, started, and was removed by the new one; ✅ fake API (`TestConformanceFakeAPI`, which reads back the driver's command lines; `TestCommand`, `TestCopyFromMissing`) | ✅ in-guest (systemd unit, unix socket through `docker exec`), through the agent |
| exec with a TTY | ✅ unix, ConPTY on the host | ✅ ConPTY in-guest | ✅ in-guest (`exec -t`) | ✅ boxd by hand (2026-10-15): `exec -t` has a pty at the terminal's 45 rows and 123 columns, and its exit code; ⬜ a resize (`window_change`), which the CLI does not send yet | ✅ in-guest (`exec -t`, the guest's own devpts) |
| exec as a user | ✅ unix | ✅ in-guest (LogonUser, elevated) | ✅ in-guest (a user added with sysadminctl: uid, HOME, groups); ⬜ Homebrew | ✅ boxd by hand (2026-10-15): `exec -u boxd` runs as boxd with its HOME, and with no user as root with root's HOME, USER, LOGNAME, and sudo's `secure_path` | ✅ in-guest (`useradd`, `exec -u`) |
| a VM whose chain was removed behind the driver's back does not boot, and is still removed (`pkg/machine/local` TestChainsKeepADiskAnotherNeeds) | ✅ (`local`, with a chaining stand-in) | ⬜ | n/a (no chain) | n/a | n/a |
| projects kept apart: a project lists and takes only its own VMs (`TestEngineKeepsProjectsApart`; the local driver's records carry the project) | ✅ | ⬜ | ⬜ | ✅ fake API (`TestEngineKeepsProjectsApart`: another project lists none of the default's, and cannot make an instance of its image; what boxd holds under no `dvm-` name is not listed); before ADR 26-10-23-417, by hand (2026-10-15): a project's `dvm-<project>--` machines and snapshots invisible to `default` | ⬜ |
| a fresh VM from the driver's base, its configuration (`machine.Config.Base`, `$DISCO_VM_BASE`), never a create's; refused where there is none (`machinetest` a-fresh-vm; `internal/e2e` TestFresh; `pkg/engine` TestInstallCommitAndCreateFrom) | ✅ a name the driver is given (`scratch` by default) | n/a (refused: no base) | ⬜ the Linux guest image (`DefaultLinuxBase`, or the configured one) | ✅ fake API (2026-11-03, `TestFreshVMsUseTheConfiguredImage`: the default image, or the configured one); before ADR 26-11-03-606, by hand on boxd (2026-10-16): a run of the default image in 6 s | ✅ (2026-11-03, Docker 29.9.2): `machinetest` and `internal/e2e` from debian:trixie, pulled and readied at Create; none configured refused (`TestNoBase`) |
| a stopped VM committed under its name, and a running one refused; a VM created from it, committed in turn, and a VM of the second runs with both writes after the first's removal, which a driver whose disks chain refuses with `ErrInUse`, and whose boot it refuses too (`Engine.Commit`, `internal/e2e` TestCommit; `machinetest` a-copy-of-a-copy; `pkg/machine/local` TestChainsKeepADiskAnotherNeeds; `pkg/engine` TestInstallCommitAndCreateFrom) | ✅ | ⬜ (`local`'s refusals are proven with a chaining stand-in on the fake) | ⬜ | ✅ fake API (2026-11-03: a snapshot `dvm-<project>--<name>`, the machine deleted; `TestCommittedVMsAreSnapshots`); ⬜ boxd itself: a snapshot named as a machine that still exists, and `GetVm` of a snapshot's ID | ✅ (2026-11-03, Docker 29.9.2): `machinetest` and `internal/e2e` TestCommit, the committed image's tag removed while a container of it runs |
| a driver without `Capabilities.Commit` (ADR 26-11-03-868): `info` reports `"commit": false`; `Commit` of a running VM and of a stopped one refused with `ErrUnsupported`, the VM left as it was; `Create` with a `From` refused, nothing made; the engine refuses both before the driver is asked (`machinetest` boot-write-commit; `pkg/engine` TestCommitNeedsADriverThatCommits; `internal/e2e` TestNoCommit: `commit`, `create --from`, `run --from`) | ✅ (`NoCommit`, 2026-11-03) | n/a (reports it) | n/a (reports it) | n/a (reports it) | n/a (reports it) |
| an issued key: issued with an expiry and listed without its secret; a driver opened with it creates, starts, copies into, stops, and removes a machine of the project; it cannot issue a key; once revoked it lists and creates nothing (`machinetest` an-issued-key; `internal/e2e` TestKeys) | ✅ (`machinetest`, `internal/e2e` TestKeys through `DISCO_VM_FAKE_KEY`) | n/a (`ErrNoKey`; ⬜ `machinetest` on Windows) | n/a (`ErrNoKey`; ⬜ `machinetest` on a Mac) | ✅ fake API (`TestConformanceFakeAPI`; `TestAnAPIKeyCannotIssue`, `TestIssuedKeysAreTheProjects`, `TestTheKeyIsAskedOnEveryCall`); by hand on boxd (2026-10-24): `CreateApiKey` and `ListApiKeys` refused to an API key with `PermissionDenied` ("requires an interactive session (`boxd login`)"), which `key issue` reports as `ErrCannotIssue`; ⬜ issuing from a `boxd login` session, and running with the issued key | n/a (`ErrNoKey`, by hand 2026-10-24) |
| isolation at create (`InstanceSpec.Isolated`, `Networks`) | refused | refused | refused | ✅ by hand on boxd (2026-10-24): `run --isolated` of the default image is `isolated` as boxd reports it; ✅ fake API (`TestIsolatedInstances`: from a base and an image, networks set on a restore, a snapshot of an isolated instance stays isolated); ⬜ `--network` on boxd itself | refused |
| a private URL, where an instance's peers reach its service (`Service.PrivateURL`, `Engine.PrivateURL`, `url --private`; `machinetest` start-attach-and-stop, `pkg/engine` TestURL, `internal/e2e` TestURL) | ✅ the URL: a host server reached through it, as a peer on the host's loopback would | n/a (none: refused) | n/a (none: refused) | ✅ by hand on boxd (2026-10-29, `computer:0.1.53`): `url --private` of a machine on a network was `http://dvm-vtest25--dvm-hub.boxd:7302`, which an isolated machine on that network fenced to `none` resolved and reached, and port 8080 at the same name, after a stop and start too, and from three restores of its snapshot; ✅ fake API (`TestServiceFakeAPI`: the URL on no networks, `http://<machine>.boxd:7302` on one) | ✅ by hand (2026-10-29, Docker 29.9, debian:trixie): `url --private` was the container's address at 7302, which docker reported; another instance reached the service there and port 8080 at the same address; refused once stopped |
| an egress fence at create, read back, kept across a boot, the spec's and not the image's (`InstanceSpec.Egress`, `run --egress`; `machinetest` a-fenced-instance; `internal/e2e` TestEgress refuses a fence a driver does not hold) | refused | refused | `none` alone, as no network device (ADR 26-10-31-990): ⬜ on a Mac; ✅ a list refused, by unit; the guest side under QEMU (row below) | ✅ fake API (`TestFencedInstances`: `["none"]` and a list, from a base and from a snapshot, set before the idle timers; a restore unfenced when none is asked; a read-back that does not match destroys the machine); ✅ by hand on boxd (2026-10-29): `run --isolated --network N --egress none` (no host-bound secrets in the org) reported egress `{}`, resolved no internet name, and could not reach an address (`https://1.1.1.1`, closed in the handshake), while it reached its peer; the same after a stop and a start; restores of its snapshot reported and enforced what each asked: `none`, `example.com` (example.com answered, github.com did not resolve), and no fence (both answered); ✅ `machinetest` a-fenced-instance on boxd itself (2026-10-29); ⬜ boxd itself refusing a read-back (the fake's alone) | refused |
| a root from before warm stages were removed: an instance prepared fast and never booted refused at start | n/a | ⬜ a fork ticket refused | ⬜ `resume.pending` refused | n/a (a stage's snapshot is not disco-vm's) | n/a (never warm) |
| unattended install from media (`disco-vm install NAME`, `Engine.Install`: a committed VM) | ✅ `internal/e2e`, `pkg/engine` | ✅ a build's `from.install` of a retail ISO, before ADR 26-10-23-410; ⬜ `disco-vm install` | ✅ IPSW download, install, provisioning, agent bootstrap, through a build's `from.install`; ⬜ `disco-vm install` | n/a, refused (a fresh VM of a boxd image instead) | n/a, refused (a fresh VM of a registry image instead, readied at Create) |
| `--gui` window (`run`, `start`, `install`) | refused | ✅ mstsc on the video console (an install's through the since-removed `build --gui`) | ✅ native window (likewise) | refused | refused |
| forward, guest to host (`CreateOptions.Forwards`; `pkg/engine` TestForward) | ✅ | ✅ in-guest, both directions (through the since-removed `run --forward`) | ✅ in-guest, both directions (through the since-removed `run --forward`) | refused | refused |
| service URL and service file (`Engine.URL`, `disco-vm url`) | ✅ a host server listening at the file's address through the shim's loopback URL, on two starts (`pkg/engine` TestURL, `internal/e2e` TestURL) | ⬜ (7302 in the ServiceTable: `TestDocumentService`) | ⬜ | ⬜ fixed port 7302 not run on boxd itself; earlier, a guest's HTTP server through the URL on two boots with an image-declared port (`TestService`, by hand, `DISCO_VM_INTEGRATION=1` and `BOXD_API_KEY`); ✅ fake API (`TestServiceFakeAPI`) | ✅ by hand (2026-10-15, Docker 29.9, debian:trixie): `url` refused while created, `http://127.0.0.1:N` once started, the service file `tcp:0.0.0.0:7302`; a static Go program copied in and run under `systemd-run`, listening with `guest.ListenService()`, answered through the URL, and again at a new port after stop and start; nothing left behind; ⬜ `internal/e2e` TestURL with `DISCO_VM_DRIVER=docker` |

| a Linux guest on vz, from a base, beside a macOS one, with no network device, its vsock reached (`TestConformanceLinux`, `TestLinuxBesideMacOS`; ADR 26-10-31-990) | n/a | n/a (later, the same base's amd64 variant) | ⬜ not run on a Mac (CI's macos job compiles and lints it); ✅ the base's guest side by hand under QEMU/KVM on Linux (2026-11-01, `linux/amd64`, built with `go tool task build:linux-guest`, booted with the kernel, the boot initrd `writeBootInitrd` made with a linux/amd64 agent appended, and the raw disk grown to 8 GiB, as vz boots it, with vhost-vsock for the socket device): with no NIC and `disco-vm.nic=0` the agent ran from `/run/disco-vm` and answered on vsock 7300 at protocol 1, `ls /sys/class/net` was `lo` alone, the system `running` with no failed unit, `network-online.target` reached in 7 ms with wait-online skipped, `/` grown to 7.6 GiB online, the machine ID the command line's, boot in 2.4 s, and an orderly shutdown through the agent powered it off; booted again on the same disk with a NIC and another ID, the new ID won over the one the first boot wrote, a DHCP lease came and names resolved, and wait-online ran and succeeded in 0.8 s; ⬜ the lease by MAC (QEMU's user network gives every guest one address) | n/a | n/a |
| guest ports (`Capabilities.Ports`): a guest port dialed through an attached machine, and a peer's port through a `Forward` to an instance, the peer answering with a file only it holds, nothing read while it is stopped, and the peer followed once it starts again (`machinetest` a-guest-port, a-peer-forward; `pkg/engine` TestDial; `internal/e2e` TestURL's `dial`) | ✅ | ⬜ | ⬜ | ✅ refused (`ErrUnsupported`, fake API) | refused (⬜ not run since) |
| a shim's control API on an owner-only Unix socket that `shim.json` names, and a shim from before it still attached at its loopback address (`pkg/machine/local` TestShimSocket) | ✅ | ⬜ | ⬜ | n/a | n/a |
| a data volume at `/.discobox/data` (`InstanceSpec.Data`; ADR 26-11-02-180): empty at Create, kept across a stop and a boot, never in a snapshot; refused where not attached (`machinetest` a-data-volume, `internal/e2e` TestVolumes, `pkg/engine` TestVolumesRefused) | ✅ a directory in the guest's root | n/a (refused: ⬜ not run on Windows) | n/a (refused: ⬜ not run on a Mac) | ✅ refused (fake API) | ✅ a host directory under `DISCO_VM_DOCKER_ROOT` (ADR 26-11-03-782; 2026-11-03, Docker 29.9.2, debian:trixie: `machinetest`, nothing left behind; a guest root's file is uid 16777216 on the host); before it, a named volume (2026-11-02, `machinetest` and `internal/e2e`) |
| a cache volume at `/.discobox/cache` (`InstanceSpec.Cache`, `SaveCache`, `Caches`, `DeleteCache`; ADR 26-11-02-180): a private clone of its key's generation, unseen by a live peer, published only by a save from a stopped instance, a guarded save refused once the key moved, another key and another project seeing nothing, a clone kept once its key is removed (`machinetest` a-cache-volume, `internal/e2e` TestVolumes, `pkg/engine` TestSaveCacheGuards) | ✅ | n/a (refused) | n/a (refused) | ✅ refused (fake API) | ✅ generations as host directories, copied on the daemon's host by a helper container under a lock on the key (2026-11-03, `machinetest`; `TestDirs` for the helper's operations); ⬜ two helpers racing on one key; ⬜ another project's, which the fake proves |
| an upgrade (`Driver.Upgrade`, `Engine.Upgrade`, `disco-vm upgrade`; ADR 26-11-03-782): a stopped VM given a committed VM's root, or a fresh one, keeps its name, settings, data volume, and cache clone, loses what it wrote to its old root, and is still named by an `Instance` from before; refused while running, from a VM the provider does not have, and where no data volume is attached; an interrupted one listed with its volumes, refused a boot and a commit, finished by upgrading again, or removed whole (`machinetest` an-upgrade, `internal/e2e` TestUpgrade, `pkg/engine` TestUpgrade, `pkg/machine/local` TestAnInterruptedUpgradeIsFinishedAgain) | ✅ (2026-11-03): `machinetest` from an install and fresh, `internal/e2e`, and an upgrade failed at each step and finished | n/a (refused: ⬜ not run on Windows) | n/a (refused: ⬜ not run on a Mac) | ✅ refused (fake API, `TestConformanceFakeAPI` an-upgrade: `ErrUnsupported`, the VM unchanged) | ✅ Docker 29.9.2 (2026-11-03, debian:trixie, a scratch `DISCO_VM_DOCKER_ROOT`): `machinetest` an-upgrade, fresh and from a committed VM; `TestUpgradeInterrupted`, failed after the new container is made and after the old one is removed, then finished, and removed whole; by hand, a VM made with named volumes by the binary before this change upgraded fresh, its data (guest uid 1000) and cache copied into directories and the volumes removed |
| a Linux guest image that assembles and verifies itself in its build (`go tool task build:linux-guest`, the `linux-guest` CI job) | ✅ `linux/amd64` by hand (2026-11-01) | n/a | ⬜ `linux/arm64` (builds under QEMU in `release:linux-guest`; not run here) | n/a | n/a |

A platform driver is done when `machinetest.Run` passes against it on real
hardware and `internal/e2e` passes with `DISCO_VM_DRIVER` set to it. See
[docs/drivers/hcs.md](docs/drivers/hcs.md), [docs/drivers/vz.md](docs/drivers/vz.md),
[docs/drivers/boxd.md](docs/drivers/boxd.md), and
[docs/drivers/docker.md](docs/drivers/docker.md). boxd's "fake API" column is
`TestConformanceFakeAPI`: the whole suite against an in-process fake of boxd's
API, which proves the driver's calls and the Exec relay but not boxd's
behavior.
