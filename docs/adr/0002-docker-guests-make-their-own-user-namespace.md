# 0002 — Docker guests make their own user namespace, and the base layer is shifted once

- **Status**: Proposed
- **Date**: 2026-10-09

## Context

We want Linux guests that look like machines rather than containers: systemd
as init, a root that can do anything to the guest, and no privilege on the
host. Docker is the runtime most hosts already have. Its only user namespace
is `userns-remap`, set in `daemon.json` for the whole daemon; `docker run
--userns` accepts only `host` (Docker 29.8). A container's files belong to
host uid 0, which a user namespace does not map unless they are shifted or
mounted through an idmap.

## Decision

The docker driver runs each guest in an ordinary container whose PID 1,
`disco-vm docker-init`, creates the user namespace itself: guest uids 0-65535
map to host uids 16777216 and up, and systemd runs as that namespace's root.
The container gets no added capability and is not privileged. It relaxes
seccomp, AppArmor, Docker's masked `/proc` paths, and the cgroup mount's
read-only flag, all of which namespace creation and systemd need.

Install shifts the base image's file owners into the guest's range once
(`docker-init --shift`) and commits it. Every later layer is written by the
guest and is in range already.

## Alternatives rejected

- **`userns-remap`.** It changes every container on the daemon, needs a
  restart and a separate image store, and stops `--privileged` working
  without `--userns=host`. A driver cannot turn that on for a daemon other
  people use.
- **sysbox.** It virtualises `/proc` and gives each container its own uid
  range, so its guests look more like machines. But it is a root install on
  the host (a runtime and two daemons), it is not on Docker Desktop, and it is
  not on the hosts we have. It stays an option for a runtime the driver could
  use where it is installed.
- **Podman with `--userns=auto --systemd=always`.** Per-container ranges and
  systemd support are built in, but it means asking for Podman instead of
  Docker.
- **Idmapped mounts instead of shifting.** They avoid the copy, and allow a
  range per container, but making one needs `CAP_SYS_ADMIN` on the
  container's side, and Docker does not offer one per container. Shifting
  once needs no capability beyond Docker's defaults.

## Consequences

- A guest runs on any rootful Docker daemon on cgroup v2, with no host setup.
- The base layer stores the image's files twice (overlay copies up each
  chowned file), and the uid offset is baked into every layer, so all of an
  image store's guests share one range.
- The guest sees the host's `/proc` (memory, CPUs, uptime) and shares the
  container's network namespace, which it cannot reconfigure.
- What protects the host is the user namespace. With seccomp off, a guest
  reaches more syscalls than Docker's default allows, as an unprivileged uid.

## Deferred

- A seccomp profile (Docker's default plus namespace creation and `mount`) in
  place of `seccomp=unconfined`, when the driver is used on shared hosts.
- Using sysbox where the daemon has `sysbox-runc`, if a guest needs to look
  more like a machine than `/proc` and networking allow today.
- Idmapped mounts, when shared directories are needed, since a bind mount
  needs one anyway.
