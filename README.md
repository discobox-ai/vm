# disco-vm

Build OS images and run them as VMs, from one codebase and one command line:

- **Windows guests on Windows**, through Host Compute Service. There is no
  Hyper-V manager, no Windows Sandbox, and no WSL, so it works on Home.
- **macOS guests on macOS**, through Virtualization.framework.
- **Linux guests in the cloud**, on [boxd](https://boxd.sh) microVMs, from
  any host.
- **Linux guests as Docker containers that look like machines**: systemd is
  their init, and their root is root only over the guest, as an unprivileged
  uid on the host, without `--privileged`.

disco-vm works on its own, and it is also meant to become a
[discobox](https://github.com/discobox-ai/discobox) sandbox provider (see
[docs/discobox.md](docs/discobox.md)).

> **Status: the shell.** Everything above the hypervisor is built and tested on
> every OS through a `fake` driver. That covers the CLI, the YAML image
> builder and its layer cache, the image store, instance lifecycle with a shim
> per VM, and the guest agent and its protocol. The `vz` driver runs macOS 27
> guests on Apple silicon, including resume clones and a native window
> (`--gui`) ([docs/drivers/vz.md](docs/drivers/vz.md)). The `hcs` driver's
> brief is [docs/drivers/hcs.md](docs/drivers/hcs.md). The `boxd` driver passes
> the conformance suite against boxd itself, and `docs/examples/boxd.yaml` builds and
> runs on it by hand; `internal/e2e` does not run on it yet (see
> [docs/drivers/boxd.md](docs/drivers/boxd.md)). The `docker` driver passes the
> conformance suite and `internal/e2e` against a local Docker daemon, and
> `docs/examples/docker.yaml` builds on it (see
> [docs/drivers/docker.md](docs/drivers/docker.md)).

```sh
disco-vm build -f docs/examples/windows.yaml -t discobox/windows:11 --build-arg ISO=D:\iso\Win11.iso   # OS image
disco-vm build -f docs/examples/discobox-base.yaml -t discobox/base                                    # + toolchains
disco-vm run --name dev discobox/base
disco-vm exec -t dev powershell
disco-vm cp ./project dev:C:\src
disco-vm stop dev && disco-vm rm dev
```

## Images are YAML

The build spec is Dockerfile-shaped: a base, then steps, cached by prefix. It
adds explicit layers, because committing an OS disk is a full shutdown, and
per-OS conditions, so one spec describes the same toolset on Windows and macOS:

```yaml
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

## Run macOS guests

On an Apple silicon Mac, build through `go tool task build`, which signs the binary with the
virtualization entitlement a VM needs:

```sh
go tool task build
build/disco-vm build -f docs/examples/macos.yaml -t discobox/macos:27     # downloads the restore image once, ~5 min after that
build/disco-vm run --name mac discobox/macos:27
build/disco-vm exec mac sw_vers
build/disco-vm run --gui --name desk discobox/macos:27   # the guest's screen in a window
```

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

The design and its reasoning are in [DESIGN.md](DESIGN.md).
