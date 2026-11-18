# archil driver (any host, Linux guest in Archil's cloud)

**Scope:** `pkg/machine/archil`. Nothing of disco-vm's runs in an archil guest:
what a guest holds and runs is the caller's, and its operations
(`machine.Guest`) are Archil's process API (ADR 26-11-03-676).

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

A sandbox's root disk persists across stop and start. PID 1 is Archil's own
init, which runs nothing of the image's: no entrypoint, no systemd. A stop
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
| base | `Config.Base` (`$DISCO_VM_BASE`), an OCI image Archil pulls, or Archil's default image when empty. |
| VM | the sandbox `dvm-<pool>--<name>`. `Instances` lists the sandboxes under the pool's prefix whose rest is a VM name, with each one's size, creation, fence (`GET .../network`), and data disk. Archil does not say which sandbox a fork came from: `From` is set only by the Create that made it. |
| `Create`, fresh | a sandbox of the base at the size asked, with its fence and `enable_service_ingress: false`; then the ingress mark where ingress is asked off, read back with the fence, a stop, and the data disk. No port is exposed. `sandbox_name_conflict` is `ErrExists`. |
| `Create`, `From` | a fork of the source's sandbox, which must be stopped and have no mounts (commit it first); the spec's fence and ingress mark set (an unfenced spec clears the source's fence); 7302 unexposed and every token the fork carried revoked; then a stop, and the data disk. A size other than the source's is refused. |
| `Commit` | reported (`Capabilities.Commit`, ADR 26-11-03-868). A stopped sandbox with mounts is started with none and stopped; then its port tokens are revoked and the data disk is deleted. Nothing in the guest is changed, and nothing records the commit. |
| `Destroy` | stop, revoke the sandbox's port tokens, delete it, then its data disk. Archil refuses a sandbox with forks (`sandbox_has_forks`): `machine.ErrInUse`. A sandbox already gone still has its data disk removed. |
| `Upgrade` | checks everything that could refuse it, then deletes the stopped sandbox and makes it again under its name, fresh from the base or a fork of `from`, at the old size, fence, and ingress, its port tokens revoked before the delete; the data disk is mounted at each start as before. Stopped in between, the VM is its data disk alone, listed with no size and the fence `none`: Start refuses it, `rm` removes it, and `upgrade` again finishes it, at the fence `none` and with ingress off, the strictest, since nothing else of it is left. A VM with no data disk is gone if the upgrade fails or stops after the delete, and the error says so (the one exception to ADR 26-11-03-782 §3). A `failed` sandbox is upgraded as a stopped one. |
| `Boot`, `Start` | unexpose 7302 (a VM made before ports were left unexposed has it exposed) and start the sandbox with its data disk mounted, or resume a paused one, then wait until a process runs. Archil runs it, so `Start` is `Boot`. A committed VM is not refused: Archil can't tell it. |
| `Attach` | running or paused is a machine, a paused one resumed by its first exec or copy; anything else is `ErrNotRunning`. |
| `Machine.Done` | polled every 2s once asked: stopped or exited is a power-off, failed or deleted an error. |
| `Machine.Kill`, `Shutdown` | Archil's stop. A reboot is refused. |
| guest operations | a process each: exec with the request's directory, variables, and terminal, `runuser -u` for another account (an image without util-linux, such as Alpine, fails `exec -u`); `cp` as tar, GNU's or BusyBox's, a copy out tarring to `/var/tmp` (so it needs that much free space in the guest, and streams nothing until tar is done) and writing `R` once that worked, then the tar in 512 KiB chunks the host asks for, read with `dd`, since Archil drops output that outruns its connection (an exec that loses output fails); `info` as `uname -m; hostname`. |
| `Listen` | `tcp:0.0.0.0:7302`. |
| `Service` | mints a port token for 7302 (`POST .../port-tokens`, a day's `ttl`) and gives `https://<the hostname Archil returns with it>` as the pool URL, and as the ingress URL unless asked off (`Capabilities.URLs`: ingress optional, pool always, `Credential`), with the token as `Credential` in `X-Archil-Token`. The hostname is the same across stop and start. A request without the token is refused with `401` before the guest, and leaves a stopped VM stopped; Archil speaks only HTTP/1.1 there. Each call mints a new token, which the driver does not keep. |
| volumes | `data`: the disk `dvm-<pool>--<name>--data` at `/.discobox/data`, made at Create (a data disk of the name with no sandbox is a VM mid-upgrade, so Create of that name is `ErrExists`) and deleted at Commit and Destroy. No `cache` (ADR 26-11-02-180). |
| egress | `network.egress` `{default: deny, allow: [...]}`, with `deny: ["8.8.8.8", "1.1.1.1"]` where it closes DNS (`Egress.CloseDNS`, and `none`, an empty `allow`), set at create (fresh) or right after the fork (`From`), and read back. The policy also carries the ingress mark, a `deny` of `192.0.2.86`, on a VM whose ingress URL is asked off. |
| bare | always (`Capabilities.Bare`): the guest holds nothing of the account; a data disk's mount token reaches only that disk. |
| keys | none: an Archil token never expires and reaches the whole account (`ErrCannotIssue`). |

## Try it

```bash
export ARCHIL_API_KEY=key-... ARCHIL_REGION=aws-us-east-1 DISCO_VM_DRIVER=archil DISCO_VM_POOL=scratch DISCO_VM_BASE=debian:sid
disco-vm run dev --data 1GiB
disco-vm exec dev sh -c 'apt-get update && apt-get install -y curl'
disco-vm stop dev
disco-vm commit dev
disco-vm run dev2 --from dev
disco-vm rm -f dev2 && disco-vm rm dev
```

## Not done

- **Committed VMs that stand alone**, if Archil lets a sandbox with forks be
  deleted.
- **The cache volume**, until Archil supports its branch API (ADR
  26-11-02-180).
- **No forward, no guest port, no networks, no display.**
- **Whether Archil revokes a sandbox's tokens at its delete, copies them to
  a fork, or caps them per sandbox** is not documented. The driver revokes
  them itself either way, so the answer changes only this page.
- **Unverified on Archil itself** (vm#186; the driver and its fake follow
  Archil's OpenAPI document, and `go tool task test:archil` has not yet run
  against them): a guest fenced with `--close-dns` resolving no name; `401`
  without a port token, and a stopped VM left stopped by one; the port and
  port-token calls taken on a stopped sandbox, which every Start, Destroy,
  and Commit makes; `DELETE …/ports/7302` of a port not exposed answering
  `404`; `enable_service_ingress: false` and a `deny` list read back as
  sent; and the ingress mark changing no traffic. The proof table's archil
  cells for them are ⬜ until that run.
