# docker driver (Linux guests as containers that look like machines)

**Scope:** `pkg/machine/docker`, and the hidden `disco-vm docker-init`
command that is a guest container's PID 1, and `disco-vm docker-volumes`,
which a helper container runs. Those commands are the one place above the
`machine.Driver` seam that names the driver: `internal/cli` imports
`pkg/machine/docker` for them, since the guest's init is the same binary (one
binary, see [DESIGN.md](../../DESIGN.md#one-binary-three-roles)). It picks no behavior by OS or driver. Nothing else above
the seam changed, and `internal/e2e` gained a `docker` target. Like
boxd, the daemon owns a running guest, so there is no shim, and every
command attaches to the container; and the daemon keeps everything the
driver knows, on its images and containers (ADR 26-10-23-417). Only
volumes and caches are kept on the daemon's host, in directories under
`/var/lib/discobox` (ADR 26-11-03-782), which the driver reaches through
helper containers: it needs that directory, or the one `$DISCO_VM_DOCKER_ROOT`
names, writable on the daemon's host.

**Done means:**

1. `machinetest.Run` passes against a real Docker daemon: `TestConformance`
   with `DISCO_VM_DOCKER_IMAGE` set, which it configures as the base.
2. `internal/e2e` passes with `DISCO_VM_DRIVER=docker`.
3. A fresh VM from the configured base is shaped through `exec`, stopped,
   and committed, and a VM is created from the committed one.

Status, 2026-10-09: all three held on Docker 29.8 (runc 1.5, containerd
snapshotter, cgroup v2), on Linux 7.0, when images were built from a spec
and a registry image was installed as a base layer. Since ADR 26-11-03-606
the base is configured, and there are only VMs; see "Confirmed" below for
what has run since.

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
   guest's root, for systemd. It also chowns the container's root
   directory to the guest's root: Docker makes that directory itself for
   every container, as the host's root, so the readied image's shift does not
   reach it, and without it the guest's root cannot make `/nix`.
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

## The base is shifted once for each fresh VM

Image files belong to uid 0 on the host, which the guest's namespace does not
map, so unshifted, the whole root filesystem would be `nobody`'s. Create of
a fresh VM moves every file of the base's image into the guest's range
once: `docker-init --shift` (`Shift`) walks the readying container's
root filesystem and chowns uid N to 16777216+N. chown clears setuid and
setgid bits and file capabilities, so it puts them back. It shifts a hard
link once, and leaves an id already in range alone. A committed VM is
written by the guest, so it is in range already, and `docker commit` keeps it
that way.

Idmapped mounts would avoid the copy, but making one takes `CAP_SYS_ADMIN` on
the container's side, and Docker does not offer one per container. Shifting
costs disk: on overlay, a chown copies the file up, so an instance's readied
image stores the base's files a second time (debian:trixie with
systemd: 434 MB on disk). The offset is in every committed VM, so all of a daemon's
guests share it.

## How the seam maps

| seam | docker |
|---|---|
| `Install` | not offered (`Capabilities.Install` is off): a fresh VM is readied from the base. |
| base | `machine.Config.Base` (the command line's `$DISCO_VM_BASE`), an image reference; none is configured by default, and a fresh `Create` is then `ErrUnsupported`. The caller never names a base at create (ADR 26-11-03-606). |
| `Create`, fresh | refused at once for a name the pool's committed VMs or containers hold. Pull the base if the daemon lacks it; in a container from it (`dvm-<pool>--base-<random>`, labeled as a VM of the pool's, so one a crash leaves is listed and `rm` removes it), as root: install systemd and dbus if missing (apt-get, dnf, or zypper), install the agent (`$DISCO_VM_AGENT`, or this binary on linux; its CPU must be the daemon's) as `disco-vm-guest.service`, mask units a container cannot run, remove Docker's `policy-rc.d`, empty `/etc/machine-id` and make D-Bus's copy a link to it, and shift; commit with `docker-init` as the entrypoint as `disco-vm-base:<pool>--<name>--<random>` (the random end keeps two creates racing for one name apart), and create the container from it as below. |
| `Create` from a committed VM | the container of the committed VM's Docker image, by its ID. |
| `Create` | create the container, with the settings above, named `dvm-<pool>--<name>`, with the VM's name as its hostname, labeled `ai.discobox.vm.instance` (the name), `.pool`, `.from` (the committed VM it came from, or empty), `.cpus`, and `.memory`, and `.committed` blank over its image's, publishing 7302/tcp on `127.0.0.1` at a port the daemon picks at each start. A name the daemon has is `ErrExists`, and so is one a committed VM took meanwhile, checked once the container is made. Cold only. |
| committed VM | a Docker image `disco-vm-vm:<pool>--<name>`, labeled `ai.discobox.vm.committed`, `.instance`, `.pool`, `.from`, `.cpus`, and `.memory` by the commit (`LABEL` changes, which also blank the volume labels the container carried). Its ID is the Docker image's, `sha256:...`, which is how the driver tells it from a container's bare hex ID; the seam reports no committed mark, which is the caller's to record (ADR 26-11-03-606). |
| VM | `Instances` lists the pool's committed VMs' images and its containers; `Instance` inspects the committed VM's tag, then the container. A container whose name a committed VM holds is what a failed commit left, and is not listed. Containers and images from before ADR 26-11-03-606 are not read: a container whose `ai.discobox.vm.image` label names an image, which since then only a readying container (`base-<6 hex>`) carries, and the `disco-vm-layer:` and `disco-vm/` tags. Remove them with `docker`, a container's readied `disco-vm-layer:<pool>--base-*` image with it. |
| `Service` | listen on `tcp:0.0.0.0:7302`; the URL is `http://127.0.0.1:<published port>` while the container runs, and the private URL `http://<the container's address on its network>:7302`, which another container dials. `Egress` is refused. |
| `Boot`, `Start`, `Commit`, `SaveCache` of a committed VM | `ErrCommitted`: an image has no container to run. `Attach` and `Service` answer `ErrNotRunning`. |
| `Boot`, `Start` | size the container (`docker update`: CPUs, and memory with no swap) and start it, then wait for the agent; a container that stops meanwhile (inspected every 2s) ends the wait. The daemon owns the container, so `Start` is `Boot`. |
| `Attach` | inspect the container: running is a machine, and stopped or gone is `ErrNotRunning`. A guest that powered itself off has already stopped its container, because Init exits with it. |
| `Machine.Done` | waiting starts on the first `Done`, `Err`, or `Kill`, so a machine attached only to run a command leaves nothing running. The daemon's `wait` on the container: exit 0 is a power-off. A failed wait (the daemon restarted) inspects and waits again. |
| `Machine.Kill` | SIGKILL to the container. |
| `Machine` guest operations | the agent (`guest.Client`), each connection a docker exec of `disco-vm pipe unix:/run/disco-vm-relay/guest/run/disco-vm/agent.sock`, on the container's side. |
| volumes | directories on the daemon's host under the root (`$DISCO_VM_DOCKER_ROOT`, default `/var/lib/discobox`): `Data` at `data/pools/<pool>/vms/<name>`, `Cache` at `cache/pools/<pool>/vms/<name>`, bind-mounted at `/.discobox/data` and `/.discobox/cache`. The container carries `ai.discobox.vm.data`, `.cache`, `.cache-bytes`, and `.generation`. Every step is a helper container of `disco-vm-helper:<sha256 prefix of the agent>`, an image imported (`POST /images/create?fromSrc=-`) of the agent alone, never removed by the driver (another disco-vm version on the daemon may use its own), with no network, bind-mounting the root at `/discobox` and running `disco-vm docker-volumes REQUEST` (JSON in its argument and on its stdout; exit 3 is `ErrCacheMoved`); copies keep owners, modes, times, symlinks, and hard links. Create makes the directories and stages the cache clone in `cache/pools/<pool>/vms/.<name>--<random>` before its container (a bind source must exist), and empties the data directory and renames the clone into place only once the container holds the name. docker-init chowns a root-owned `/.discobox` and each mount point into the guest's uids at every start, so on the host files keep the guest's shifted uids. A directory has no quota, so a size is a hint. A VM made before ADR 26-11-03-782 mounts named volumes `dvm-<pool>--<name>--data-<random>` and `--cache-<random>` instead: it boots with them, and `Upgrade` copies them into its directories and removes them. |
| caches | a generation is `cache/pools/<pool>/keys/<key>/<generation>`, and the key's pointer `current.json` beside it (generation, size, when saved), replaced by a rename once its generation is whole and renamed into place, so no VM clones a partial one. `keys/<key>/.lock` is a flock every helper on the host shares: a clone holds it shared while it copies, and `SaveCache`'s guard, publish, and prune of the key's other generations hold it exclusive, after the copy, which goes to a hidden `.save-<generation>`, held by a flock on `.save-<generation>.lock` while the save lives, and is checked against the guard first too; a prune removes a copy whose lock nobody holds. `Caches` reads each key's pointer; `DeleteCache` removes the pointer, then the generations, and keeps the key's directory and lock. `SaveCache` of a VM from before the ADR reads its named cache volume. Generations saved as named volumes then are not read, so their keys start empty; `docker volume rm` removes them. |
| `Upgrade` | refuse a committed VM (`ErrCommitted`), a running container, a `from` that is not one of the pool's committed VMs (`ErrNotFound`) or not linux, and a fresh root with no base (`ErrUnsupported`). Make the new container, of the committed VM's image or one readied from the base, with the VM's labels (`.from` the new one, `.replaces` the IDs it replaced, `.replaced-image` the image the old one ran) and its same directories, as `dvm-<pool>--<name>--upgrade`; remove the old container; rename the new one into place; remove the named volumes of a VM from before ADR 26-11-03-782, and the old image where nothing else needs it. The old container is checked stopped again once the new one is made. Caught between: the VM is the old container until it is removed, then the new one, which `Instance` and `Instances` find under the name; from the moment the new container exists `Boot`, `Start`, and `Commit` refuse the VM, `Upgrade` from the same `from` finishes it without reading that committed VM again (it may be gone), and `Destroy` removes both. An `Instance` whose container was replaced still names the VM, by `.replaces`. |
| `Commit` | refuse a running container; empty its `/etc/machine-id`, so every VM created from it makes its own, `docker commit` it as `disco-vm-vm:<pool>--<name>`, then remove the container and its directories as `Destroy` does. `docker commit` never holds a bind mount. |
| `Destroy` | of a VM: its directories, then `docker rm -f` of its container and an upgrade's new one, then the named volumes of a VM from before ADR 26-11-03-782, found by their labels (one a later VM of the name mounts is left), then the image the container ran if no tag names it any more, then the `disco-vm-base:<pool>--<name>--*` tag of the image the container ran, and every other one of the name older than 30 minutes that no container runs: what a Destroy that failed partway or a create that lost a race left, and never a create of the name still readying (no name has `--`, so another name's are never matched). Of a committed VM: a container a failed commit left, then its tag, removed with `force`, which the daemon turns into an untag where a container runs the image: a VM created from it stands alone, and its own `Destroy` removes the untagged image. |

The driver talks to the Docker Engine API over `DOCKER_HOST` itself, not
through the docker CLI or SDK, as boxd's does.

## Try it

On a Linux host with a rootful Docker daemon. Creating an instance of a
registry image copies the binary into it as the guest's agent and the
container's init, where it runs on the image's libc, so build it static:

```sh
CGO_ENABLED=0 go build -o disco-vm ./cmd/disco-vm   # or: go tool task build:linux-agent (amd64)
export DISCO_VM_DRIVER=docker

export DISCO_VM_BASE=debian:trixie

./disco-vm info                                            # check: ok
./disco-vm run dev                                         # pulls, readies, and starts it
./disco-vm exec dev systemctl is-system-running            # running
./disco-vm exec dev sh -c 'apt-get update && apt-get install -y tree'
./disco-vm stop dev && ./disco-vm commit dev               # dev is frozen
./disco-vm run --from dev dev2
./disco-vm exec -it dev2 bash
./disco-vm stop dev2 && ./disco-vm start dev2
./disco-vm rm -f dev2 && ./disco-vm rm dev
```

From a host whose daemon is elsewhere, the agent must be built for the
daemon's OS and CPU, static:
`CGO_ENABLED=0 GOOS=linux go build -o disco-vm-linux ./cmd/disco-vm`, and
`DISCO_VM_AGENT=./disco-vm-linux`. Create checks only the CPU, so a binary
linked against a libc the base lacks fails at create, where the readying
script runs it to shift the image's owners (exit 127), before any guest
boots.

## Configuration

| env | default | |
|---|---|---|
| `DOCKER_HOST` | `unix:///var/run/docker.sock` | `unix://` or `tcp://` without TLS. Docker contexts, `ssh://`, and TLS are not read. |
| `DISCO_VM_AGENT` | this binary, on linux | the static linux disco-vm, for the daemon's CPU, that Create puts in the base |
| `DISCO_VM_BASE` | none | the base, an image reference such as `debian:trixie` (`machine.Config.Base`); with none, a fresh VM is refused |

`Check` refuses a daemon on cgroup v1, one that runs Windows containers, and a
rootless one, whose user namespace has too few uids to map the guest's 65536.

## Confirmed

On this host (Docker 29.8.2, runc 1.5.1, containerd snapshotter, cgroup v2,
Linux 7.0.11), 2026-10-09, when a registry image was installed as a base
layer and images were built from a spec; what an install did, Create of a
base now does, for each instance:

- `TestConformance` passes from `debian:trixie`: install (about 30 s), boot,
  write, commit, clones that run at once and do not see each other's writes,
  kill, and attach (another process reaches a running guest, and finds one
  that powered itself off stopped).
- No shim: after `run`, no `disco-vm shim` process runs and the instance has
  no `shim.json`. `exec`, `start`, `stop`, and `rm` each attach. A guest's own
  `systemctl poweroff` shows as stopped in the next `ls`.
- `internal/e2e` passes with `DISCO_VM_DRIVER=docker`, about 40 s, and leaves
  no container or image behind. (`TestURL` came after this run.)
- An image built from the since-removed `docs/examples/docker.yaml` runs through `run`, `exec`, `exec -t` (a
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
- Every instance has its own machine ID, of a built image and of the base
  alike, and keeps it across a reboot and a stop and start.

Since ADR 26-10-23-410 (2026-10-24, Docker 29.9, Linux 7.1):
`TestConformance` from `debian:trixie` as a base, readied at Create (about 3
minutes with apt), every subtest but the listed bases'; `internal/e2e` with
the static binary, e2e/base a snapshot of debian:trixie; and by hand, two
generations of snapshots with the first removed and the second run with
both writes, and nothing left on the daemon.

Since ADR 26-11-02-180 (2026-11-02, Docker 29.9.2, Linux 7.1):
`TestConformance` from `debian:trixie`, a-data-volume and a-cache-volume
included, and `internal/e2e` TestVolumes with the static binary; no volume
or container left behind.

Since ADR 26-11-03-606 (2026-11-03, Docker 29.9.2, containerd snapshotter,
Linux 7.1): `TestConformance` with `debian:trixie` as the configured base,
every subtest but the fence's and the ports', which docker refuses (68 s);
by hand, a VM created from a committed one booted and read its writes after
the committed VM was removed, and its own Destroy removed the untagged
image. Nothing was left on the daemon. ⬜ `internal/e2e`.

Since ADR 26-11-03-782 (2026-11-03, Docker 29.9.2, Linux 7.1), with
`$DISCO_VM_DOCKER_ROOT` a scratch directory: `TestConformance` from
`debian:trixie`, a-data-volume, a-cache-volume, and an-upgrade included
(177 s), and `TestUpgradeInterrupted`, an upgrade stopped after each step,
then finished and destroyed (40 s). By hand: a VM made with named volumes
by the binary before the ADR saved its cache, was upgraded to a fresh root,
and read its data (a file of guest uid 1000, kept) and cache with its old
root's writes gone and its named volumes removed; and, under the default
`/var/lib/discobox`, a file the guest's root wrote to its data volume was
uid 16777216 on the host, and `rm` removed the VM's directory. Nothing was
left on the daemon but empty pool directories and keys' locks.

## Not verified yet

- Docker Desktop on macOS and Windows, a remote `tcp://` daemon, and arm64;
  under Docker Desktop, whether its file sharing lets a container bind-mount
  `/var/lib/discobox` in its VM.
- Two helpers on one key at once (the flock), and an upgrade interrupted by
  a crash rather than a failed step.
- SELinux hosts (`label=disable` is passed, but not run against).
- Load: each connection to the agent is a docker exec, and every engine call
  on an instance inspects its container first.

## Not done

- **Proxies.** The docker CLI hands containers the proxy settings in
  `~/.docker/config.json`, but systemd starts services with a clean
  environment, so the agent's `exec` does not see them. `exec -e` passes
  them to a command.
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
