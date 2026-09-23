# disco-vm as a discobox sandbox provider

This is a proposal. It depends on discobox ADR 0126, which is still proposed.

## Where it plugs in

Every discobox provider today (`docker`, `vz`, `libkrun`, `wslc`,
`digitalocean`, `execvm`) implements `dockerworker.Driver`. That means: bring
up one Linux host per pool that runs Docker, then run each sandbox as a
container on it. A Windows or macOS guest cannot be that host. There is no
Docker daemon inside one to give each sandbox, and a vz macOS guest has no
nested virtualization.

ADR 0126 ("Remote sandboxes connect out to their pool") is the seam that fits.
Under it:

- The pool keeps one pool environment: pool-agent, proxy, BuildKit, registry,
  and Git origins.
- The pool agent creates **one provider environment per sandbox** through a
  backend interface: create, inspect/list, start, stop, delete, transfer, and
  connection.

disco-vm is one implementation of that interface, with one VM per sandbox.

| 0126 backend operation | disco-vm |
|---|---|
| create | `engine.Create(image, {Name: sandboxID, Mode})`, then `Start` |
| inspect / list | `engine.Get` / `List` and `State`; instance names carry the sandbox ID, so a lost create response is recovered by name, never duplicated |
| start / stop | `engine.Start` / `Stop` (orderly, through the guest agent) |
| delete | `engine.Remove(force)` |
| transfer | `guest.Client.CopyTo` / `CopyFrom` (tar) |
| connection | `engine.Dial(inst, port)`: a byte stream to any guest port, through the shim |

## Where the engine runs

The pool agent runs inside the Linux pool VM (vz on macOS, wslc on Windows) and
cannot call Virtualization.framework or HCS. So the backend runs on the **host
side**, in the discobox server, which embeds `pkg/engine` as a library, the way
the vz provider embeds Code-Hex/vz today. The pool agent asks the control
plane, and the server calls the engine.

The engine's per-VM shims mean a server restart does not kill running
sandboxes. Discobox's rule that "VMs die with the server" can then become a
choice rather than a necessity.

## What discobox must add

1. **A "native guest" sandbox kind.** 0126 assumes each environment boots the
   Linux harness OCI image with sandbox-agent as PID 1 under systemd. A native
   guest instead:
   - boots a disco-vm image (such as `discobox/base`, built from
     `examples/discobox-base.yaml`)
   - runs sandbox-agent as a **service**, not as PID 1
   - keeps `/.discobox/{data,cache,config,sources,secrets}` as plain
     directories inside the guest, with no bind mounts
2. **sandbox-agent on windows and darwin.** Its exec, PTY, services, and meta
   APIs are what discobox's server and CLI already speak. Its `procio` and
   `terminal` packages already have Windows code paths. The Linux-only PID 1
   work (volume binds, uid partitions, systemd) moves behind build tags.
   disco-vm's own agent stays as the lifecycle and bootstrap channel: it
   shuts the guest down, copies files, and installs and upgrades sandbox-agent.
3. **Capacity in placement.** `Capabilities.MaxRunning` (2 macOS guests per
   Mac) has to reach scheduling, or a third macOS sandbox fails at start.

## What stays the same

- The pool is still the credential and egress boundary (its proxy), the build
  cache, and the Git origin.
- Server and CLI requests still enter through pool-local routes. The
  connection is transport only, and sandbox-agent still authorizes every
  request.
