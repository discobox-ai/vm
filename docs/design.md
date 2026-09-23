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
| clone modes | cold, **fork** (live template, many clones, about 1s) | cold, **resume** (saved state, once per identity) | cold |
| max running | none | **2 macOS guests** (framework limit) | none |
| shared dirs | (Plan9, later) | virtiofs | no |
| display | guestfb over hvsocket | framework view | no |

The engine enforces `MaxRunning`. `--mode fork|resume` is refused where it is
unsupported.

### 3. Ports, not GUIDs

The guest agent listens on **port 7300** everywhere. On vz that is AF_VSOCK. On
hcs it is the hvsocket service ID `00001C84-FACB-11E6-BD58-64006A7986D3`, which
is the vsock template GUID with the port in its first field
(`winio.VsockServiceID`). The hcs driver must list that ID in the VM's HvSocket
ServiceTable. 7301 is reserved for the display stream. Because callers only
ever say "port", the shim, the engine, and a discobox provider never need to
know which hypervisor they are talking to.

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
too (a template instance), not a special daemon.

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
never leaves a half-layer under a real ID.

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
| machine conformance suite (`pkg/machine/machinetest`) | ✅ | ⬜ | ⬜ |
| e2e CLI lifecycle (`internal/e2e`) | ✅ | ⬜ | ⬜ |
| agent: exec, files, shutdown, info | ✅ | ⬜ in-guest | ⬜ in-guest |
| agent: TTY | ✅ unix | ⬜ ConPTY | ✅ unix |
| agent: run as user | ✅ unix | ⬜ | ✅ unix |

A platform driver is done when `machinetest.Run` passes against it on real
hardware and `internal/e2e` passes with `DISCO_VM_DRIVER` set to it. See
[drivers/hcs.md](drivers/hcs.md) and [drivers/vz.md](drivers/vz.md).
