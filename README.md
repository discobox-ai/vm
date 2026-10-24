# disco-vm

Run OS images as VMs, and snapshot them as new images, from one codebase and
one command line:

- **Windows guests on Windows**, through Host Compute Service. There is no
  Hyper-V manager, no Windows Sandbox, and no WSL, so it works on Home.
- **macOS guests on macOS**, through Virtualization.framework.
- **Linux guests in the cloud**, on [boxd](https://boxd.sh) microVMs, from
  any host.
- **Linux guests as Docker containers that look like machines**: systemd is
  their init, and their root is root only over the guest, as an unprivileged
  uid on the host, without `--privileged`.

disco-vm works on its own, and it is also meant to become a
[discobox](https://github.com/discobox-ai/discobox) sandbox provider, which
embeds `pkg/engine` as a library.

> **Status: the shell.** Everything above the hypervisor is built and tested on
> every OS through a `fake` driver. That covers the CLI, instance lifecycle,
> snapshots, the local driver's store and its shim per VM, and the guest agent
> and its protocol. The `vz` driver runs macOS 27 guests on Apple silicon, with a
> native window (`--gui`) ([docs/drivers/vz.md](docs/drivers/vz.md)). The
> `hcs` driver's brief is [docs/drivers/hcs.md](docs/drivers/hcs.md). The
> `boxd` driver runs boxd's own images and snapshots by hand, and passes the
> conformance suite against a fake of boxd's API; `internal/e2e` does not run
> on it yet (see [docs/drivers/boxd.md](docs/drivers/boxd.md)). The `docker`
> driver runs any registry image (see
> [docs/drivers/docker.md](docs/drivers/docker.md)).

List the drivers compiled into the current binary, then select one with the
global `--driver` flag (or `DISCO_VM_DRIVER`):

```sh
disco-vm drivers
disco-vm --driver docker info
```

## Images are snapshots

There is no image build. An image is an install from media (hcs, vz), or a
snapshot of a stopped instance; a driver may also have bases that run with
no image at all (boxd's images and snapshots, any Docker registry image).
Shape an image by running one, changing the guest through `exec` and `cp`,
stopping it, and taking a snapshot:

```sh
disco-vm install -t discobox/windows:11 --media D:\iso\Win11.iso --edition "Windows 11 Pro" --disk 128GiB
disco-vm run --name dev discobox/windows:11
disco-vm cp ./install-toolchains.ps1 dev:C:\setup
disco-vm exec dev powershell -File C:\setup\install-toolchains.ps1
disco-vm stop dev && disco-vm snapshot dev discobox/base   # consumes dev
disco-vm run --name work discobox/base
```

Each image stands alone: removing the one a snapshot came from leaves the
snapshot whole.

## Run macOS guests

On an Apple silicon Mac, build through `go tool task build`, which signs the binary with the
virtualization entitlement a VM needs:

```sh
go tool task build
build/disco-vm install -t discobox/macos:27 --disk 128GiB   # downloads the restore image once, ~5 min after that
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
| `pkg/machine` | the driver seam; `boxd`, `docker`, `fake`, `hcs` (windows), `vz` (darwin); `local`, the store and shim of the drivers on this host's disk; `machinetest` conformance suite |
| `pkg/engine` | images, tags, instances, lifecycle, install, and snapshot, kept by the driver |
| `pkg/guest` | agent protocol, server, and host client |
| `internal/e2e` | the CLI end to end |

The design and its reasoning are in [DESIGN.md](DESIGN.md).
