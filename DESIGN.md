# disco-vm design

disco-vm builds OS images and runs them as VMs. It runs Windows guests on
Windows (Host Compute Service), macOS guests on macOS
(Virtualization.framework), and Linux guests in the cloud on boxd from any
host, or as Docker containers that boot systemd in a user namespace of their
own. There is one codebase, one binary per host OS, and one command line. It is used two ways:

- **Standalone:** `disco-vm build | run | exec | cp | stop | rm ...`.
- **As a discobox sandbox provider:** discobox embeds `pkg/engine` as a
  library. See [docs/discobox.md](docs/discobox.md).

## The stack

```
  disco-vm CLI (internal/cli)           discobox provider (embeds the engine)
                 \                         /
                  pkg/engine  ── instances, lifecycle, per-VM shim
                  pkg/build   ── YAML spec → layers, cached by inputs
                  pkg/image   ── layer store + name:tag
                  pkg/converge ── state files → a guest's packages, files, services, by exec and copy
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
provides them is its own: the agent over a private channel, or boxd's API. Everything below it is one package per hypervisor, selected by
build tags, so each binary contains only the drivers its OS can run
(`pkg/machine/drivers`). boxd's hypervisor is in the cloud, behind an API, and
a Docker daemon may be on another host or in Docker Desktop's VM, so every
binary contains both.

## Decisions

### 1. One binary, three roles

`disco-vm` is the CLI. It is also the guest agent (`disco-vm guest`) and each
VM's supervisor (`disco-vm shim <id>`). The agent is the same program built for
the guest's GOOS. On a Windows host the guest is Windows, and on a macOS host
the guest is macOS, so the running binary is also the agent, and install bakes
in `os.Executable()`. A Docker guest is Linux, so from another host
`build --agent` names a disco-vm built for it. Protocol changes therefore ship
to both ends at once. A driver that reaches its guest without the agent (boxd)
does not list `Capabilities.Agent`, puts nothing of disco-vm's in the guest,
and its builds need no `--agent`.

### 2. The driver contract is the intersection, and differences are reported

`machine.Driver` is Install, Prepare (clone), Boot, Commit, Destroy, and
Service (decision 3b). Boot returns once the guest answers. A running
`Machine` is the guest's operations (`machine.Guest`: Exec with stdio, a TTY
and its resizes, a user, environment, and directory; CopyTo and CopyFrom, as
tar; Info; Shutdown), and Kill and Done. A driver without a shim's URL to
serve keeps its channel to the guest to itself; the others' machines are
`machine.Dialer`s, which the shim dials (decision 3b). Anything only one
hypervisor can do is a `Capabilities` field, never a faked method:

| capability | hcs | vz | boxd | docker | fake |
|---|---|---|---|---|---|
| guest OS | windows | darwin | linux | linux | host's |
| clone modes | cold, **fork** (live template, many clones, about 1s) | cold, **resume** (two saved templates, any number of clones) | cold, **resume** (a snapshot, any number of clones) | cold (a cold boot is under a second) | cold, resume (pre-copied roots) |
| max running | none | **2 macOS guests** (framework limit) | none (the org's quota) | none | none |
| shared dirs | (Plan9, later) | virtiofs | no | no (needs an idmapped mount) | no |
| display | video console in mstsc (guestfb over hvsocket, later) | framework view in a native window | no | no | no |
| forward (guest to host) | hvsocket to the parent partition | vsock to the host, CID 2 | no | no | loopback |
| remote (no shim; any process attaches) | no | no | **yes** | **yes** | no |
| agent (Install bakes in `disco-vm guest`) | yes | yes | **no**: boxd's Exec | yes | yes |

The engine enforces `MaxRunning`. `--mode fork|resume` is refused where it is
unsupported. A fast mode also needs a warm image (decision 7), so it is a
property of an image on this host, not only of the driver.

### 3. Ports, not GUIDs

Where there is an agent, it listens on **port 7300**. On vz that is AF_VSOCK.
On hcs it is the hvsocket service ID `00001C84-FACB-11E6-BD58-64006A7986D3`,
which is the vsock template GUID with the port in its first field
(`winio.VsockServiceID`). The hcs driver must list that ID in the VM's HvSocket
ServiceTable. On docker it listens on a root-only unix socket, which
`disco-vm pipe` reaches through docker exec. boxd has no socket from the host
into the guest and no agent: its guest operations are command lines over
boxd's authenticated Exec stream (decision 8). 7301 is reserved for the
display stream. A port is a driver's own detail: the shim, the engine, and a
discobox provider see only `machine.Guest`, and the shim dials one port, the
service's.

The other direction is a port too. A guest process connects out to the host
with `guest.DialHost(port)` (vsock to CID 2 on vz, hvsocket to the parent
partition on HCS), and an instance created with `Forwards` has its shim accept
those connections (`machine.HostListener`) and splice each into a Unix socket on
the host, passing half-closes through. That is how a guest reaches a host
service with no TCP listener anywhere, as libkrun maps a vsock port to a Unix
socket. `disco-vm dial-host PORT` does the same from a shell in the guest.

### 3a. A window is per boot, and lives with the VM

`run --gui`, `start --gui`, and `build --gui` ask for a window on the guest's
display. It is a property of one boot, not of the instance: the engine passes
`BootOptions.GUI` (and `InstallSpec.GUI` for an install's boots), and the
driver opens the window in the process that called `Boot`, which is the one
that owns the VM. For `run` and `start` that is the shim (`shim --gui`); for a
build it is the CLI. The window closes when the VM stops, and closing it leaves
the VM running. A driver without `Capabilities.Display` refuses GUI with
`ErrUnsupported` rather than ignoring it.

A native window on macOS must run on the process's main thread, which only
`main` can give away. So `main` locks its goroutine to the main thread and runs
the program through `machine.Main`. A driver that shows windows installs a
`machine.MainLoop` there; vz brings AppKit up on the main thread only when the
first window is asked for, so no other command (and not the guest agent) ever
touches it.

### 3b. Every instance has one service, at one HTTP URL

The driver, not the caller or the image, picks where an instance's one
service listens in the guest (`Driver.Service`, `machine.Service`;
[ADR 0005](docs/adr/0005-every-instance-has-one-http-url-and-the-driver-picks-its-port.md)):
`vsock:7302` on vz and hcs (`guest.ServicePort`, listed in every hcs VM's
ServiceTable), `tcp:0.0.0.0:7302` on boxd and docker, and a loopback port
picked at Prepare on fake, whose guests share the host's. A build spec
declares no service.

At every start, before it returns, the engine writes that address with the
guest's CopyTo to its `guest.ServiceFile` (`/var/lib/disco-vm/service`, or
`C:\ProgramData\disco-vm\service`), on disk so it outlives a reboot in the
guest. A program the caller copies in reads it, or calls
`guest.ListenService()`, and listens there.

`Engine.URL` and `disco-vm url INSTANCE` give the service's URL, always HTTP:

- **The driver's**, where it has one: boxd's `https://<machine>.boxd.sh`,
  pinned to 7302 at Prepare and the same across stop and start; Docker's
  `http://127.0.0.1:N`, a port the daemon publishes at each start.
- **The shim's** everywhere else: `http://127.0.0.1:N`, a loopback port the
  shim binds at each start and splices, connection by connection, into
  `machine.Dialer`'s `Dial(7302)`. It is in `shim.json`, and there is none
  while the instance is stopped.

No URL takes a credential, so the service authenticates its callers.

### 4. HTTP between host and agent

Where a driver has an agent, the agent protocol is HTTP/1.1
(`pkg/guest/proto.go`), as every discobox guest service already is.
`guest.Client` is that driver's `machine.Guest`, and the shim serves the same
protocol from its machine (`guest.Relay`) to other processes:

| endpoint | purpose |
|---|---|
| `GET /v1/health`, `GET /v1/info` | readiness, OS/arch/hostname, the root of a guest that is a directory |
| `POST /v1/exec` | upgrades to a framed stream: stdin, resize, stdout, stderr, exit |
| `GET`/`PUT /v1/files` | tar out of and into the guest |
| `POST /v1/shutdown` | orderly power-off or reboot |

The guest, not the hypervisor, powers itself off: the agent where there is
one, systemd on boxd. A vz stop request reaches macOS as a power-button press,
and macOS answers it by sleeping. HCS has no graceful path for a stock Windows
guest at all. So Kill is only the fallback.

The agent is a boot-start service (SYSTEM on Windows, root launchd daemon on
macOS). It is up before any user logs on, and steps can drop to a user with
`user:`. On Windows it builds each process's environment from the registry
(`CreateEnvironmentBlock`), so a toolchain installed by one build step is on
PATH for the next.

### 5. No daemon: one shim process per running VM

The two hypervisors disagree about who owns a running VM. A
Virtualization.framework VM dies with the process that created it, and an HCS
VM belongs to vmcompute. A **shim per VM** makes both look the same:

- `start` spawns `disco-vm shim <id>` detached (setsid, or DETACHED_PROCESS).
- The shim boots the VM, holds it for its whole life, and exits when the VM
  stops.
- The shim writes `instances/<id>/shim.json` (pid, loopback address, bearer
  token) once its machine answers, and serves `/status`, `/stop`, and
  `/guest`. `/guest` upgrades to the agent protocol, which the shim answers
  from its machine's guest operations (`guest.Relay`): `Engine.Guest`.
- Any process (a later CLI invocation, a discobox server) reaches any instance
  through the shim. A crash is contained to one VM.
- The shim exits when the VM stops.

A **remote driver** (`Capabilities.Remote`: boxd, docker) runs no shim: its
service (boxd's API, the Docker daemon) owns the VM, so each call attaches
(`machine.Attacher`) with its own credentials. See
[docs/drivers/boxd.md](docs/drivers/boxd.md) and
[docs/drivers/docker.md](docs/drivers/docker.md).

HCS's live-template fork needs something to hold the template. That is a shim
too, a **warm shim** (`disco-vm shim --warm <layer>`), not a special daemon. See
decision 7.

A program that embeds the engine (discobox) starts its shims as a subcommand of
its own binary: `Engine.ShimCommand` names it, and that subcommand calls
`Engine.ShimMain`. So an embedder ships one binary, and its VMs outlive it.

### 6. The image store is the build cache

A layer is an immutable, committed disk state owned by a driver:

- hcs: a differencing VHDX
- vz: an APFS-cloned bundle
- boxd: a boxd snapshot, named in `layer.json` and deleted with the layer
- docker: a Docker image committed from the stopped guest, named in
  `layer.json` and deleted with the layer
- fake: a directory tree

A layer's **ID is its cache key**. The key is a hash of the parent ID, driver,
guest OS, and the fully resolved steps, including a content digest of every
copied file. An existing layer is never rebuilt, and `--no-cache` salts the
keys instead of overwriting. Tags name layers. A layer with no tag
and no child is an untagged image: `images` lists it as `<none>` (`images -a`
lists every untagged layer, a chain's intermediate layers too) and `rmi`
takes its ID. `rmi` untags, then
deletes layers that nothing else tags, parents, or runs, as `docker rmi` does.
A layer is written under a temporary name, then renamed into place and
recorded in one transaction, so a crash never leaves a half-layer under a
committed ID. Installation media a driver
downloads itself (`media: latest`) is not a layer; it is kept in
`cache/<driver>/` (`InstallSpec.CacheDir`), so it is fetched once per host. A
driver may hold a layer outside its directory (a boxd snapshot), so the store
has the driver release a layer whenever it drops one: on `rmi`, on an aborted
build, and when another build published the same layer first. What the
driver holds can also be deleted behind the store's back (`boxd snap rm`,
`docker rmi`), so a build asks the driver whether each cached layer is still
there (`LayerExists`). One that is gone is built again under a new ID, salted
as `--no-cache` salts one, and the store records that the new layer now
answers for the old one's inputs. A new ID is what makes every layer after it
build again: theirs were made from the disk that is gone, and steps are not
reproducible. Nothing is removed on the driver's word. The lost layer and the
images built on it stay until `rmi`, untagged once the build moves their tags.
For the same
reason a tag is told to the driver (`TagLayer`, `UntagLayer`): on boxd a tag
is a snapshot of its own, named for the layer and the tag, so the account's
snapshot listing says which snapshots are which images. Tags are still read
only from the store.

### 6a. Metadata is rows in a database, and disks are files

What disco-vm knows about a root is in a database opened with
`github.com/discobox-ai/x/gormdb`, as every discobox-ai project's is: layers,
tags, and replaced layers (`pkg/image`), instances and warm requests
(`pkg/engine`). What a driver writes stays in files under the root: a layer's
`payload/`, an instance's and a stage's `machine/`, and a shim's `shim.json`
and `shim.log`, which live and die with one process.

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
  once their rows are in: `layer.json`, `tags.json`, `replaced.json`,
  `instance.json`, and `warm.json`, into the default project. A database
  from before projects is upgraded the same way: `disco_vm_tags` is copied
  into `disco_vm_image_tags` and dropped, and the unique index on an
  instance's name alone is dropped.
- **A row and its directory change in an order that can be repeated.** A
  layer's row is inserted in the transaction that renames its directory, and
  deleted before the directory is. An instance's row is inserted after the
  driver prepared it, and deleted after its directory is.

`--debug` (`machine.SetDebug`) has a driver report every call it makes to a
service outside the process, boxd's API or the Docker daemon, and how long the
answer took.

### 6b. A project scopes everything, and the driver's leftovers are listed

`--project` (`$DISCO_VM_PROJECT`, `Engine.Project`; `default` when not given; ADR 0004)
names the project a command works in. A project is 1 to 32 lowercase letters,
digits, and single hyphens. Every row has one: a project's `images`, `ls`,
names, and tags are its own, and an instance's name is unique within it.
Projects share a root's directories, so a project other than the default is
part of every layer's cache key, and two projects never share a layer ID. The
default project's keys are those from before projects, so its cache stays
valid. A shim is started with the engine's project.

The driver puts the project in the name of everything it makes outside the
store (`machine.ResourcePrefix`): boxd's machines and snapshots and docker's
containers are `dvm-<project>--...`, and docker's layer images are tagged
`disco-vm-layer:<project>--...`. `--` cannot occur in a project, so a name
belongs to exactly one project. A name from before projects, with no `--`,
is the default project's.

`ls` and `images` ask the driver every time for what it holds under the
project's names that the store has no record of (`Driver.Orphans`, given
`machine.Known`): a store that was deleted, a crash between making something
and recording it, another host's store, or something being made right now,
such as an install's machine. They are listed as `<orphan>` rows, stopped or
not, so nothing billed is out of sight. `rmi NAME` deletes an orphaned layer
and `rm NAME` an orphaned machine (`Engine.RemoveOrphan`,
`Driver.RemoveOrphan`), each only after asking the driver again that NAME is
an orphan of the project now, so nothing the store records and nothing of
another project's is deleted this way; a running machine takes `-f`. A
driver whose resources
live only in the store's directories (hcs, vz, fake) has none.

### 7. Fast clones come from warm images, and auto falls back to cold

Neither fast mode works from a committed layer alone. A resume needs saved
machine states, and a fork needs a live template frozen in memory. So staging
is its own step, and each image on each host is either warm or cold:

```
disco-vm warm IMAGE [--count N] [--cpus N] [--memory SIZE]
disco-vm warm --rm IMAGE
```

- **The driver picks how to stage** (`machine.Warmer`). hcs freezes one
  template that forks any number of clones (`Warmth.Clones` is -1). vz saves
  two templates, one per guest the framework can run at once, and each resume
  clone is an APFS clone of whichever one no running VM holds, so it too serves
  any number (-1). A saved state restores only under the identity it was saved
  with (measured: a new machine identifier or MAC is "invalid argument"), and
  two running guests must not share one, which two templates guarantee. The
  fake driver stages a pool of `--count` clones that is used up, and `warm`
  tops it back up.
- **`--mode auto` is the default** for `run` and `create`. It picks the mode
  the image is warm for, and cold when the stage is used up, missing, stale, or
  sized differently from the request (a clone runs at its stage's CPUs and
  memory). A stage that disappears between choosing and taking (`ErrNotWarm`
  from Prepare) also falls back to cold. The instance records the mode it
  actually got, and `ls` shows it. An explicit `--mode fork|resume` is strict:
  it fails, and says to run `warm`.
- **Staging is explicit, never a side effect of `run`.** Making a stage costs
  a full cold boot, so the run that triggered it would gain nothing. A stage
  also holds real resources: a template holds its VM's memory for as long as
  it lives, and a saved state is memory-sized on disk. `images` shows what each
  image has staged, and `rmi` refuses a warm image until it is cooled.
- **A stage is not a layer.** A layer is immutable and its ID is a content
  hash. A stage is tied to this host, can go stale (a host OS update
  invalidates vz states), and a pool (fake) is used up by clones. It lives in
  `warm/<layer>/` (the driver's `machine/`; what was asked for is a row), and drivers see
  it as `WarmSpec.Dir` and `InstanceSpec.WarmDir`.
- **Warming runs in a warm shim.** A resident stage must stay in the process
  that made it, so `warm` spawns `disco-vm shim --warm <layer>`. A stage on
  disk (vz, fake) lets the shim exit once it is written. A resident one (hcs)
  keeps the shim serving `/status` and `/stop` until `warm --rm`.
- **A stage can log in a user** (`warm --user NAME[:UID]`, or `--local-user`
  for the host's own account, with the same name and uid, so files on a shared
  directory have one owner on both sides). The user is part of the stage, like
  its size, and `WarmSpec.User` asks the driver for it; a driver that cannot
  make one returns `ErrUnsupported` (fake does). A cold clone of the image does
  not have the user.
- **The builder always clones cold.** A build starts from exactly the
  committed disk, and it must not spend stages meant for instances.

Later, and not built yet: refilling a resume pool as clones use it, `run
--warm` (boot cold now, stage in the background), `build --warm`, and an idle
TTL for templates.

### 8. boxd: a layer is a snapshot, and the guest is reached through Exec

boxd offers machines and snapshots of running machines, not disk images, and
no socket into a guest. The boxd driver maps the seam onto what it has, with
nothing of disco-vm's in the guest but a shutdown hook. See
[docs/drivers/boxd.md](docs/drivers/boxd.md).

- **A layer is a snapshot** (memory and disk) of the guest booted from the
  committed disk. Commit starts the stopped instance and snapshots it, because
  boxd only snapshots a running machine. The layer is still cut from an
  orderly shutdown: the snapshot's disk is the one the guest flushed.
- **A cold clone** restores the layer's snapshot and powers it off in order,
  so its first boot is a cold boot of that disk. **A resume clone**
  restores the stage's snapshot and suspends it until Boot. A snapshot
  restores any number of times, so resume never runs out (`Clones` is -1),
  and warming at the layer's own size costs nothing.
- **The guest's operations are Exec streams.** Each is one command line run
  as boxd's own user: `sudo -n -H -u USER -- env -C DIR -- K=V... ARGV...`,
  every word quoted, root unless the request names a user. Stdin, stdout,
  stderr, the exit code, and a TTY with its size and resizes are Exec's own.
  A copy in is `tar -x` reading the stream, and a copy out writes one ready
  byte once it has found the path and then `tar -c`, so a missing path is an
  error, not an empty tar. Info is `uname -m` and `hostname`. Boot, Commit,
  and Install wait until `systemctl is-system-running --wait` answers, so a
  command runs in a booted system. boxd authenticates every stream. An image
  installed before this carries an agent that nothing reaches; it does no
  harm.
- **The URL is the machine's default proxy.** Prepare pins the default proxy
  at `<machine>.boxd.sh` to 7302 and turns off bot protection, which would
  put a browser challenge in front of a request that wakes the machine.
  Pinning it also stops the proxy forwarding to whichever common port the
  guest listens on (80, 443, 8080, 8000, 3000, 5000, 5173).
- **A guest cannot power its machine off.** boxd leaves a halted guest
  `running`, so an orderly shutdown (`systemctl --no-block poweroff`) ends
  with the driver stopping the machine through the API. A systemd shutdown hook marks the moment the
  guest's disks are synced, and the driver waits for that mark.
- **The driver talks to boxd's public gRPC API directly**, not through the
  boxd CLI: it works from a server that embeds the engine, from any host OS,
  and with an API key (`BOXD_API_KEY`) rather than a browser login.

### 9. docker: a container that boots systemd in its own user namespace

A Linux guest can be a container whose init is systemd, in a user namespace
the driver makes per container, since Docker offers one only daemon-wide. See
[pkg/machine/docker/DESIGN.md](pkg/machine/docker/DESIGN.md).

### 10. A client converges a guest to state files

State files declare packages, files, services, and guarded scripts
([docs/state-file.md](docs/state-file.md),
[ADR 0003](docs/adr/0003-a-client-converges-a-guest-to-state-files.md)).
`pkg/converge` reads them on the client and makes a running guest match them
through exec and copy alone (`Engine.Converge`, `disco-vm converge`, and a
build's `converge:` step).

- **Nothing is installed in the guest for it.** The logic is the client's
  version, whatever the age of the guest. A script is copied in as a file and
  run with the shell the guest OS has: sh, or PowerShell on Windows. The
  scripts are text in `pkg/converge`, chosen by the OS the guest reports.
- **It applies a difference, from a record the guest holds.** Each resource
  has a digest, and the guest keeps the digests applied and the state file
  each came from. A resource whose digest is recorded is not looked at.
- **Runs add up.** A run applies the state files it is given, in name order,
  each one whole: files, packages, `exec`, services. A later state file's
  declaration of a resource replaces an earlier one's, as a later image
  layer's file does.
- **An image is a guest that was booted, converged, and snapshotted.** The
  layer is committed with the record in it, so converging an instance to its
  image's state files applies nothing.
- **A run holds a lease on the guest**, a minute long and renewed every
  thirty seconds, so two clients cannot converge one guest. A run that finds
  it held fails at once; one that loses it, or its guest, stops.
- **A guest that is a directory** (the fake driver) reports its root, and a
  run in it writes packages, services, and owners to a ledger instead of
  changing the host.

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
| no shim: run, then exec/cp/stop/start/rm with a fresh credential each | n/a | n/a | n/a | ✅ by hand in a discobox (2026-10-08): exec and cp 5.5 minutes after `run`, once its sentinel had expired; stop, start, a guest's own `poweroff` found stopped by the next `ls`, rm | ✅ by hand (2026-10-09): no shim process after `run`; exec, a guest's own `systemctl poweroff` found stopped by the next `ls`, start, stop, rm; `machinetest` attach |
| e2e CLI lifecycle (`internal/e2e`) | ✅ | ✅ `DISCO_VM_DRIVER=hcs` | ✅ `DISCO_VM_DRIVER=vz` | ⬜ (not run with `DISCO_VM_DRIVER=boxd` yet; the same lifecycle passed by hand with `docs/examples/boxd.yaml`) | ✅ `DISCO_VM_DRIVER=docker` |
| guest operations, `machine.Guest`: exec, files, shutdown, info (`machinetest` checks exec's stdin, stdout, stderr, environment, directory, and exit code) | ✅ through the agent, and from another process through the shim's relay (`pkg/engine` tests) | ✅ in-guest, through the agent; ⬜ the shim's relay | ✅ in-guest (vsock, launchd daemon), through the agent; ⬜ the shim's relay | ✅ boxd by hand in a discobox (2026-10-15), with no agent: `docs/examples/boxd.yaml` built with no `--agent` (apt as root, an orderly shutdown, a commit), and `exec` with `-e`, `-w`, `-i`, separate stderr, and an exit code; `cp` in (root's, a directory's mode kept) and out, and a missing path refused; `inspect`'s info; `stop` in order, `start`, a guest's own `systemctl poweroff` found stopped, `warm` and a resumed `run` in 8 s, `rm`, `rmi`; an image the agent-based binary built ran, stopped, started, and was removed by the new one; ✅ fake API (`TestConformanceFakeAPI`, which reads back the driver's command lines; `TestCommand`, `TestCopyFromMissing`) | ✅ in-guest (systemd unit, unix socket through `docker exec`), through the agent |
| exec with a TTY | ✅ unix, ConPTY on the host | ✅ ConPTY in-guest | ✅ in-guest (`exec -t`) | ✅ boxd by hand (2026-10-15): `exec -t` has a pty at the terminal's 45 rows and 123 columns, and its exit code; ⬜ a resize (`window_change`), which the CLI does not send yet | ✅ in-guest (`exec -t`, the guest's own devpts) |
| exec as a user | ✅ unix | ✅ in-guest (LogonUser, elevated) | ✅ in-guest (a user added with sysadminctl: uid, HOME, groups); ⬜ Homebrew | ✅ boxd by hand (2026-10-15): `exec -u boxd` runs as boxd with its HOME, and with no user as root with root's HOME, USER, LOGNAME, and sudo's `secure_path` | ✅ in-guest (`useradd`, `exec -u`) |
| a tag recorded where the driver keeps layers (`TagLayer`, `UntagLayer`) | no-op | no-op | no-op | ✅ boxd by hand in a discobox (2026-10-12): `tag` waited for the tag's snapshot to be ready, `run` from the tag, `rmi`; the snapshot was not read back with `boxd snap ls`; ✅ fake API (`TestTagSnapshot`, `machinetest`) | no-op |
| a build replaces a layer its driver lost, and rebuilds the layers after it (`LayerExists`) | ✅ `internal/e2e` TestLostLayer, `pkg/image` TestReplacedLayer | ⬜ | ⬜ | ✅ boxd by hand in a discobox (2026-10-13): a middle layer pointed at a snapshot name boxd does not have was replaced, the layer after it rebuilt, a third build was all cached, and `run` from the tag read both layers' files; no snapshot was actually deleted with `boxd snap rm`; ✅ fake API (`TestTagSnapshot`: `LayerExists`) | ⬜ |
| projects, and the driver's resources the store has no record of listed as `<orphan>` and removed by name (`Driver.Orphans`, `RemoveOrphan`; `pkg/engine` TestOpenUpgradesADatabaseFromBeforeProjects, `machinetest`) | ✅ projects kept apart in one database, and a database from before projects upgraded; none to list | ⬜ (none to list) | ⬜ (none to list) | ✅ boxd by hand in a discobox (2026-10-15): a build and an instance in project `vtest` made `dvm-vtest--` machines and snapshots, a tag's included; an empty root listed all three as orphans in `vtest` and none in `default`; `rm` and `rmi` left none; a snapshot a deleted default store left behind was listed; from an empty root, `rm` destroyed the stopped orphan machine and `rmi` both orphan snapshots by name, which the default project could not; ✅ fake API (`TestEngineListsOrphans`, `machinetest`): machines and snapshots under other names, and other projects', are not listed or removed, nor is the store's own snapshot; a second store lists every one of the first's | ✅ by hand (2026-10-15, Docker on Linux 7.0): a project's container and images named for it, invisible to `default` and to a project whose name is a prefix of it, and listed as orphans from an empty root; a container named before projects is the default's orphan, and an unlabeled `dvm-` container is not one; `rm` refused the running orphan container without `-f` and removed it with it, `rmi` removed both orphan images, and another project could not; `images -a` listed the intermediate layer; ✅ `machinetest` (`DISCO_VM_DOCKER_IMAGE=debian:trixie`) |
| unattended install from media (`docs/examples/<os>.yaml`) | n/a | ✅ `docs/examples/windows.yaml` | ✅ IPSW download, install, provisioning, agent bootstrap | n/a (boots a boxd image) | n/a (an image from a registry, with systemd and the agent added and its owners shifted: `docs/examples/docker.yaml`) |
| warm, auto, fast clones (`machinetest` warm, `internal/e2e` TestWarm) | ✅ resume | ✅ fork | ✅ resume from two templates, any number of clones (`TestTemplates`; agent answers about 6 s after Boot; needs an unlocked screen, and a locked one falls back to cold, `TestResumeOrFallBack`) | ✅ resume (`machinetest` on boxd) | n/a (cold only) |
| `--gui` window (`run`, `start`, `build`) | refused | ✅ mstsc on the video console | ✅ native window | refused | refused |
| forward, guest to host (`CreateOptions.Forwards`; `pkg/engine` TestForward) | ✅ | ✅ in-guest, both directions (through the since-removed `run --forward`) | ✅ in-guest, both directions (through the since-removed `run --forward`) | refused | refused |
| service URL and service file (`Engine.URL`, `disco-vm url`) | ✅ a host server listening at the file's address through the shim's loopback URL, on two starts (`pkg/engine` TestURL, `internal/e2e` TestURL) | ⬜ (7302 in the ServiceTable: `TestDocumentService`) | ⬜ | ⬜ fixed port 7302 not run on boxd itself; earlier, a guest's HTTP server through the URL on two boots with an image-declared port (`TestService`, by hand, `DISCO_VM_INTEGRATION=1` and `BOXD_API_KEY`); ✅ fake API (`TestServiceFakeAPI`) | ✅ by hand (2026-10-15, Docker 29.9, debian:trixie): `url` refused while created, `http://127.0.0.1:N` once started, the service file `tcp:0.0.0.0:7302`; a static Go program copied in and run under `systemd-run`, listening with `guest.ListenService()`, answered through the URL, and again at a new port after stop and start; nothing left behind; ⬜ `internal/e2e` TestURL with `DISCO_VM_DRIVER=docker` |
| metadata in the database: a root's JSON files imported, an embedder's database shared (`pkg/image` TestOpenImportsJSONFiles, `pkg/engine` TestOpenImportsInstanceFiles and TestOpenInACallersDatabase) | ✅ SQLite, Linux; `internal/e2e` passes on it | ⬜ | ⬜ | ✅ fake API (`machinetest`, Linux); ⬜ boxd itself | ⬜ |
| converge, from the client through exec and copy: a build's `converge` step, a no-op against an image's own state files, a changed state file, removal, a failing run, the lease (`internal/e2e` TestConverge, `pkg/converge`) | ✅ files, exec; packages and services in a ledger; sh on Linux, and the PowerShell scripts under pwsh 7 on Linux by hand (`DISCO_VM_TEST_PWSH`), ⬜ Windows PowerShell 5.1 | ⬜ | ⬜ | ⬜ | ✅ by hand (2026-10-13, debian:13-slim): discobox's base, sandbox, and claude-code images built from their state files (300 resources: apt from four third-party repositories, npm, 72 masked units, source directories, `exec` with a `shell`, a harness's file replacing the base's) in 10.5 minutes, with nothing of converge's in the guest but its record; the image matched the one its Dockerfiles build in package names and in the hash of every file the image places; an instance booted `running`; converging it to its image's state files applied nothing in 0.4 s; a running sandbox-base guest became the claude-code harness in 4 s; a second client was refused while the first held the lease, still refused after the first had renewed past a minute, and had the guest a minute after the first was killed |
| warm with a user (`warm --user`, `--local-user`) | refused | refused | ✅ resumed into the user's session, at its uid, with passwordless sudo (`TestWarmUser`) | refused | n/a (cold only) |

A platform driver is done when `machinetest.Run` passes against it on real
hardware and `internal/e2e` passes with `DISCO_VM_DRIVER` set to it. See
[docs/drivers/hcs.md](docs/drivers/hcs.md), [docs/drivers/vz.md](docs/drivers/vz.md),
[docs/drivers/boxd.md](docs/drivers/boxd.md), and
[docs/drivers/docker.md](docs/drivers/docker.md). boxd's "fake API" column is
`TestConformanceFakeAPI`: the whole suite against an in-process fake of boxd's
API, which proves the driver's calls and the Exec relay but not boxd's
behavior.
