# disco-vm

Run VMs, fresh or from committed ones, and commit them to make more, from one
codebase and one command line:

- **Windows guests on Windows**, through Host Compute Service. There is no
  Hyper-V manager, no Windows Sandbox, and no WSL, so it works on Home.
- **macOS guests on macOS**, through Virtualization.framework.
- **Linux guests in the cloud**, on [boxd](https://boxd.sh) microVMs or
  [Archil](https://archil.com) sandboxes, from any host.
- **Linux guests as Docker containers that look like machines**: systemd is
  their init, and their root is root only over the guest, as an unprivileged
  uid on the host, without `--privileged`.

disco-vm works on its own, and it is also meant to become a
[discobox](https://github.com/discobox-ai/discobox) sandbox provider, which
embeds `pkg/engine` as a library.

> **Status: the shell.** Everything above the hypervisor is built and tested on
> every OS through a `fake` driver. That covers the CLI, the VM lifecycle,
> commits, the local driver's store and its shim per VM, and the guest agent
> and its protocol. The `vz` driver runs macOS 27 guests on Apple silicon, with a
> native window (`--gui`) ([docs/drivers/vz.md](docs/drivers/vz.md)). The
> `hcs` driver's brief is [docs/drivers/hcs.md](docs/drivers/hcs.md). The
> `boxd` driver runs fresh VMs of boxd's images by hand, and passes the
> conformance suite against a fake of boxd's API; `internal/e2e` does not run
> on it yet (see [docs/drivers/boxd.md](docs/drivers/boxd.md)). The `docker`
> driver makes fresh VMs of a registry image it is configured with (see
> [docs/drivers/docker.md](docs/drivers/docker.md)). The `archil` driver
> passes the conformance suite against Archil itself (see
> [docs/drivers/archil.md](docs/drivers/archil.md)).

List the drivers compiled into the current binary, then select one with the
global `--driver` flag (or `DISCO_VM_DRIVER`):

```sh
disco-vm drivers
disco-vm --driver docker info
```

## Install

On macOS (Apple silicon) or Linux:

```sh
curl -sSfL https://github.com/discobox-ai/vm/releases/latest/download/install.sh | sh
```

or with Homebrew:

```sh
brew install discobox-ai/tap/disco-vm
```

On Windows, in PowerShell:

```powershell
irm https://github.com/discobox-ai/vm/releases/latest/download/install.ps1 | iex
```

Each installs the newest release. To install another, such as a release
candidate, pass `--version v0.2.0-rc.1` to `install.sh` (`sh -s -- --version
...` through a pipe), or set `$env:DISCO_VM_INSTALL_VERSION` before
`install.ps1`. Every release also has its binaries and their `checksums.txt`
on its [GitHub Releases](https://github.com/discobox-ai/vm/releases) page.
`disco-vm --version` prints the release.

The macOS binary is signed with the entitlement Virtualization.framework
needs, so it runs macOS guests as installed. It also carries the Linux
disco-vm of the same release, which it hands a Linux guest on vz, or a
Docker guest, as its agent, so those run as installed too; the Windows
binary carries one for Docker guests the same way. To use another, name it
by its absolute path in `DISCO_VM_AGENT`.

To build from a checkout instead, run `go tool task build` (or `install`).

## There are only VMs

A VM is made fresh, from the driver's base (its configuration,
`$DISCO_VM_BASE`: a Docker image, boxd's default image, an OCI image Archil
runs, vz's Linux guest image), installed from media (hcs, vz), or created from a committed VM.
`commit` freezes a stopped VM under its name: create VMs from it, and never
start it again. There are no images or tags; versions are names
(ADR 26-11-03-606). Some providers cannot tell a committed VM from a stopped
one, so keep your own record of which VMs you committed.

```sh
disco-vm install win11 --media D:\iso\Win11.iso --edition "Windows 11 Pro" --disk 128GiB
disco-vm run dev --from win11
disco-vm cp ./install-toolchains.ps1 dev:C:\setup
disco-vm exec dev powershell -File C:\setup\install-toolchains.ps1
disco-vm stop dev && disco-vm commit dev
disco-vm run work --from dev
```

On most drivers a VM created from a committed one outlives it. On hcs, whose
disks chain, a committed VM cannot be removed while a VM created from it
exists.

## Run macOS guests

On an Apple silicon Mac, build through `go tool task build`, which signs the binary with the
virtualization entitlement a VM needs:

```sh
go tool task build
build/disco-vm install macos-27 --disk 128GiB   # downloads the restore image once, ~5 min after that
build/disco-vm run mac --from macos-27
build/disco-vm exec mac sw_vers
build/disco-vm run desk --gui --from macos-27   # the guest's screen in a window
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
| `pkg/machine` | the driver seam; `archil`, `boxd`, `docker`, `fake`, `hcs` (windows), `vz` (darwin); `local`, the store and shim of the drivers on this host's disk; `machinetest` conformance suite |
| `pkg/engine` | VMs, their lifecycle, install, and commit, kept by the driver |
| `pkg/guest` | agent protocol, server, and host client |
| `internal/e2e` | the CLI end to end |

The design and its reasoning are in [DESIGN.md](DESIGN.md).
