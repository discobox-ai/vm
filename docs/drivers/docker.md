# docker driver (Linux guests as containers that look like machines)

**Scope:** `pkg/machine/docker`, and the hidden `disco-vm docker-init`
command that is a guest container's PID 1. That command is the one place
above the `machine.Driver` seam that names the driver: `internal/cli` imports
`pkg/machine/docker` for it, since the guest's init is the same binary (one
binary, decision 1). It picks no behavior by OS or driver. Nothing else above
the seam changed, and `internal/e2e` gained a `docker` target. Like
boxd, the driver is remote (`Capabilities.Remote`): the Docker daemon owns a
running guest, so the engine runs no shim, and every command attaches to the
container.

**Done means:**

1. `machinetest.Run` passes against a real Docker daemon: `TestConformance`
   with `DISCO_VM_DOCKER_IMAGE` set.
2. `internal/e2e` passes with `DISCO_VM_DRIVER=docker`.
3. `docs/examples/docker.yaml` builds.

Status, 2026-10-09: all three hold on Docker 29.8 (runc 1.5, containerd
snapshotter, cgroup v2), on Linux 7.0. See "Confirmed" below.

## What the guest is

A Docker container whose init is systemd, in a user namespace of its own. In
the guest, root is uid 0 with every capability. `systemctl` works,
`systemctl is-system-running` says `running`, `useradd` and `su` work, and
packages start their services. On the host, every guest process is an
unprivileged uid: guest uid N is host uid 16777216+N (`UIDBase`). The
container runs without `--privileged` and without any added capability.

It is not a VM, and it shows:

- **The kernel is the host's.** No modules, no kernel of the guest's choosing,
  and `uname -r` is the host's.
- **`/proc` shows the host's resources.** `free`, `nproc`, and
  `/proc/uptime` report the host's memory, CPUs, and uptime, not the
  container's limits. sysbox and LXC hide this with lxcfs; this driver does
  not yet.
- **The network is Docker's.** The guest shares the container's network
  namespace, which belongs to the host's user namespace, so the guest's root
  cannot change interfaces or routes (`ip link set` is refused). `/sys` is
  Docker's read-only sysfs for the same reason. Outbound traffic, DNS from `/etc/resolv.conf`, and
  listening on ports all work.
- **`/etc/hosts`, `/etc/hostname`, and `/etc/resolv.conf` are Docker's.**
  Docker bind-mounts them over the guest's, and they belong to the host's
  root, so the guest reads them but cannot change them.
- **Every guest shares one uid range.** Guests are kept apart by their
  namespaces, not by distinct host uids.

## Why there is an init, and what it does

Docker offers a user namespace only for a whole daemon (`userns-remap` in
`daemon.json`), not per container. `docker run --userns` accepts only `host`
(checked on 29.8). Turning on remapping changes every container on the daemon,
needs a restart and a separate image store, and keeps `--privileged` from
being used without `--userns=host`. So the driver makes the namespace itself,
from inside an ordinary container. The image's entrypoint is
`/usr/local/libexec/disco-vm docker-init` (`Init`). It runs as the container's
root (host root, with Docker's default capabilities) and:

1. **Delegates the cgroup.** It moves itself into a leaf (`/init`), enables
   every controller below the container's cgroup, and chowns `/guest` to the
   guest's root, for systemd.
2. **Starts the guest.** It re-runs itself in new user, mount, pid, cgroup,
   uts, and ipc namespaces, mapping guest uids 0-65535 to 16777216 and up, as
   uid 0 of that namespace. That side mounts a new `/proc`, the guest's
   cgroup2, a tmpfs on `/run`, its own devpts, and an mqueue, then execs
   systemd with `container=disco-vm`.
3. **Links the guest for the relay.** It points
   `/run/disco-vm-relay/guest` at `/proc/<guest pid>/root`. `relayDir` is a
   tmpfs only the container's root can open, and the guest's `/run` hides it.
4. **Waits.** systemd's power-off ends the guest; Init exits 0, so the
   container stops and `Done` closes with no error. A reboot (SIGHUP) starts
   the guest again. `docker stop` (SIGTERM) asks systemd to power off.

Two kernel details shaped it:

- A child cloned into a new user namespace keeps its parent's uid, which is
  unmapped there, so it execs with no capabilities. The guest is started with
  an explicit credential of 0:0, as `unshare --setuid 0` does.
- `mount(2)` refuses to put a filesystem on a mount point whose top mount is
  the same filesystem (EBUSY), and Docker's `/sys/fs/cgroup` is cgroup2. The
  guest's cgroup2 is mounted with the new mount API (`fsopen`, `fsmount`,
  `move_mount`), which does not check.

The container relaxes four of Docker's protections and adds nothing:

| setting | why |
|---|---|
| `seccomp=unconfined` | the default profile refuses to create namespaces without `CAP_SYS_ADMIN` |
| `apparmor=unconfined` (and `label=disable`) | `docker-default` refuses `mount`, and the guest mounts `/proc`, `/run`, and the rest |
| `systempaths=unconfined` (empty `MaskedPaths` and `ReadonlyPaths`) | the kernel mounts a new `/proc` only where the existing one is not partly hidden |
| `writable-cgroups=true`, `--cgroupns=private` | Init writes the container's own cgroup subtree, to hand it to the guest |

What protects the host is the user namespace, as with rootless Podman or an
unprivileged LXC container. Guest code never runs on the container's side:
only Init and the `disco-vm pipe` relays do. With seccomp off, a guest process
reaches syscalls the default profile would block, but as an unprivileged uid.
A seccomp profile that is Docker's default plus namespace creation and `mount`
would narrow that, and is not written yet.

## The base layer is shifted once

Image files belong to uid 0 on the host, which the guest's namespace does not
map, so unshifted, the whole root filesystem would be `nobody`'s. Install
moves every file's owner into the guest's range once: `docker-init --shift`
(`Shift`) walks the install container's root filesystem and chowns uid N to
16777216+N. chown clears setuid and setgid bits and file capabilities, so it
puts them back. It shifts a hard link once, and leaves an id already in range
alone. Every later layer is written by the guest, so it is in range already,
and `docker commit` keeps it that way.

Idmapped mounts would avoid the copy, but making one takes `CAP_SYS_ADMIN` on
the container's side, and Docker does not offer one per container. Shifting
costs disk: on overlay, a chown copies the file up, so the base layer stores
the image's files a second time (debian:trixie with systemd: 434 MB on disk).
The offset is in every layer, so all of an image store's guests share it.

## How the seam maps

| seam | docker |
|---|---|
| `Install` | pull `options.image` (default `debian:trixie`) if the daemon lacks it; in a container from it, as root: install systemd and dbus if missing (apt-get, dnf, or zypper), install the agent as `disco-vm-guest.service`, mask units a container cannot run, remove Docker's `policy-rc.d`, empty `/etc/machine-id` and make D-Bus's copy a link to it, and shift; commit with `docker-init` as the entrypoint. Media must be `latest`. |
| layer | `layer.json`: the committed image's ID and the tag (`disco-vm-layer:<id>-<rand>`) that keeps it from being pruned. `DeleteLayer` removes the tag, and the image with it. |
| `Prepare` | create the container from the parent's image, with the settings above, named `dvm-<instance id>-<rand>`. Cold only. |
| `Boot` | size the container (`docker update`: CPUs, and memory with no swap) and start it. |
| `Attach` | (`Capabilities.Remote`: the daemon owns the container, so the engine runs no shim.) Inspect the container: running is a machine, and stopped or gone is `ErrNotRunning`. A guest that powered itself off has already stopped its container, because Init exits with it. |
| `Machine.Done` | waiting starts on the first `Done`, `Err`, or `Kill`, so a machine attached only to dial leaves nothing running. The daemon's `wait` on the container: exit 0 is a power-off. A failed wait (the daemon restarted) inspects and waits again. |
| `Machine.Kill` | SIGKILL to the container. |
| `Machine.Dial(port)` | docker exec `disco-vm pipe`, on the container's side: `unix:/run/disco-vm-relay/guest/run/disco-vm/agent.sock` for the agent, `tcp:127.0.0.1:PORT` otherwise, which the guest shares. |
| `Commit` | empty the stopped container's `/etc/machine-id`, so every instance of the layer makes its own, and `docker commit` it. |
| `Destroy` | `docker rm -f`. |

The driver talks to the Docker Engine API over `DOCKER_HOST` itself, not
through the docker CLI or SDK, as boxd's does.

## Try it

On a Linux host with a rootful Docker daemon. Install copies the binary into
the image as the guest's agent and the container's init, where it runs on the
image's libc, so build it static:

```sh
CGO_ENABLED=0 go build -o disco-vm ./cmd/disco-vm   # or: go tool task build:linux-agent (amd64)
export DISCO_VM_DRIVER=docker

./disco-vm info                                            # check: ok
./disco-vm build -f docs/examples/docker.yaml -t demo/docker
./disco-vm run --name dev demo/docker
./disco-vm exec dev systemctl is-system-running            # running
./disco-vm exec -it dev bash
./disco-vm stop dev && ./disco-vm start dev
./disco-vm rm -f dev && ./disco-vm rmi demo/docker
```

From a host whose daemon is elsewhere, the agent must be built for the
daemon's OS and CPU, static:
`CGO_ENABLED=0 GOOS=linux go build -o disco-vm-linux ./cmd/disco-vm` and
`build --agent ./disco-vm-linux`. Install checks only the CPU, so a binary
linked against a libc the image lacks fails at install, where the install
script runs it to shift the image's owners (exit 127), before any guest boots.

## Configuration

| env | default | |
|---|---|---|
| `DOCKER_HOST` | `unix:///var/run/docker.sock` | `unix://` or `tcp://` without TLS. Docker contexts, `ssh://`, and TLS are not read. |

`Check` refuses a daemon on cgroup v1, one that runs Windows containers, and a
rootless one, whose user namespace has too few uids to map the guest's 65536.

## Confirmed

On this host (Docker 29.8.2, runc 1.5.1, containerd snapshotter, cgroup v2,
Linux 7.0.11), 2026-10-09:

- `TestConformance` passes from `debian:trixie`: install (about 30 s), boot,
  write, commit, clones that run at once and do not see each other's writes,
  kill, and attach (another process reaches a running guest, and finds one
  that powered itself off stopped).
- No shim: after `run`, no `disco-vm shim` process runs and the instance has
  no `shim.json`. `exec`, `start`, `stop`, and `rm` each attach. A guest's own
  `systemctl poweroff` shows as stopped in the next `ps`.
- `internal/e2e` passes with `DISCO_VM_DRIVER=docker`, about 40 s, and leaves
  no container or image behind. `TestWarm` and `TestForward` skip, because the
  driver lists neither.
- `docs/examples/docker.yaml` builds and runs through `run`, `exec`, `exec -t` (a
  TTY on the guest's own devpts), `exec -u`, `cp`, `stop`, `start`, `rm`, and
  `rmi`. A cold `run` takes 0.6 s, `stop` 0.4 s, and `start` 0.5 s. Writes and
  users survive a stop and start, and another instance of the image sees none
  of them. `apt-get install cron` in a build step leaves cron `active` in the
  instance.
- In the guest, systemd is PID 1 and `systemctl is-system-running` says
  `running`, with journald, logind, and dbus up and no failed units. On the
  host, `ps` shows those processes at uids 16777216 and up.
- An orderly shutdown through the agent powers systemd off, Init exits 0, and
  the container stops.
- A guest's own `systemctl reboot` comes back in the same container, in a
  cgroup made anew, with its writes kept and `systemctl is-system-running`
  `running`.
- Every instance has its own machine ID, of a built layer and of the base
  alike, and keeps it across a reboot and a stop and start.

## Not verified yet

- Docker Desktop on macOS and Windows, a remote `tcp://` daemon, and arm64.
- SELinux hosts (`label=disable` is passed, but not run against).
- Load: each Dial is a docker exec, and every engine call on an instance
  inspects its container first.

## Not done

- **Proxies.** The docker CLI hands containers the proxy settings in
  `~/.docker/config.json`, but systemd starts services with a clean
  environment, so build steps do not see them. A spec's `env:` passes them to
  its steps.
- **No fast clones.** A cold boot is under a second, so there is no `Warmer`.
- **No shared directories.** A bind mount would show the guest the host's
  owners, which it does not map; it needs an idmapped mount.
- **No forward** (guest to host), no display.
- **`/proc` virtualisation and a network namespace of the guest's own**, which
  would make the guest look more like a machine. Both are possible without
  `CAP_SYS_ADMIN`: an lxcfs-style FUSE `/proc`, and a network namespace the
  guest owns, joined to the container's by a veth or slirp.
- **sysbox.** Where the daemon has `sysbox-runc`, it could run systemd
  directly, with `/proc` virtualised and a uid range per container. The
  driver does not use it.
