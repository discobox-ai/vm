# disco-vm design

disco-vm builds OS images and runs them as VMs. It runs Windows guests on
Windows (Host Compute Service) and macOS guests on macOS
(Virtualization.framework). There is one codebase, one binary per host OS, and
one command line. It is used two ways:

- **Standalone:** `disco-vm build | run | exec | cp | stop | rm ...`.
- **As a discobox sandbox provider:** discobox embeds `pkg/engine` as a
  library. See [discobox.md](discobox.md).

## The stack

```
  disco-vm CLI (internal/cli)           discobox provider (embeds the engine)
                 \                         /
                  pkg/engine  ── instances, lifecycle, per-VM shim
                  pkg/build   ── YAML spec → layers, cached by inputs
                  pkg/image   ── layer store + name:tag
                  pkg/guest   ── agent protocol, host client  ◄──── same code ────┐
  ────────────────────────────── machine.Driver seam ──────────────────────────── │
       machine/hcs (windows)    machine/vz (darwin)    machine/fake (any OS)      │
            │                         │                        │                  │
       HCS VM (vmcompute)       VZ VM (in-process)       a host process           │
            └──────── hvsocket / vsock / tcp: port 7300 ───────┘                  │
                                   disco-vm guest (the agent) ◄───────────────────┘
```

Everything above the seam is OS-neutral and is tested on every OS through the
fake driver. Everything below it is one package per hypervisor, selected by
build tags, so each binary contains only the drivers its OS can run
(`pkg/machine/drivers`).

## Decisions

### 1. One binary, three roles

`disco-vm` is the CLI. It is also the guest agent (`disco-vm guest`) and each
VM's supervisor (`disco-vm shim <id>`). The agent is the same program built for
the guest's GOOS. On a Windows host the guest is Windows, and on a macOS host
the guest is macOS, so the running binary is also the agent, and install bakes
in `os.Executable()`. Protocol changes therefore ship to both ends at once.

### 2. The driver contract is the intersection, and differences are reported

`machine.Driver` is Install, Prepare (clone), Boot, Commit, Destroy. A running
`Machine` offers Dial(port), Kill, and Done. Anything only one hypervisor can do
is a `Capabilities` field, never a faked method:

| capability | hcs | vz | fake |
|---|---|---|---|
| guest OS | windows | darwin | host's |
| clone modes | cold, **fork** (live template, many clones, about 1s) | cold, **resume** (two saved templates, any number of clones) | cold, resume (pre-copied roots) |
| max running | none | **2 macOS guests** (framework limit) | none |
| shared dirs | (Plan9, later) | virtiofs | no |
| display | video console in mstsc (guestfb over hvsocket, later) | framework view in a native window | no |
| forward (guest to host) | hvsocket to the parent partition | vsock to the host, CID 2 | loopback |

The engine enforces `MaxRunning`. `--mode fork|resume` is refused where it is
unsupported. A fast mode also needs a warm image (decision 7), so it is a
property of an image on this host, not only of the driver.

### 3. Ports, not GUIDs

The guest agent listens on **port 7300** everywhere. On vz that is AF_VSOCK. On
hcs it is the hvsocket service ID `00001C84-FACB-11E6-BD58-64006A7986D3`, which
is the vsock template GUID with the port in its first field
(`winio.VsockServiceID`). The hcs driver must list that ID in the VM's HvSocket
ServiceTable. 7301 is reserved for the display stream. Because callers only
ever say "port", the shim, the engine, and a discobox provider never need to
know which hypervisor they are talking to.

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

### 4. HTTP between host and guest

The agent protocol is HTTP/1.1 (`pkg/guest/proto.go`), as every discobox guest
service already is:

| endpoint | purpose |
|---|---|
| `GET /v1/health`, `GET /v1/info` | readiness, OS/arch/hostname |
| `POST /v1/exec` | upgrades to a framed stream: stdin, resize, stdout, stderr, exit |
| `GET`/`PUT /v1/files` | tar out of and into the guest |
| `POST /v1/shutdown` | orderly power-off or reboot |

The agent, not the hypervisor, powers the guest off. A vz stop request reaches
macOS as a power-button press, and macOS answers it by sleeping. HCS has no
graceful path for a stock Windows guest at all. So Kill is only the fallback.

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
  token) and serves `/status`, `/stop`, and `/dial?port=N`. `/dial` upgrades
  and splices bytes to any guest port.
- Any process (a later CLI invocation, a discobox server) reaches any instance
  through the shim. A crash is contained to one VM.

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
- fake: a directory tree

A layer's **ID is its cache key**. The key is a hash of the parent ID, driver,
guest OS, and the fully resolved steps, including a content digest of every
copied file. An existing layer is never rebuilt, and `--no-cache` salts the
keys instead of overwriting. Tags (`tags.json`) name layers. `rmi` untags, then
deletes layers that nothing else tags, parents, or runs, as `docker rmi` does.
A layer is written under a temporary name and renamed into place, so a crash
never leaves a half-layer under a real ID. Installation media a driver
downloads itself (`media: latest`) is not a layer; it is kept in
`cache/<driver>/` (`InstallSpec.CacheDir`), so it is fetched once per host.

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
  actually got, and `ps` shows it. An explicit `--mode fork|resume` is strict:
  it fails, and says to run `warm`.
- **Staging is explicit, never a side effect of `run`.** Making a stage costs
  a full cold boot, so the run that triggered it would gain nothing. A stage
  also holds real resources: a template holds its VM's memory for as long as
  it lives, and a saved state is memory-sized on disk. `images` shows what each
  image has staged, and `rmi` refuses a warm image until it is cooled.
- **A stage is not a layer.** A layer is immutable and its ID is a content
  hash. A stage is tied to this host, can go stale (a host OS update
  invalidates vz states), and a pool (fake) is used up by clones. It lives in
  `warm/<layer>/` (`warm.json` plus the driver's `machine/`), and drivers see
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

## The build spec

This has its own document: [build-spec.md](build-spec.md). In short, it is
Dockerfile-shaped YAML (a base, then ordered steps, cached by prefix) with two
changes that OS images force:

- **Explicit layers.** A layer boundary is a full guest shutdown and disk
  commit, not a cheap filesystem diff, so the author places boundaries.
- **`when: {os: ...}` on layers and steps.** One spec, such as
  [examples/discobox-base.yaml](../examples/discobox-base.yaml), describes the
  same batteries-included toolset on Windows and macOS. The base image decides
  which steps apply.

A layer is committed only from an orderly shutdown. A guest that has to be
forced off fails the build rather than caching a disk with unflushed writes.

## What is proven, and where

| | fake (all OSes, CI) | hcs | vz |
|---|---|---|---|
| machine conformance suite (`pkg/machine/machinetest`) | ✅ | ✅ Win 11 Pro guest | ✅ macOS 27 guest |
| e2e CLI lifecycle (`internal/e2e`) | ✅ | ✅ `DISCO_VM_DRIVER=hcs` | ✅ `DISCO_VM_DRIVER=vz` |
| agent: exec, files, shutdown, info | ✅ | ✅ in-guest | ✅ in-guest (vsock, launchd daemon) |
| agent: TTY | ✅ unix, ConPTY on the host | ✅ ConPTY in-guest | ✅ in-guest (`exec -t`) |
| agent: run as user | ✅ unix | ✅ in-guest (LogonUser, elevated) | ✅ in-guest (a user added with sysadminctl: uid, HOME, groups); ⬜ Homebrew |
| unattended install from media (`examples/<os>.yaml`) | n/a | ✅ `examples/windows.yaml` | ✅ IPSW download, install, provisioning, agent bootstrap |
| warm, auto, fast clones (`machinetest` warm, `internal/e2e` TestWarm) | ✅ resume | ✅ fork | ✅ resume from two templates, any number of clones (`TestTemplates`; agent answers about 6 s after Boot; needs an unlocked screen, and a locked one falls back to cold, `TestResumeOrFallBack`) |
| `--gui` window (`run`, `start`, `build`) | refused | ✅ mstsc on the video console | ✅ native window |
| forward, guest to host (`internal/e2e` TestForward) | ✅ | ✅ in-guest, both directions (hcs TestForward: `dial-host` from `cmd`; the e2e test is POSIX-only, so `run --forward` to a host socket is not yet run on Windows) | ✅ in-guest, both directions |
| warm with a user (`warm --user`, `--local-user`) | refused | refused | ✅ resumed into the user's session, at its uid, with passwordless sudo (`TestWarmUser`) |

A platform driver is done when `machinetest.Run` passes against it on real
hardware and `internal/e2e` passes with `DISCO_VM_DRIVER` set to it. See
[drivers/hcs.md](drivers/hcs.md) and [drivers/vz.md](drivers/vz.md).
