# archil driver (any host, Linux guest in Archil's cloud)

**Scope:** `pkg/machine/archil`. No agent of disco-vm's runs in an archil
guest: its operations (`machine.Guest`) are Archil's process API (ADR
26-11-03-676), entering the namespaces of the systemd the driver has Archil's
supervisor run in every guest (ADR 26-11-20-447).

**Done means:**

1. `machinetest.Run` passes against Archil itself: `TestConformance` with
   `ARCHIL_API_KEY`, `ARCHIL_REGION`, and `DISCO_VM_ARCHIL_IMAGE` set, and
   against the fake API (`TestConformanceFakeAPI`) everywhere.
2. `internal/e2e` passes with `DISCO_VM_DRIVER=archil` (`go tool task
   test:archil` runs both against Archil).
3. A fresh VM of an OCI image is shaped through `exec`, stopped, and
   committed, and a VM created from it runs.

Status: see the root [DESIGN.md](../../DESIGN.md#what-is-proven-and-where)'s
proof table.

## What Archil is

Archil (https://archil.com, docs at https://docs.archil.com) runs Firecracker
microVM sandboxes, amd64 only, from public OCI images, beside disks backed by
object storage that the provider mounts in them. Its control plane is a REST
API per region (the configuration's `region`: `--archil-region`,
`$ARCHIL_REGION`, required; ADR 26-11-11-215), authenticated
with an API key (`ARCHIL_API_KEY`); there is no Go SDK. Processes run over a
WebSocket protocol only its TypeScript SDK implements.

A sandbox's root disk persists across stop and start. A sandbox carries
up to 32 labels, keys and values of 1 to 63 letters, digits, `.`, `_`, or
`-`, set only when it is made or forked, never changed, invisible to the
guest, and a listing filters by them (`label=key=value`, repeated and
ANDed), by `name`, and by `name_prefix`. A disk takes no labels, and a disk
create answers the disk of its name, 200, rather than refuse it (201 when it
makes one); a fork takes a name and labels, none of `mounts`, `network`, or
`vcpu_count` (seen 2026-11-24). PID 1 is Archil's own
init, which runs nothing of the image's: no entrypoint, no systemd. A
service of its supervisor in the guest (`/opt/archil/archil-client
services`, not in Archil's documented CLIs) starts at once, runs again when
it exits, and comes back at every boot. A stop
checkpoints the root disk, and a fork branches a checkpoint into a new
sandbox that boots at once with its source's mounts and network policy. A
sandbox with forks can't be deleted. A session lasts at most 24 hours, then
Archil pauses it. A request to an exposed port, or to an unexposed one with
a port token (`X-Archil-Token`), starts a stopped sandbox, or resumes a
paused one; a request without the token is answered `401` and wakes
nothing.

## How the seam maps

| seam | archil |
|---|---|
| `Install` | not offered: a fresh VM runs the base. |
| base | `Config.Base` (`$DISCO_VM_BASE`), an OCI image Archil pulls, or Archil's default image when empty; declared `machine.OCIImage`, so an embedder sets it to an image of its own. |
| VM | the live sandbox labeled `ai.discobox.vm.pool` and `ai.discobox.vm.instance` (ADR 26-11-24-905), and, for a copy, `ai.discobox.vm.from`. `Instances` asks Archil for the pool's sandboxes by label (`label=ai.discobox.vm.pool=<pool>`; a VM's adds its instance's, and Archil ANDs them) and lists one per VM, with its size, creation, fence (`GET .../network`), data disk, and `From`, its label's: Archil does not say which sandbox a fork came from. Of several labeled for a VM, one an upgrade made to replace another still live (`ai.discobox.vm.replaces`) is not the VM, and of the rest the oldest is. A sandbox with no labels is no VM. |
| `Create`, fresh | refused `ErrExists` where a sandbox is the VM already; then a sandbox `dvm-<pool>--<name>` of the base at the size asked, with its fence, its labels, and `enable_service_ingress: false`; then the ingress mark where ingress is asked off, read back with the fence, a stop, and the data disk. No port is exposed. `sandbox_name_conflict` is `ErrExists`, and so is an older sandbox of the VM found once this one is made (a race), which removes this one. With `Start` (ADR 26-11-23-458), the data disk is made first and mounted by the sandbox's create, and the sandbox is left running: one start. A data disk of the name already there (Archil's 200, not 201) may be a racing create's, so it is left, the sandbox made first, and it is replaced (a removed VM's) only once this create holds the name, and the VM started once more with it mounted. |
| `Create`, `From` | a fork of the source's sandbox, which must be stopped and have no mounts (commit it first), given its own labels (a fork given none keeps its source's; it takes no mounts, seen 2026-11-24); the spec's fence and ingress mark set (an unfenced spec clears the source's fence); every token the fork carried revoked; then a stop, and the data disk. A size other than the source's is refused. With `Start`, it is left running, or, with a data volume, stopped and started once with it mounted. |
| `Commit` | reported (`Capabilities.Commit`, ADR 26-11-03-868). A stopped sandbox with mounts is started with none and stopped; then its port tokens are revoked and the data disk is deleted. Nothing in the guest is changed, and nothing records the commit. |
| `Destroy` | each sandbox labeled for the VM, the VM's own last: stop, revoke its port tokens, delete it; then the data disk. Archil refuses a sandbox with forks (`sandbox_has_forks`): `machine.ErrInUse`, and the data disk is kept. A VM with no sandbox left still has its data disk removed. |
| `Upgrade` | checks everything that could refuse it, removes what an earlier upgrade or a create of the name left, then makes the new sandbox first, fresh from the base or a fork of `from`, named `dvm-<pool>--<name, cut to fit>--<hex>`, at the old size, fence, and ingress, labeled as a Create's and `ai.discobox.vm.replaces` the old sandbox, with systemd launched, and stops it; then revokes the old sandbox's port tokens and deletes it. A failure before that removes the new sandbox and leaves the VM as it was; one stopped in between leaves a sandbox nothing lists, removed by the next `upgrade` or `rm`. The data disk is mounted at each start as before. A `failed` sandbox is upgraded as a stopped one. |
| systemd | every VM's, with no option (ADR 26-11-20-447): while Create's sandbox runs, under its fence, systemd, systemd-sysv, dbus, and util-linux are installed with apt where the base lacks `/lib/systemd/systemd` (or `nsenter`, `unshare`, `setpriv`, `runuser`, `mount`), and `disco-vm-systemd` is registered with Archil's supervisor (`/opt/archil/archil-client services create`), which runs `unshare --pid --fork --mount-proc --propagation slave --kill-child /lib/systemd/systemd` at every boot, the root made shared first, and kills `unshare`, so systemd, at TERM, Archil's stop's signal (a VM registered before keeps the old script: only the driver's stops of it, which kill systemd first, are prompt). A base with no `apt-get`, or a VM that can't reach its mirror (`--egress none`), fails Create naming `/lib/systemd/systemd`; Create returns once systemd's process runs (launched, not settled: its boot may still be going). A fork carries both: Create registers only where `services get` says there is none (exit 1), and fails on any other answer. A fresh `Upgrade` of a base without systemd, fenced from its mirror, fails before the old sandbox is deleted, and leaves the VM as it was. |
| `Boot`, `Start` | start the sandbox with its data disk mounted, or resume a paused one, then wait until a process runs and until systemd's process runs, for up to a minute, not for its boot to finish: a guest operation can enter its namespaces, and a unit may still be starting (a start installs and registers nothing; one whose systemd is not launched is stopped). Archil runs it, so `Start` is `Boot`. A committed VM is not refused: Archil can't tell it. `Boot` of a paused (or pausing) sandbox is `ErrPaused`. |
| `State` | `running` is running; `paused` or `pausing` is paused, read without resuming it; anything else is stopped. |
| `Pause` | Archil's caller pause (`POST .../pause`), waited out to `paused`; one paused is left so, one `pausing` waited out, one `pending` paused once it runs, and one stopped is `ErrNotRunning`. Archil keeps the sandbox's memory and processes, and `Start` resumes it (`Capabilities.Pause`). |
| `Attach` | running is a machine; paused or pausing is `ErrPaused`, left paused; anything else is `ErrNotRunning`. |
| `Machine.Done` | polled every 2s once asked: stopped or exited is a power-off, failed or deleted an error. |
| `Machine.Kill`, `Shutdown` | no shutdown in the guest: `Shutdown` kills systemd and flushes (`sync`, and `archil sync` of the data volume where it is mounted), then asks Archil's stop; a flush that fails is an error and asks no stop. `Kill` kills systemd where the guest answers, flushing no data volume, and the driver's own stops kill it and flush the data volume where mounted; both then stop it either way. A reboot is refused. |
| guest operations | a process each, entering systemd's namespaces (`nsenter --all` of the process whose PID in its namespace is 1, in the killed process's group, which Archil SIGKILLs whole, and under `setpriv --pdeathsig TERM`, again under `runuser -u` for another account), failing while systemd does not run: exec with the request's directory, variables, and terminal, `runuser -u` for another account; `cp` as tar, GNU's or BusyBox's, a copy out tarring to `/var/tmp` (so it needs that much free space in the guest, and streams nothing until tar is done) and writing `R` once that worked, then the tar in 512 KiB chunks the host asks for, read with `dd`, since Archil drops output that outruns its connection (an exec that loses output fails); `info` as `uname -m; hostname`. |
| `Listen` | `tcp:0.0.0.0:7302`. |
| `Service` | mints a port token for 7302 (`POST .../port-tokens`, a day's `ttl`) and gives `https://<the hostname Archil returns with it>` as the pool URL, and as the ingress URL unless asked off (`Capabilities.URLs`: ingress optional, pool always, `Credential`), with the token as `Credential` in `X-Archil-Token`. The hostname is the same across stop and start. A request without the token is refused with `401` before the guest, and leaves a stopped VM stopped; Archil speaks only HTTP/1.1 there. Each call mints a new token, which the driver does not keep. |
| volumes | `data`: the disk `dvm-<pool>--<name>--data` at `/.discobox/data`, made at Create (one of the name already there is a removed VM's, deleted and made again; disks take no labels) and deleted at Commit and Destroy. No `cache` (ADR 26-11-02-180). |
| egress | `network.egress` `{default: deny, allow: [...]}`, with `deny: ["8.8.8.8", "1.1.1.1"]` where it closes DNS (`Egress.CloseDNS`, and `none`, an empty `allow`), set at create (fresh) or right after the fork (`From`), and read back. The policy also carries the ingress mark, a `deny` of `192.0.2.86`, on a VM whose ingress URL is asked off. |
| bare | always (`Capabilities.Bare`): the guest holds nothing of the account, and no data disk's mount token is visible in the namespaces it runs in: Archil's client mounts the disk from outside them (seen on client version 1006, 2026-11; another base or older image may differ). |
| keys | none: an Archil token never expires and reaches the whole account (`ErrCannotIssue`). |

## Try it

```bash
export ARCHIL_API_KEY=key-... ARCHIL_REGION=aws-us-east-1 DISCO_VM_DRIVER=archil DISCO_VM_POOL=scratch DISCO_VM_BASE=debian:sid
disco-vm run dev --data 1GiB      # installs systemd: debian:sid has none
disco-vm exec dev systemctl is-system-running
disco-vm exec dev sh -c 'apt-get update && apt-get install -y curl'
disco-vm stop dev
disco-vm commit dev
disco-vm run dev2 --from dev
disco-vm rm -f dev2 && disco-vm rm dev
```

## Not done

- **Not yet seen on Archil itself:** a VM's sandbox paused and resumed
  with systemd in it, and an upgrade. The container test (`go tool task
  test:archil:container`) runs the driver's lines in containers with sh as
  PID 1 and a stand-in for the supervisor.
- **A cgroup namespace of systemd's own:** systemd takes the guest's cgroup
  root, beside Archil's init (ADR 26-11-20-447).

- **Committed VMs that stand alone**, if Archil lets a sandbox with forks be
  deleted.
- **The cache volume**, until Archil supports its branch API (ADR
  26-11-02-180).
- **No forward, no guest port, no networks, no display.**
- **Whether Archil caps the port tokens per sandbox** is not documented,
  and was not reached: a holder renews about once a day.

Seen on Archil (2026-11-21, aws-us-east-1, `debian:sid`): `run` installs
systemd in about 40 s, and systemd is `running` with no failed unit
(journald, logind, dbus, and a console getty), `systemd-detect-virt`
saying `container-other`; `services get` answers the service as JSON, exit
1 for none; a file copied to `/run`, the data disk, and a tmpfs mounted
after systemd started are read by a `systemd-run` unit; `exec -u nobody`
and `exec -t` run there; systemd runs again after `stop` and `start` (3.5
s; the stop took 31 s, then, before vm#217), and in `run --from` a committed VM with `--egress
none`, which installs nothing; `--egress none` of `debian:sid` (in 44 s, its apt failing fast as a fenced
VM's does; 2:52 with apt's retries) and a base of `alpine:3.20` fail `run`
naming `/lib/systemd/systemd`; an interrupted
`exec` leaves nothing running, no trap having run.

Seen on Archil (2026-11-23, aws-us-east-1, `debian:sid`, vm#217): the
supervisor's stop of a service is `stopping`, then `stopped` 30.0 s later
for one still running (`archil-client services events`), its grace; `POST
.../stop` documents no option to skip it. The guest has `archil` on its
`PATH` (`/usr/local/bin/archil`), and `archil-client sync` too. With
`systemdScript` ending at TERM and `haltScript` before the stop: `stop`
3.9 s (34.3 s before), 4.3 s for a VM whose service still has the old
script, a stop sent straight to the API 2.6 s, `run --data 1GiB` 17.0 s
(51.4 s before), `start` 6 to 7.7 s, `commit` 8.2 s, `run --from` 14.8 s;
a 64 MiB file and a marker written just before `stop` read back after
`start`. The supervisor logs a killed systemd `failed: signal` and
`restarting`, and Archil's stop came before the restart.

Seen on Archil (2026-11-18, aws-us-east-1): a fork carries none of its
source's tokens and has a hostname of its own, where the source's token is
answered `401`; a deleted sandbox's token is answered `401` at its old
hostname. The driver revokes them all the same (ADR 26-11-17-311 §4).
