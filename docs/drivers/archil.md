# archil driver (any host, Linux guest in Archil's cloud)

**Scope:** `pkg/machine/archil`. Nothing of disco-vm's runs in an archil guest:
what a guest holds and runs is the caller's, and its operations
(`machine.Guest`) are Archil's process API (ADR 26-11-03-676).

**Done means:**

1. `machinetest.Run` passes against Archil itself: `TestConformance` with
   `ARCHIL_API_KEY` and `DISCO_VM_ARCHIL_IMAGE` set.
2. `internal/e2e` passes with `DISCO_VM_DRIVER=archil`.
3. A fresh VM of an OCI image is shaped through `exec`, stopped, and
   committed, and a VM created from it runs.

Status: see the root [DESIGN.md](../../DESIGN.md#what-is-proven-and-where)'s
proof table. Criterion 2 waits on a layout for `DISCO_VM_DRIVER=archil` in
`internal/e2e`.

## What Archil is

Archil (https://archil.com, docs at https://docs.archil.com) runs Firecracker
microVM sandboxes, amd64 only, from public OCI images, beside disks backed by
object storage that the provider mounts in them. Its control plane is a REST
API per region (`$ARCHIL_REGION`, `aws-us-east-1` by default), authenticated
with an API key (`ARCHIL_API_KEY`); there is no Go SDK. Processes run over a
WebSocket protocol only its TypeScript SDK implements.

A sandbox's root disk persists across stop and start. PID 1 is Archil's own
init, which runs nothing of the image's: no entrypoint, no systemd. A stop
checkpoints the root disk, and a fork branches a checkpoint into a new
sandbox that boots at once with its source's mounts and network policy. A
sandbox with forks can't be deleted. A session lasts at most 24 hours, then
Archil pauses it.

## How the seam maps

| seam | archil |
|---|---|
| `Install` | not offered: a fresh VM runs the base. |
| base | `Config.Base` (`$DISCO_VM_BASE`), an OCI image Archil pulls, or Archil's default image when empty. |
| VM | the sandbox `dvm-<project>--<name>`. `Instances` lists the sandboxes under the project's prefix whose rest is a VM name, with each one's size, creation, fence (`GET .../network`), and data disk. Archil does not say which sandbox a fork came from: `From` is set only by the Create that made it. |
| `Create`, fresh | a sandbox of the base at the size asked, with its fence; then 7302 exposed, a stop, and the data disk. `sandbox_name_conflict` is `ErrExists`. |
| `Create`, `From` | a fork of the source's sandbox, which must be stopped and have no mounts (commit it first); the spec's fence set (an unfenced spec clears the source's); then 7302 exposed, a stop, and the data disk. A size other than the source's is refused. |
| `Commit` | reported (`Capabilities.Commit`, ADR 26-11-03-868). A stopped sandbox with mounts is started with none and stopped; then the data disk is deleted. Nothing in the guest is changed, and nothing records the commit. |
| `Destroy` | stop, delete the sandbox, then its data disk. Archil refuses a sandbox with forks (`sandbox_has_forks`): `machine.ErrInUse`. A sandbox already gone still has its data disk removed. |
| `Boot`, `Start` | start the sandbox with its data disk mounted, or resume a paused one, then wait until a process runs. Archil runs it, so `Start` is `Boot`. A committed VM is not refused: Archil can't tell it. |
| `Attach` | running or paused is a machine, a paused one resumed by its first exec or copy; anything else is `ErrNotRunning`. |
| `Machine.Done` | polled every 2s once asked: stopped or exited is a power-off, failed or deleted an error. |
| `Machine.Kill`, `Shutdown` | Archil's stop. A reboot is refused. |
| guest operations | a process each: exec with the request's directory, variables, and terminal, `runuser -u` for another account (an image without util-linux, such as Alpine, fails `exec -u`); `cp` as tar, GNU's or BusyBox's, a copy out tarring to `/var/tmp` (so it needs that much free space in the guest, and streams nothing until tar is done) and writing `R` once that worked, then the tar in 512 KiB chunks the host asks for, read with `dd`, since Archil drops output that outruns its connection (an exec that loses output fails); `info` as `uname -m; hostname`. |
| `Service` | listen on `tcp:0.0.0.0:7302`; the URL is `https://<hostname Archil gives port 7302>`, the same across stop and start, and the private URL is the URL. |
| volumes | `data`: the disk `dvm-<project>--<name>--data` at `/.discobox/data`, made at Create (one of the name already there is removed first, with or without `--data`) and deleted at Commit and Destroy. No `cache` (ADR 26-11-02-180). |
| egress | `network.egress` `{default: deny, allow: [...]}`, set at create (fresh) or right after the fork (`From`), and read back. |
| keys | none: an Archil token never expires and reaches the whole account (`ErrCannotIssue`). |

## Try it

```bash
export ARCHIL_API_KEY=key-... DISCO_VM_DRIVER=archil DISCO_VM_PROJECT=scratch DISCO_VM_BASE=debian:trixie
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
- **No forward, no guest port, no isolation, no display.**
