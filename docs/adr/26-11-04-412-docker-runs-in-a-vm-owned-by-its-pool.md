# 26-11-04-412 — Docker runs in a VM owned by its pool

- **Status**: Accepted
- **Date**: 2026-11-04

Accepted by the owner by merging #54, 2026-11-04.

## Context

Issue #46 asks how disco-vm replaces discobox's Docker-in-VM pool hosts.
The owner chose a pool scope instead of a project scope, and a third internal
URL, preferred over private and public URLs for communication (issue discussion). This proposal records those choices and the prerequisites;
it implements nothing and does not claim guest behavior has been verified.

At the checked-out default branch:

- `pkg/machine/registry.go` constructs one driver for `Config.Project`;
  construction must be cheap and must not contact the provider.
- `pkg/machine/docker/api.go` dials a Unix socket or plain TCP from
  `DOCKER_HOST`. HTTP requests and hijacked exec streams use its dialer.
- `pkg/machine/docker/docker.go` publishes service port 7302 on the daemon
  host's loopback and reports a container bridge address as `PrivateURL`.
  Neither address necessarily reaches a caller outside a host VM.
- Docker volume and cache helpers operate on the daemon host's directories
  (`pkg/machine/docker/volumes.go`; ADR 26-11-03-782 §6).
- `pkg/machine/machine.go`, the local shim, and `pkg/engine/engine.go`
  restrict host-to-guest `Dial` to agent 7300 and service 7302, as proposed
  in ADR 26-11-03-247. An arbitrary Docker vsock port is therefore not an
  available transport.
- vz runs Linux and macOS guests but has no data volumes or Upgrade;
  libkrun runs Linux but likewise has no data volumes. hcs runs Windows
  guests; there is no Linux-on-Windows driver here
  (`pkg/machine/DESIGN.md`, `docs/drivers/{vz,libkrun,hcs}.md`).
- `pkg/engine/engine.go` checks `MaxRunning` against one project's listing,
  so renaming the scope alone does not enforce a host-wide macOS limit.

ADR 0004 defines project scoping; ADR 26-10-23-417 keeps the engine stateless;
ADRs 0002 and 26-11-03-782 define Docker user namespaces and durable storage.
This proposal changes their scope terminology and storage path component
from project to pool on implementation, leaving their isolation, cache,
ownership, and upgrade contracts intact. Their status notes are updated
when this proposal is accepted, not in this ADR-only change.

## Decision

### 1. Compose below the seam, one host VM per pool

Add `pkg/machine/dockervm`, composing a concrete Docker driver with a VM
`machine.Driver`. Register explicit combinations, starting with `docker-vz`;
`docker-libkrun` follows when its storage prerequisites are met. Registration
stays under `pkg/machine/drivers`' platform selection. The engine and CLI
remain OS-neutral. Do not add a general dependency graph, optional driver
interfaces, or a second binary.

Rename `Project` to `Pool` throughout the seam, CLI (`--pool`,
`DISCO_VM_POOL`), keys, labels, local store, and documentation; keep `default`
as the default and the existing scope validation/name budgets. discobox uses
its globally unique pool ID, not projectId+poolId. Pool IDs scope names,
commits, keys and caches. Move local `MaxRunning` enforcement below the seam,
under a host-wide lock at start/boot, across every pool in that driver's
root. Preserve OS-neutral capability reporting above the seam.

This is a breaking API/CLI and state-format rename, with no old flag alias
or state migration. The owner confirmed on 2026-11-04 that there are no
production deployments and requested the rename without migration. Local
records and keys use `pool`; Docker labels use `ai.discobox.vm.pool`, and
its volume/cache paths use `pools`. State written with the old project
format is not adopted as new pool state. Development users remove their old
resources with the old binary before switching. The discobox integration
must update its callers and dependency together.

The underlying VM is named `host` in the same pool of the underlying driver.
Its namespace is separate from inner Docker VM names; `host` remains usable
as an inner container name. A caller inspects or administers the dependency
through `--driver vz --pool <id>`. The composite lists only inner VMs.

### 2. Own the dependency's lifecycle explicitly

Driver construction and capability listing never boot a host VM. Check
validates prerequisites without creating one; stateful operations ensure the
host exists and answers. Serialize host create/start/upgrade/removal per pool
in the underlying provider's durable coordination domain. Re-read state after
a competing create/start; do not treat every failure as an attach success.
Independent processes must converge on the same host, including after a crash.
A missing host with previously allocated durable state is an error to repair,
never permission to initialize an empty daemon over it.

Host CPU, memory and disk sizing are explicit composite settings, distinct
from the inner base image and each container's CPU/memory limits. Keep the
pinned host image reference distinct from `Config.Base`, which remains the
inner container base. The concrete settings API and defaults remain open to
implementation; validate them and refuse conflicts with an existing host.

The host outlives client processes and individual inner VM removals. Stopping
an inner VM never stops it. There is no automatic idle eviction in the first
version. Pool teardown is orchestrated by the caller: stop admissions, remove
inner VMs, committed VMs and cache keys, then destroy the host through the
underlying driver. Destroying that host deletes the whole pool's Docker state
and is an explicit destructive administrative action.

### 3. Carry control through guest operations, without a new port

Give Docker an explicit daemon connection constructor, used for both ordinary
HTTP requests and hijacked streams; keep native Docker's environment-based
constructor. The composite opens each connection through the host VM's guest
Exec of `disco-vm pipe unix:/run/docker.sock` (already supported by
`internal/cli/internal.go`) and carry bytes over the host agent's existing
7300 connection. Preserve cancellation, EOF and half-close semantics; no
connection or helper process survives the request.

No daemon TCP listener, new vsock port, or relaxation of `DialablePort` is
required. This reuses `machine.Guest.Exec`, so `machine.Driver` needs no new
dependency or arbitrary-port method. It is privileged host control: never
expose this transport to an inner sandbox or bind-mount Docker's socket there.
The host and inner agents each use their matching Linux binary; macOS's
executable is not copied into a Linux container.

### 4. Publish internal, private and public service URLs

Add `InternalURL` to `machine.Service`, returned through an OS-neutral engine
accessor and CLI `url --internal`; an unavailable explicit URL is unsupported.
Keep existing `URL` as the public field and `PrivateURL` as the peers' field.
Communication code in discobox selects the first nonempty reachable-scope
field in order **internal, private, public**. This is selection by advertised
address, not retry after an authentication or application error.

Internal means reachable by the driver caller managing the pool; it is not
an address every peer can use. A caller on another host must not reuse a
host-local URL, so the driver advertises it only for the caller's topology.
Private retains the inner container bridge address, reachable by peers within
that host VM. Do not mislabel the host VM's loopback Docker publication as
public on the physical host. Public may be empty when no public route exists.

For `docker-vz`, provide an owner-only host relay with a physical-host
loopback HTTP listener for each running inner service. It carries connections
through host guest Exec to a pipe targeting only that container's service
port 7302. It returns that listener as `InternalURL`. Resolve the container
from provider state; never accept arbitrary host socket or destination input
from an inner guest. Reuse the existing shim lifecycle pattern and
`Config.ShimCommand`; the relay runs as a role of `disco-vm` and outlives the
creating CLI. Rediscover it from driver-owned state, reject stale endpoints,
and clean it up on stop, destroy, or host loss. Reattach/recreate after restart;
URLs need not retain their ephemeral port across boots.

This relay makes the owner's internal URL usable without opening a new
vsock port or exposing all containers to the VM's NAT peers. It does not
provide authentication for the application; issue #45's service security
remains separate. Existing drivers may leave InternalURL empty. Update every
Service producer and caller, fake, conformance and CLI tests together.

### 5. Keep all pool state on a durable host data disk

Publish a digest-pinned Linux guest image variant with rootful dockerd,
cgroup v2, user namespaces and overlayfs. Build and verify it through the
Linux guest image pipeline. Mount the underlying VM's data disk before
Docker starts; place both `/var/lib/docker` and `/var/lib/discobox` on it.
A missing or failed mount fails boot rather than writing either directory
to the root disk. This makes data volumes and interrupted Upgrade recovery
on the underlying driver mandatory before enabling a composite registration.

Docker remains responsible for inner roots, commits, data volumes and cache
generations. Helpers copy in the host VM, not across the outer control
connection. Rename the `projects` storage component to `pools` with the scope
migration; do not adopt discobox's unrelated cache tree. Files retain shifted
uids 16777216+N as today. The outer disk is pool-owned and each inner data
volume is still removed only with its inner VM or Commit, as specified by
ADR 26-11-03-782.

To upgrade the host: stop admissions and every running inner VM, stop dockerd
cleanly and power off the host, then call underlying Upgrade with the new
pinned root, preserving its data disk. Restart and verify the daemon and
stored inner inventory before admitting work. Do not commit the host as a
backup: Commit intentionally excludes its data disk. Record image/storage
compatibility before changing roots; a Docker storage migration is not
assumed reversible. An interrupted upgrade must finish or remain refused
with its data preserved. No automatic destructive rollback is permitted.

The host kernel must pass Docker's existing rootful/cgroup checks and real
nested user-namespace/overlayfs/systemd tests. The composite reports Docker's
Linux, agent, commit, data/cache and upgrade behavior, not the underlying
hypervisor's display, guest OSes, keys, fences, or ports. In particular, an
outer VM fence does not establish per-container egress guarantees.

## Alternatives rejected

- **One dockerd VM shared across all pools.** It amortizes RAM but makes
  independent pool removal, sizing and isolation depend on one shared daemon.
  The owner chose one VM per pool despite its memory cost.
- **Retain project or use projectId+poolId.** It confuses discobox's project
  with the resource owner and spends the limited provider name budget on an
  already globally unique pool ID.
- **A systemd proxy on a new vsock port.** The current Dial contract refuses
  it; keeping it would require widening every driver and libkrun's boot maps.
  Guest Exec already provides the necessary byte stream.
- **Expose the daemon or all service publications on the host VM NIC.** It
  introduces network reachability and trust requirements for pool control,
  and exposes services to NAT peers merely to reach the physical host.
- **Return the Docker host loopback URL unchanged.** Inside the VM that URL
  names a different host from the caller's. A third field alone cannot fix
  transport; the internal relay supplies the missing path.
- **Install Docker at first boot and keep its store on root.** Provisioning
  is unpinned and root replacement destroys every inner VM and commit.
- **Implement generic driver dependencies in the engine.** It moves provider
  lifecycle and durable coordination above the seam for one composition.

## Consequences

This is large work, delivered in independently reviewable steps. Proposed
follow-up issues, not existing assignments:

| Step | Repository and deliverable | Size | Prerequisites |
|---|---|---|---|
| A | vm: pool rename and state-format cutover; host-wide local capacity separately | medium | accepted ADR |
| B | vm: explicit Docker connection and host-socket pipe transport | small | accepted ADR |
| C | vm: vz Linux data disk and recoverable Upgrade | medium | accepted ADR |
| D | vm: pinned Docker host image with fail-closed data mounts | medium | C |
| E | vm: InternalURL across seam, engine, CLI and existing drivers | medium | accepted ADR |
| F | vm: dockervm/docker-vz lifecycle, coordination and internal relay | large | A, B, D, E |
| G | discobox: pool ID scope and internal/private/public selection | medium | A, E |
| H | discobox: replace dockerworker's vz pool host with docker-vz | medium | F, G, real-guest verification |
| I | vm: libkrun data disk/Upgrade, then docker-libkrun | medium | F |
| J | vm: scope Linux-on-Windows host driver, then Docker composition | large | separate driver design |

The discobox replacement description follows #46's scope and discussion;
this ADR does not claim a fresh audit of that repository's dockerworker code.
Migrate one pool backend at a time; retain old pools until explicitly drained
or migrated. Do not silently import their Docker stores or shifted host data.

Verification must cover concurrent independent callers and failures during
host creation, start, upgrade and teardown; two pools with identical inner
names; control-stream cancellation and half-close; service URL reachability
from the physical caller and private URL from a peer; process exit and
reattach; host reboot; inner commit, data and guarded cache saves; host root
upgrade preserving all inner inventory and files; and mount/upgrade failure
without an empty replacement daemon. Run machinetest and CLI e2e on the
composite against a real Linux guest on vz, then separately on libkrun.

This ADR moves no layer of DESIGN.md's proof table. Implementation must keep
fake and cross-platform CI green, run `go tool task ci`, record the exact
real-guest runs and update the proof table. Neither a Docker fake API nor a
native Docker run proves Docker inside a VM.

## Deferred

- **Host directory sharing with macOS/Windows.** Revisit when a caller needs
  direct physical-host file access and shifted uid semantics are proven for
  the chosen filesystem transport; no virtiofs export is required initially.
- **Idle shutdown and resizing.** Revisit with explicit pool admission and
  settings-change contracts; Upgrade presently preserves CPU/memory settings.
- **Linux guests on Windows.** hcs's Windows guest support is insufficient;
  scope a Linux HCS or WSL backend before registering a Windows composite.
- **Windows arm64 guests on vz.** Separate from running Linux Docker on a
  Windows host; revisit through its own feasibility and guest-driver proof.
- **Automated import of existing dockerworker pools.** Revisit after a
  versioned, verified migration preserves roots, volumes, caches and uid
  ownership; first rollout uses new pools and explicit draining.
