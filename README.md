# disco-vm

Build OS images and run them as VMs, from one codebase and one command line:

- **Windows guests on Windows**, through Host Compute Service. There is no
  Hyper-V manager, no Windows Sandbox, and no WSL, so it works on Home.
- **macOS guests on macOS**, through Virtualization.framework.

disco-vm works on its own, and it is also meant to become a
[discobox](https://github.com/discobox-ai/discobox) sandbox provider (see
[docs/discobox.md](docs/discobox.md)).

> **Status: the shell.** Everything above the hypervisor is built and tested on
> every OS through a `fake` driver. That covers the CLI, the YAML image
> builder and its layer cache, the image store, instance lifecycle with a shim
> per VM, and the guest agent and its protocol. The `hcs` and `vz` drivers are
> stubs; their briefs are [docs/drivers/hcs.md](docs/drivers/hcs.md) and
> [docs/drivers/vz.md](docs/drivers/vz.md).

```sh
disco-vm build -f examples/windows.yaml --build-arg ISO=D:\iso\Win11.iso   # OS image
disco-vm build -f examples/discobox-base.yaml                              # + toolchains
disco-vm run --name dev discobox/base:2026.09
disco-vm exec -t dev powershell
disco-vm cp ./project dev:C:\src
disco-vm stop dev && disco-vm rm dev
```

## Images are YAML

The build spec is Dockerfile-shaped: a base, then steps, cached by prefix. It
adds explicit layers, because committing an OS disk is a full shutdown, and
per-OS conditions, so one spec describes the same toolset on Windows and macOS:

```yaml
name: discobox/base
from:
  image: ${OS_IMAGE}
args:
  OS_IMAGE: discobox/windows:11
layers:
  - name: toolchains
    steps:
      - when: {os: darwin}
        user: admin
        run: brew install git node go
      - when: {os: windows}
        run: ./install-toolchains.ps1
```

The reference is [docs/build-spec.md](docs/build-spec.md).

## Try it without a hypervisor

The `fake` driver runs the guest agent as a host process, with a directory
standing in for the disk:

```sh
go build ./cmd/disco-vm
DISCO_VM_DRIVER=fake ./disco-vm info
go test ./...          # includes the full CLI lifecycle on the fake driver
```

## Layout

| path | what |
|---|---|
| `cmd/disco-vm` | the one binary: CLI, guest agent (`guest`), per-VM supervisor (`shim`) |
| `pkg/machine` | the driver seam; `fake`, `hcs` (windows), `vz` (darwin); `machinetest` conformance suite |
| `pkg/engine` | instances and lifecycle; the shim |
| `pkg/build` | YAML spec, plan, cache keys, builder |
| `pkg/image` | layer store and tags |
| `pkg/guest` | agent protocol, server, and host client |
| `internal/e2e` | the CLI end to end |

The design and its reasoning are in [docs/design.md](docs/design.md).
