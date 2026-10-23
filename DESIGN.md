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
                  pkg/engine  ── instances, lifecycle, per-VM shim
                  pkg/build   ── YAML spec → layers, cached by inputs
                  pkg/image   ── layer store + name:tag
  ─────────────── machine.Driver seam: Machine is the guest's operations (machine.Guest) ───────────────
   machine/hcs (windows)  machine/vz (darwin)  machine/docker (any)  machine/fake  machine/boxd (any)
          │                     │                     │                   │              │
 HCS VM (vmcompute)    VZ VM (in-process)    container + userns     host process    boxd microVM
          │                     │               (Engine API)              │          (gRPC API)
      hvsocket                vsock             exec + `pipe`            tcp         Exec: sudo, tar,
          └──────── port 7300 ──┴─────────────────────┴───────────────────┘          systemctl; no
            disco-vm guest (the agent), reached with pkg/guest's client               agent at all
```

Everything above the seam is OS-neutral and is tested on every OS through the
fake driver. Above the seam a running guest is only `machine.Guest`: exec,
copy in and out, info, and an orderly shutdown (ADR 0001). How a driver
provides them is its own: the agent over a private channel, or boxd's API.
Everything below it is one package per hypervisor, selected by build tags,
so each binary contains only the drivers its OS can run (`pkg/machine/drivers`). boxd's hypervisor is in the cloud, behind an API, and
a Docker daemon may be on another host or in Docker Desktop's VM, so every
binary contains both.

## Where the design lives

Each package's `DESIGN.md` holds the decisions it owns:

- [pkg/machine](pkg/machine/DESIGN.md): the driver contract and capabilities,
  ports, windows, and cold clones. Its drivers:
  [boxd](pkg/machine/boxd/DESIGN.md) and [docker](pkg/machine/docker/DESIGN.md).
- [pkg/guest](pkg/guest/DESIGN.md): the agent protocol, HTTP between host and
  agent.
- [pkg/engine](pkg/engine/DESIGN.md): one shim per running VM, the service URL,
  projects, bases, and why disco-vm does not converge guests.
- [pkg/image](pkg/image/DESIGN.md): the image store, which is the build cache.
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

## Metadata is rows in a database, and disks are files

What disco-vm knows about a root is in a database opened with
`github.com/discobox-ai/x/gormdb`, as every discobox-ai project's is: layers,
tags, and replaced layers (`pkg/image`), and instances (`pkg/engine`). What a
driver writes stays in files under the root: a layer's `payload/`, an
instance's `machine/`, and a shim's `shim.json` and `shim.log`, which live
and die with one process.

- **The default is SQLite**, `<root>/disco-vm.db`. With no daemon, every
  command and every shim opens it, and SQLite's write lock is what serializes
  them.
- **An embedder passes its own database**
  (`engine.Open(root, driver, db, project)`), and the engine does not close
  it. Every table is named `disco_vm_*`, so none collides with the
  embedder's. The rows describe the files in one root, so one database serves
  one root, and every project in it.
- **Each package migrates its own tables** when it opens (`AutoMigrate`).
- **JSON files from before the database are imported on open** and removed
  once their rows are in: `layer.json`, `tags.json`, `replaced.json`, and
  `instance.json`, into the default project. A database from before projects
  is upgraded the same way: `disco_vm_tags` is copied into
  `disco_vm_image_tags` and dropped, and the unique index on an instance's
  name alone is dropped. A database from before warm stages were removed
  ([pkg/machine](pkg/machine/DESIGN.md#every-clone-is-cold-and-an-image-is-made-fast-by-what-is-in-it)) loses `disco_vm_warm_stages`, and keeps an instance's `mode`
  column, which nothing reads.
- **A row and its directory change in an order that can be repeated.** A
  layer's row is inserted in the transaction that renames its directory, and
  deleted before the directory is. An instance's row is inserted after the
  driver prepared it, and deleted after its directory is.

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
| machine conformance suite (`pkg/machine/machinetest`) | ✅ | ✅ Win 11 Pro guest | ✅ macOS 27 guest | ⬜ `TestConformance` on boxd itself with no agent (it passed by hand with the agent, `BOXD_API_KEY`; the same operations passed by hand through the CLI, below); ✅ fake API (Linux CI), exec included; ⬜ `attach` on boxd itself (fake API only) | ✅ Docker 29.8 on Linux 7.0, from debian:trixie and ubuntu:24.04 (by hand, `DISCO_VM_DOCKER_IMAGE`) |
| no shim: run, then exec/cp/stop/start/rm with a fresh credential each | n/a | n/a | n/a | ✅ by hand in a discobox (2026-10-08): exec and cp 5.5 minutes after `run`, once its sentinel had expired; stop, start, rm; a guest's own `poweroff`, then found through the shutdown hook, is not supported since ADR 26-10-15-833 | ✅ by hand (2026-10-09): no shim process after `run`; exec, a guest's own `systemctl poweroff` found stopped by the next `ls`, start, stop, rm; `machinetest` attach |
| e2e CLI lifecycle (`internal/e2e`) | ✅, including `drivers` and a `--driver` override | ✅ `DISCO_VM_DRIVER=hcs` | ✅ `DISCO_VM_DRIVER=vz` | ⬜ (not run with `DISCO_VM_DRIVER=boxd` yet; the same lifecycle passed by hand with `docs/examples/boxd.yaml`) | ✅ `DISCO_VM_DRIVER=docker` |
| guest operations, `machine.Guest`: exec, files, shutdown, info (`machinetest` checks exec's stdin, stdout, stderr, environment, directory, and exit code) | ✅ through the agent, and from another process through the shim's relay (`pkg/engine` tests) | ✅ in-guest, through the agent; ⬜ the shim's relay | ✅ in-guest (vsock, launchd daemon), through the agent; ⬜ the shim's relay | ✅ boxd by hand in a discobox (2026-10-15), with no agent: `docs/examples/boxd.yaml` built with no `--agent` (apt as root, an orderly shutdown, a commit), and `exec` with `-e`, `-w`, `-i`, separate stderr, and an exit code; `cp` in (root's, a directory's mode kept) and out, and a missing path refused; `inspect`'s info; `stop` in order (then through the shutdown hook; since ADR 26-10-15-833 `sync` and `StopVm`, 2026-10-16: a 512 MiB file written just before `stop` read back after `start` with the same SHA-256), `start`, `rm`, `rmi`; an image the agent-based binary built ran, stopped, started, and was removed by the new one; ✅ fake API (`TestConformanceFakeAPI`, which reads back the driver's command lines; `TestCommand`, `TestCopyFromMissing`) | ✅ in-guest (systemd unit, unix socket through `docker exec`), through the agent |
| exec with a TTY | ✅ unix, ConPTY on the host | ✅ ConPTY in-guest | ✅ in-guest (`exec -t`) | ✅ boxd by hand (2026-10-15): `exec -t` has a pty at the terminal's 45 rows and 123 columns, and its exit code; ⬜ a resize (`window_change`), which the CLI does not send yet | ✅ in-guest (`exec -t`, the guest's own devpts) |
| exec as a user | ✅ unix | ✅ in-guest (LogonUser, elevated) | ✅ in-guest (a user added with sysadminctl: uid, HOME, groups); ⬜ Homebrew | ✅ boxd by hand (2026-10-15): `exec -u boxd` runs as boxd with its HOME, and with no user as root with root's HOME, USER, LOGNAME, and sudo's `secure_path` | ✅ in-guest (`useradd`, `exec -u`) |
| a tag recorded where the driver keeps layers (`TagLayer`, `UntagLayer`) | no-op | no-op | no-op | ✅ boxd by hand in a discobox (2026-10-12): `tag` waited for the tag's snapshot to be ready, `run` from the tag, `rmi`; the snapshot was not read back with `boxd snap ls`; ✅ fake API (`TestTagSnapshot`, `machinetest`) | no-op |
| a build replaces a layer its driver lost, and rebuilds the layers after it (`LayerExists`) | ✅ `internal/e2e` TestLostLayer, `pkg/image` TestReplacedLayer | ⬜ | ⬜ | ✅ boxd by hand in a discobox (2026-10-13): a middle layer pointed at a snapshot name boxd does not have was replaced, the layer after it rebuilt, a third build was all cached, and `run` from the tag read both layers' files; no snapshot was actually deleted with `boxd snap rm`; ✅ fake API (`TestTagSnapshot`: `LayerExists`) | ⬜ |
| projects, and the driver's resources the store has no record of listed as `<orphan>` and removed by name (`Driver.Orphans`, `RemoveOrphan`; `pkg/engine` TestOpenUpgradesADatabaseFromBeforeProjects, `machinetest`) | ✅ projects kept apart in one database, and a database from before projects upgraded; none to list | ⬜ (none to list) | ⬜ (none to list) | ✅ boxd by hand in a discobox (2026-10-15): a build and an instance in project `vtest` made `dvm-vtest--` machines and snapshots, a tag's included; an empty root listed all three as orphans in `vtest` and none in `default`; `rm` and `rmi` left none; a snapshot a deleted default store left behind was listed; from an empty root, `rm` destroyed the stopped orphan machine and `rmi` both orphan snapshots by name, which the default project could not; ✅ fake API (`TestEngineListsOrphans`, `machinetest`): machines and snapshots under other names, and other projects', are not listed or removed, nor is the store's own snapshot; a second store lists every one of the first's | ✅ by hand (2026-10-15, Docker on Linux 7.0): a project's container and images named for it, invisible to `default` and to a project whose name is a prefix of it, and listed as orphans from an empty root; a container named before projects is the default's orphan, and an unlabeled `dvm-` container is not one; `rm` refused the running orphan container without `-f` and removed it with it, `rmi` removed both orphan images, and another project could not; `images -a` listed the intermediate layer; ✅ `machinetest` (`DISCO_VM_DOCKER_IMAGE=debian:trixie`) |
| a driver's bases listed, and an instance run from one with no layer; a build on a base, rebuilt for its new version; a snapshot of an instance of one; `tag` and `rmi` of a base refused (`Driver.Bases`, `InstanceSpec.Base`; `internal/e2e` TestBases; `machinetest` an-instance-of-a-base) | ✅ a catalog the test gives it | n/a (none) | n/a (none) | ✅ boxd by hand in a discobox (2026-10-16): `images` listed the default image (`europe-docker.pkg.dev/azin-console-prod/boxd/computer:0.1.52`) and no `dvm-` snapshot; `run` of it with no build in 6 s, systemd `running`, no disco-vm file in `system-shutdown/`; `stop` and `start` kept a file; `snapshot` of the instance made an image, and a cold `run` of it in 10 s read the file back; `tag` and `rmi` of the base refused; an empty root listed no orphans after; ⬜ on boxd itself: a hand-made snapshot as a base (the account had none), a base's machine made at a size asked for (not checked), a build on a base and its rebuild for a new version, and `machinetest`'s an-instance-of-a-base; ✅ fake API (`TestEngineRunsABase`: the default image and a hand-made snapshot listed, `dvm-` ones of every project not; no machine until the first start, which makes it from the snapshot, holding its disk, or from the image at the size asked; a snapshot of it runs; `machinetest` boots the default image and commits a layer from it) | n/a (none) |
| a stopped instance snapshotted as an image, consuming the instance; a running one, or a bad tag, refused with the instance kept; an instance of the snapshot snapshotted in turn (`Engine.Snapshot`, `internal/e2e` TestSnapshot) | ✅ | ⬜ | ⬜ | ⬜ | ✅ `DISCO_VM_DRIVER=docker` (2026-10-15, Docker 29.9 on Linux 7.0, debian:trixie): two generations of snapshots, each instance's files read back from the next |
| a root from before warm stages were removed: the table dropped, an instance made in resume mode kept, found by name and listed, and a new one added beside the old mode column (`pkg/engine` TestOpenDropsWarmStages); an instance prepared fast and never booted refused at start | ✅ the database | ⬜ a fork ticket refused | ⬜ `resume.pending` refused | ⬜ a stage's snapshot listed as an orphan (no new test; the generic orphan listing in `TestEngineListsOrphans` covers it) | n/a (never warm) |
| unattended install from media (`docs/examples/<os>.yaml`) | n/a | ✅ `docs/examples/windows.yaml` | ✅ IPSW download, install, provisioning, agent bootstrap | n/a (boots a boxd image) | n/a (an image from a registry, with systemd and the agent added and its owners shifted: `docs/examples/docker.yaml`) |
| `--gui` window (`run`, `start`, `build`) | refused | ✅ mstsc on the video console | ✅ native window | refused | refused |
| forward, guest to host (`CreateOptions.Forwards`; `pkg/engine` TestForward) | ✅ | ✅ in-guest, both directions (through the since-removed `run --forward`) | ✅ in-guest, both directions (through the since-removed `run --forward`) | refused | refused |
| service URL and service file (`Engine.URL`, `disco-vm url`) | ✅ a host server listening at the file's address through the shim's loopback URL, on two starts (`pkg/engine` TestURL, `internal/e2e` TestURL) | ⬜ (7302 in the ServiceTable: `TestDocumentService`) | ⬜ | ⬜ fixed port 7302 not run on boxd itself; earlier, a guest's HTTP server through the URL on two boots with an image-declared port (`TestService`, by hand, `DISCO_VM_INTEGRATION=1` and `BOXD_API_KEY`); ✅ fake API (`TestServiceFakeAPI`) | ✅ by hand (2026-10-15, Docker 29.9, debian:trixie): `url` refused while created, `http://127.0.0.1:N` once started, the service file `tcp:0.0.0.0:7302`; a static Go program copied in and run under `systemd-run`, listening with `guest.ListenService()`, answered through the URL, and again at a new port after stop and start; nothing left behind; ⬜ `internal/e2e` TestURL with `DISCO_VM_DRIVER=docker` |
| metadata in the database: a root's JSON files imported, an embedder's database shared (`pkg/image` TestOpenImportsJSONFiles, `pkg/engine` TestOpenImportsInstanceFiles and TestOpenInACallersDatabase) | ✅ SQLite, Linux; `internal/e2e` passes on it | ⬜ | ⬜ | ✅ fake API (`machinetest`, Linux); ⬜ boxd itself | ⬜ |

A platform driver is done when `machinetest.Run` passes against it on real
hardware and `internal/e2e` passes with `DISCO_VM_DRIVER` set to it. See
[docs/drivers/hcs.md](docs/drivers/hcs.md), [docs/drivers/vz.md](docs/drivers/vz.md),
[docs/drivers/boxd.md](docs/drivers/boxd.md), and
[docs/drivers/docker.md](docs/drivers/docker.md). boxd's "fake API" column is
`TestConformanceFakeAPI`: the whole suite against an in-process fake of boxd's
API, which proves the driver's calls and the Exec relay but not boxd's
behavior.
