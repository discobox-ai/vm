# disco-vm design

disco-vm builds OS images and runs them as VMs. It runs Windows guests on
Windows (Host Compute Service), macOS guests on macOS
(Virtualization.framework), and Linux guests in the cloud on boxd from any
host, or as Docker containers that boot systemd in a user namespace of their
own. There is one codebase, one binary per host OS, and one command line.
It is used two ways:

- **Standalone:** `disco-vm build | run | exec | cp | stop | rm ...`.
- **As a discobox sandbox provider:** discobox embeds `pkg/engine` as a
  library.

## The stack

```
  disco-vm CLI (internal/cli)           discobox provider (embeds the engine)
                 \                         /
                  pkg/engine  ── images, instances, lifecycle; no state
                  pkg/build   ── YAML spec → layers, cached by inputs
  ─────── machine.Driver seam: the provider's layers, tags, instances; Machine is machine.Guest ───────
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
  the build cache, projects, bases, and why disco-vm does not converge
  guests.
- [docs/build-spec.md](docs/build-spec.md): the build spec.

## One binary, three roles

`disco-vm` is the CLI. It is also the guest agent (`disco-vm guest`) and each
VM's supervisor (`disco-vm shim <id>`). The agent is the same program built for
the guest's GOOS. On a Windows host the guest is Windows, and on a macOS host
the guest is macOS, so the running binary is also the agent, and install bakes
in `os.Executable()`. A Docker guest is Linux, so from another host
`build --agent` names a disco-vm built for it. Protocol changes therefore ship
to both ends at once. A driver that reaches its guest without the agent (boxd)
does not list `Capabilities.Agent`, puts nothing of disco-vm's in the guest,
and its builds need no `--agent`.

## The engine keeps no state, and a driver keeps it in its provider

disco-vm's core keeps nothing: no database, no root, and no files (ADR
26-10-23-417). `engine.Open(driver, project)` is the whole of an engine, and
every image, tag, and instance is read from the driver, which reads it from
its provider every time, so any process with the provider's credentials sees
the same ones, on any host. The driver keeps what it knows on the provider's
own resources, in the provider's way:

- **docker:** labels, set when an image is committed or a container made
  (`ai.discobox.vm.*`: project, layer, parent, comment; an instance's name,
  image reference, and size), and names: a layer's image tag, an instance's
  container `dvm-<project>--<name>`. A container's layer is the one whose
  image it runs. Nothing is kept on the host.
- **boxd:** names, since boxd keeps no labels: a layer's snapshot and an
  instance's machine `dvm-<project>--<name>`, at most 63 characters, so a
  project and a name are 57 together; and what the API reports: a snapshot's
  creation, a machine's size, image, and creation. boxd keeps no parent or
  comment for a layer, and does not say which snapshot a machine was
  restored from, so a boxd instance has no layer and `rmi` does not wait for
  it; a restored machine outlives its snapshot (by hand, 2026-10-23).
  Nothing is kept on the host.
- **hcs, vz, fake:** this host's disk is the provider, kept by
  [pkg/machine/local](pkg/machine/local/DESIGN.md) in a directory of its own.

An instance's name is unique in its project the way its provider makes names
unique (a container's, a machine's, a claim file), and is 1 to 63 lowercase
letters, digits, and single hyphens (`machine.ValidateName`). A build's
instances are named `build--<random>`, which no other name can be, and
listings leave them out.

**Nothing from before is read** (ADR 26-10-23-417): disco-vm is pre-alpha,
so a root's `disco-vm.db` and the layers and instances it described, and
what a driver made under its old names, are not taken in. Remove them, or
keep using the disco-vm that made them.

`--debug` (`machine.SetDebug`) has a driver report every call it makes to a
service outside the process, boxd's API or the Docker daemon, and how long the
answer took.

## The build spec

This has its own document: [docs/build-spec.md](docs/build-spec.md). In short, it is
Dockerfile-shaped YAML (a base, then ordered steps, cached by prefix) with two
changes that OS images force:

- **Explicit layers.** A layer boundary is a full guest shutdown and disk
  commit, not a cheap filesystem diff, so the author places boundaries.
- **`when: {os: ...}` on layers and steps.** One spec, such as
  [docs/examples/discobox-base.yaml](docs/examples/discobox-base.yaml), describes the
  same batteries-included toolset on Windows and macOS. The base image decides
  which steps apply.

A layer is committed only from an orderly shutdown. A guest that has to be
forced off fails the build rather than caching a disk with unflushed writes.

## What is proven, and where

| | fake (all OSes, CI) | hcs | vz | boxd | docker |
|---|---|---|---|---|---|
| machine conformance suite (`pkg/machine/machinetest`): since ADR 26-10-23-417 it also finds, lists, and tags layers, finds and lists instances by a name no second one can take, and attaches to a started machine from another call | ✅ | ✅ Win 11 Pro guest, before ADR 26-10-23-417; ⬜ since | ✅ macOS 27 guest, before ADR 26-10-23-417; ⬜ since | ⬜ `TestConformance` on boxd itself with no agent (it passed by hand with the agent, `BOXD_API_KEY`; the same operations passed by hand through the CLI, below); ✅ fake API (Linux CI), exec included | ✅ Docker 29.9 on Linux 7.1, from debian:trixie (by hand, `DISCO_VM_DOCKER_IMAGE`, 2026-10-23): every subtest but a base's, which docker has none of; before ADR 26-10-23-417, Docker 29.8 from debian:trixie and ubuntu:24.04 |
| nothing local: run, then exec/cp/stop/start/rm with a fresh credential each, and from another engine | n/a | n/a | n/a | ✅ by hand in a discobox (2026-10-08): exec and cp 5.5 minutes after `run`, once its sentinel had expired; stop, start, rm; a guest's own `poweroff`, then found through the shutdown hook, is not supported since ADR 26-10-15-833; ✅ by hand in a discobox (2026-10-23): one empty root ran boxd's default image and wrote a file, a second stopped the instance and snapshotted it as a tagged image, a third listed the image, ran it and read the file back, and `rmi` deleted the snapshots; every root stayed empty; ✅ fake API (`TestEngineRunsInstances`: a second engine made from nothing but the API lists the first's image and instance, stops it, and starts it) | ✅ by hand (2026-10-09): no shim process after `run`; exec, a guest's own `systemctl poweroff` found stopped by the next `ls`, start, stop, rm; ✅ `machinetest` start-attach-and-stop (2026-10-23) |
| e2e CLI lifecycle (`internal/e2e`) | ✅, including `drivers` and a `--driver` override | ✅ `DISCO_VM_DRIVER=hcs`, before ADR 26-10-23-417; ⬜ since | ✅ `DISCO_VM_DRIVER=vz`, before ADR 26-10-23-417; ⬜ since | ⬜ (not run with `DISCO_VM_DRIVER=boxd` yet; the same lifecycle passed by hand with `docs/examples/boxd.yaml`) | ✅ `DISCO_VM_DRIVER=docker` (Docker 29.9, 2026-10-23, in project `e2e`, nothing left behind) |
| guest operations, `machine.Guest`: exec, files, shutdown, info (`machinetest` checks exec's stdin, stdout, stderr, environment, directory, and exit code) | ✅ through the agent, and from another process through the shim's relay (`pkg/engine` tests) | ✅ in-guest, through the agent; ⬜ the shim's relay | ✅ in-guest (vsock, launchd daemon), through the agent; ⬜ the shim's relay | ✅ boxd by hand in a discobox (2026-10-15), with no agent: `docs/examples/boxd.yaml` built with no `--agent` (apt as root, an orderly shutdown, a commit), and `exec` with `-e`, `-w`, `-i`, separate stderr, and an exit code; `cp` in (root's, a directory's mode kept) and out, and a missing path refused; `inspect`'s info; `stop` in order (then through the shutdown hook; since ADR 26-10-15-833 `sync` and `StopVm`, 2026-10-16: a 512 MiB file written just before `stop` read back after `start` with the same SHA-256), `start`, `rm`, `rmi`; an image the agent-based binary built ran, stopped, started, and was removed by the new one; ✅ fake API (`TestConformanceFakeAPI`, which reads back the driver's command lines; `TestCommand`, `TestCopyFromMissing`) | ✅ in-guest (systemd unit, unix socket through `docker exec`), through the agent |
| exec with a TTY | ✅ unix, ConPTY on the host | ✅ ConPTY in-guest | ✅ in-guest (`exec -t`) | ✅ boxd by hand (2026-10-15): `exec -t` has a pty at the terminal's 45 rows and 123 columns, and its exit code; ⬜ a resize (`window_change`), which the CLI does not send yet | ✅ in-guest (`exec -t`, the guest's own devpts) |
| exec as a user | ✅ unix | ✅ in-guest (LogonUser, elevated) | ✅ in-guest (a user added with sysadminctl: uid, HOME, groups); ⬜ Homebrew | ✅ boxd by hand (2026-10-15): `exec -u boxd` runs as boxd with its HOME, and with no user as root with root's HOME, USER, LOGNAME, and sudo's `secure_path` | ✅ in-guest (`useradd`, `exec -u`) |
| a tag kept where the driver keeps layers, set, moved, listed, and dropped (`Tags`, `Tag`, `Untag`; `machinetest` tag-move-and-untag) | ✅ a file | ⬜ a file | ⬜ a file | ✅ fake API (`TestTagsAreSnapshots`: a second snapshot `dvm-<project>--<layer>--t-<ref>`, the ref read back, a move deleting the other layer's); before ADR 26-10-23-417, by hand (2026-10-12): `tag` waited for the tag's snapshot to be ready, `run` from the tag, `rmi` | ✅ an image tag `disco-vm/<project>/<ref>` (`machinetest`, Docker 29.9, 2026-10-23) |
| a layer its driver lost built again under its key, and a local layer built on a parent made again since built again too (`internal/e2e` TestLostLayer) | ✅ | ⬜ | ⬜ | ✅ fake API (`TestTagsAreSnapshots`: a layer whose snapshot was deleted is not found) | ⬜ |
| projects kept apart: a project lists and takes only its own layers, tags, and instances (`TestEngineKeepsProjectsApart`; the local driver's records carry the project) | ✅ | ⬜ | ⬜ | ✅ fake API (`TestEngineKeepsProjectsApart`: another project lists none of the default's, and cannot make an instance of its layer; what boxd holds under no `dvm-` name is not listed); before ADR 26-10-23-417, by hand (2026-10-15): a project's `dvm-<project>--` machines and snapshots invisible to `default` | ⬜ |
| a driver's bases listed, and an instance run from one with no layer; a build on a base, rebuilt for its new version; a snapshot of an instance of one; `tag` and `rmi` of a base refused (`Driver.Bases`, `InstanceSpec.Base`; `internal/e2e` TestBases; `machinetest` an-instance-of-a-base) | ✅ a catalog the test gives it | n/a (none) | n/a (none) | ✅ boxd by hand in a discobox (2026-10-16): `images` listed the default image (`europe-docker.pkg.dev/azin-console-prod/boxd/computer:0.1.52`) and no `dvm-` snapshot; `run` of it with no build in 6 s, systemd `running`, no disco-vm file in `system-shutdown/`; `stop` and `start` kept a file; `snapshot` of the instance made an image, and a cold `run` of it in 10 s read the file back; `tag` and `rmi` of the base refused; an empty root listed no orphans after; ⬜ on boxd itself: a hand-made snapshot as a base (the account had none), a base's machine made at a size asked for (not checked), a build on a base and its rebuild for a new version, and `machinetest`'s an-instance-of-a-base; ✅ fake API (`TestEngineRunsABase`: the default image and a hand-made snapshot listed, `dvm-` ones of every project not; the machine made at create, stopped, from the snapshot, holding its disk, or from the image at the size asked; a snapshot of it runs; `machinetest` boots the default image and commits a layer from it) | n/a (none) |
| a stopped instance snapshotted as an image, consuming the instance; a running one, or a bad tag, refused with the instance kept; an instance of the snapshot snapshotted in turn (`Engine.Snapshot`, `internal/e2e` TestSnapshot) | ✅ | ⬜ | ⬜ | ⬜ | ✅ `DISCO_VM_DRIVER=docker` (2026-10-15, Docker 29.9 on Linux 7.0, debian:trixie): two generations of snapshots, each instance's files read back from the next |
| a root from before warm stages were removed: an instance prepared fast and never booted refused at start | n/a | ⬜ a fork ticket refused | ⬜ `resume.pending` refused | n/a (a stage's snapshot is not disco-vm's) | n/a (never warm) |
| unattended install from media (`docs/examples/<os>.yaml`) | n/a | ✅ `docs/examples/windows.yaml` | ✅ IPSW download, install, provisioning, agent bootstrap | n/a (boots a boxd image) | n/a (an image from a registry, with systemd and the agent added and its owners shifted: `docs/examples/docker.yaml`) |
| `--gui` window (`run`, `start`, `build`) | refused | ✅ mstsc on the video console | ✅ native window | refused | refused |
| forward, guest to host (`CreateOptions.Forwards`; `pkg/engine` TestForward) | ✅ | ✅ in-guest, both directions (through the since-removed `run --forward`) | ✅ in-guest, both directions (through the since-removed `run --forward`) | refused | refused |
| service URL and service file (`Engine.URL`, `disco-vm url`) | ✅ a host server listening at the file's address through the shim's loopback URL, on two starts (`pkg/engine` TestURL, `internal/e2e` TestURL) | ⬜ (7302 in the ServiceTable: `TestDocumentService`) | ⬜ | ⬜ fixed port 7302 not run on boxd itself; earlier, a guest's HTTP server through the URL on two boots with an image-declared port (`TestService`, by hand, `DISCO_VM_INTEGRATION=1` and `BOXD_API_KEY`); ✅ fake API (`TestServiceFakeAPI`) | ✅ by hand (2026-10-15, Docker 29.9, debian:trixie): `url` refused while created, `http://127.0.0.1:N` once started, the service file `tcp:0.0.0.0:7302`; a static Go program copied in and run under `systemd-run`, listening with `guest.ListenService()`, answered through the URL, and again at a new port after stop and start; nothing left behind; ⬜ `internal/e2e` TestURL with `DISCO_VM_DRIVER=docker` |

A platform driver is done when `machinetest.Run` passes against it on real
hardware and `internal/e2e` passes with `DISCO_VM_DRIVER` set to it. See
[docs/drivers/hcs.md](docs/drivers/hcs.md), [docs/drivers/vz.md](docs/drivers/vz.md),
[docs/drivers/boxd.md](docs/drivers/boxd.md), and
[docs/drivers/docker.md](docs/drivers/docker.md). boxd's "fake API" column is
`TestConformanceFakeAPI`: the whole suite against an in-process fake of boxd's
API, which proves the driver's calls and the Exec relay but not boxd's
behavior.
