# boxd driver (any host, Linux guest in boxd's cloud)

**Scope:** `pkg/machine/boxd`. Nothing of disco-vm's runs in a boxd guest: its
operations (`machine.Guest`) are command lines over boxd's Exec stream (ADR
0001), and a machine's power is boxd's API alone (ADR 26-10-15-833).

**Done means:**

1. `machinetest.Run` passes against boxd itself: `TestConformance` with
   `BOXD_API_KEY` set, starting from boxd's default image.
2. `internal/e2e` passes with `DISCO_VM_DRIVER=boxd`.
3. A fresh VM of boxd's default image is shaped through `exec`, stopped, and
   committed, and a VM created from the committed one runs.

Status: on 2026-10-07, with an agent in the guest, `TestConformance` passed
against boxd. On 2026-10-15 and 16, with no agent (ADR 0001), the CLI built
and ran images through `run`, `exec` (`-t` included), `cp`, `stop`, `start`,
`snapshot`, `rm`, and `rmi`, with nothing left on boxd; images were built
then with `disco-vm build`, which is gone (ADR 26-10-23-410), and the
default image ran and was snapshotted with no build. `TestConformance` has
not been run against boxd since, because a discobox's five-minute credential
does not outlive it. Criterion 2 waits on a layout for
`DISCO_VM_DRIVER=boxd` in `internal/e2e`.
Since ADR 26-11-03-606 (images, tags, and bases give way to committed VMs)
the driver has run against the fake API only.
`TestConformanceFakeAPI` runs the same suite in CI against an in-process fake
of boxd's API (`fakeapi_test.go`), whose "machines" are directory roots: its
Exec reads back the driver's command lines and runs them on the host.

## What boxd is

boxd (https://boxd.sh, docs at https://docs.boxd.sh) runs Linux KVM microVMs
in the cloud: Ubuntu 24.04, linux/amd64, a fixed 100 GiB disk, and four sizes
(1 vCPU/4 GiB, 2/8, 4/16, 8/32). Everything goes through one public gRPC API,
`boxd.api.v1.BoxdApi` at `boxd.sh:9443`, which the `boxd` CLI and SDKs also
use. The parts that matter here:

- **Snapshots** capture a *running* machine's memory and disk, and
  `CreateVmFromSnapshot` restores one into a new machine, any number of times.
- **Exec** is a bidirectional stream: the first chunk names the machine and a
  shell command, later chunks carry stdin, and the server streams stdout,
  stderr, and an exit code. Cancelling the call kills the command.
- **Stop/Start, Suspend/Resume, Hibernate/Wake** change state. Machines have
  idle timers for auto-suspend and auto-hibernate (4 hours by default).
- **Auth** is an API key (`bxd_...`, from `boxd auth keys create`) exchanged at
  `https://app.boxd.sh/api/v1/auth/token` for a JWT that lives an hour.

## How the seam maps

| seam | boxd |
|---|---|
| `Install` | not offered (`Capabilities.Install` is off): a fresh VM starts from boxd's image. No agent either: `Capabilities.Agent` is off. |
| `Create` | the machine `dvm-<project>--<name>`, at most 63 characters: fresh, with `CreateVm` (restart policy `never`) of the driver's base (`machine.Config.Base`), or of `GetConfig`'s `default_image` when it has none; or, from a committed VM, restored from its snapshot by name (`CreateVmFromSnapshot`, at the VM's size when it asks for one); a `From` that is no ready snapshot of its name and ID, a machine or a snapshot gone, is `ErrNotFound`, since boxd restores only a snapshot. An `Egress` set at once and read back ([Egress](#egress)), idle timers off, then the proxy pinned (`SetBotProtection` off, `SetProxyPort` pins the default proxy, the one `ListProxies` marks `is_default`, to 7302), then `sync` in the guest, `StopVm`, and wait for `stopped`: the first boot is a cold boot of the disk. A step that fails destroys the machine. A name the project has, as a machine or a committed VM's snapshot, is `ErrExists`. |
| VM | `Instances` is `ListVms` and `ListSnapshots`, keeping the project's names whose rest is a VM name (`machine.ValidateName`). That a VM is committed is the caller's to record (ADR 26-11-03-606); the driver tells a committed VM by its kind, the ready snapshot of its name whose ID is the VM's, when a call on it as a machine fails. A VM is its machine: its ID, its name less the project's prefix, its size, when it was made, and its isolation, networks, and egress allowlist. boxd does not say which snapshot a machine was restored from, so `From` is empty. A committed VM is a `ready` snapshot of the same name: its ID is the snapshot's, its size and creation the snapshot's. A snapshot and a machine of one name are a `Commit` that has not deleted the machine yet: the committed VM is listed, and the machine is not. `Instance` finds a name among them. |
| `Boot`, `Start` | a committed VM, a snapshot with no machine, is `ErrCommitted`. `StartVm` (after `ResizeVm` when the instance asks for another size), `ResumeVm`, or `WakeVm`, by state. A machine still `stopping` is waited out first. Returns once the guest answers: an Exec of `sudo -n systemctl is-system-running --wait` returns, once a second, for up to 5 minutes. A machine that stops meanwhile (`GetVm` every 2s) ends the wait. boxd owns the machine, so `Start` is `Boot`. |
| `Attach` | a committed VM is `ErrNotRunning`. `GetVm`: `running` or `starting` is the machine; not found, `stopped`, or any other state is `ErrNotRunning`. |
| `Machine.Done` | watching starts on the first `Done`, `Err`, or `Kill`, so a machine attached only to run a command polls nothing. `GetVm` every 2s: `stopped` is a power-off, and `failed`, `destroying`, or not-found is an error. |
| `Machine.Kill` | `StopVm`, retried until boxd takes it, then wait for `stopped`. Callers pass no deadline, so Kill has its own (5 minutes). Past it, Kill gives the machine up: `Done` closes, and `Err` says it may still be running. The machine is still listed, so `rm` destroys it later. |
| `Machine.Exec` | one Exec stream: `sudo -n -H -u USER -- env -C DIR -- K=V... ARGV...`, every word single-quoted (a program whose path has an `=` in it would be read by `env` as a variable). The user is root unless the request names one, and the directory `/` unless it names one. sudo gives the account's HOME, USER, LOGNAME, and `secure_path`; the request's variables are added to them. Stdin chunks, a half-close for EOF, stdout and stderr apart, and the exit code in the last chunk. A TTY sets `tty` with `rows` and `cols`, and a resize is a chunk with `window_change`. Closing the process cancels the call, which kills the command. |
| `Machine.CopyTo` | Exec of `tar -x --no-same-owner --no-overwrite-dir` as root into the directory, made first, with the tar as stdin: what it unpacks is root's, and a directory already there keeps its mode, as with an agent. |
| `Machine.CopyFrom` | Exec of a script that fails on a missing path, else writes one ready byte, then `tar -c` of the path rooted at its base name. |
| `Machine.Info` | Exec of `uname -m; hostname`: linux, the GOARCH of the machine, its hostname. No version: there is no agent. |
| `Machine.Shutdown` | a power-off is an Exec of `sync` as root, then `StopVm`; the watcher sees `stopped`. A reboot is `RebootVm`. Nothing in the guest is asked to power off. |
| `Commit` | a committed VM is `ErrCommitted`, and the machine must be `stopped`, after an orderly shutdown. Start it, wait for the guest, snapshot it as `dvm-<project>--<name>`, the machine's own name, wait for the snapshot `ready`, and destroy the machine: boxd snapshots only a running machine. The snapshot's disk is the one the guest flushed. The committed VM keeps nothing else. |
| `Destroy` | `DestroyVm`; one already gone is not an error. A committed VM, the ready snapshot of its name and ID: `DeleteSnapshot`, and `DestroyVm` of any machine of its name a `Commit` left. A machine restored from the snapshot has a disk of its own, and outlives it. |
| `Service` | a committed VM is `ErrCommitted`. Listen on `tcp:0.0.0.0:7302`; the URL is `https://` and the default proxy's domain, `ListProxies`, the same across stop and start. The private URL is `http://<machine>.boxd:7302` for a machine on networks, and the URL for one on none. |

Exec runs as boxd's `boxd` user, who has passwordless sudo, so every guest
operation goes through `sudo -n`, as root by default. An image installed
before the driver stopped carrying an agent still has
`disco-vm-guest.service`, and one installed before ADR 26-10-15-833 a
shutdown hook in `/usr/lib/systemd/system-shutdown/disco-vm`; nothing reads
either, and they do no harm. A VM committed from a fresh VM of boxd's default
image has neither.

Nothing is kept on the host: every name carries what disco-vm knows, and the
API reports the rest, so any host with the account's key sees the same VMs.
What disco-vm made before ADR 26-11-03-606 is not read (as ADR
26-10-23-417): an image's snapshot, `dvm-<project>--<64-digit ID>`, a tag's,
with `--t-` in its name, and a snapshot named before projects
(`dvm-<id>-<random>`) are not listed; delete them with `boxd snap rm`. A
machine named as before is listed under the name after its project's
prefix where that is a VM name, and `rm` removes it.

## Try it

From a checkout, on any host:

```sh
go tool task build                                      # or go build -o disco-vm ./cmd/disco-vm, on any host

export BOXD_API_KEY=$(boxd auth keys create disco-vm)   # once; keep it somewhere safe
export DISCO_VM_DRIVER=boxd

./disco-vm info                                         # check: ok means the key works
./disco-vm run stock                                    # a fresh VM of boxd's default image
./disco-vm exec stock sh -c 'apt-get update && apt-get install -y tree'
./disco-vm stop stock && ./disco-vm commit stock        # stock is now a snapshot, and no machine
./disco-vm ls -a                                        # stock, committed
./disco-vm run --from stock dev
./disco-vm exec dev tree --version
./disco-vm exec -it dev bash
./disco-vm cp ./README.md dev:/tmp
./disco-vm url dev                                      # https://<machine>.boxd.sh
./disco-vm stop dev && ./disco-vm start dev
./disco-vm rm -f dev
./disco-vm rm stock
```

Each VM is a machine named `dvm-default--<name>` in `boxd ls`, and each
committed VM a snapshot of the same name in `boxd snap ls` (`--project`
names the project). `rm` destroys the machine, or deletes a committed VM's
snapshot; a VM created from it keeps running. disco-vm keeps nothing on the
host, so every host with the account's key sees the same VMs. A fresh VM is
made from the driver's base, `machine.Config.Base`, or boxd's default image
when it has none (ADR 26-11-03-606): a caller never names the image at
`create`, and a snapshot the account made by hand is not one disco-vm
reads.

## Configuration

| env | default | |
|---|---|---|
| `BOXD_API_KEY` | | an API key. `BOXD_TOKEN`, which the boxd CLI reads, also works, and may hold a JWT. The command line reads them (`machine.KeyFromEnv`); a program that embeds the engine gives the driver a `machine.KeySource` instead, and the driver reads neither. |
| `BOXD_GRPC_ADDR` | `boxd.sh:9443` | another cluster |
| `BOXD_TOKEN_URL` | `https://app.boxd.sh/api/v1/auth/token` | its key exchange |
| `HTTPS_PROXY` | | gRPC goes through it with HTTP CONNECT, as does the key exchange. A proxy that intercepts TLS has to offer HTTP/2 (ALPN `h2`) for gRPC to work through it. A discobox's egress proxy does, and the conformance suite passes through it (2026-10-08). |
| `DISCO_VM_DEBUG` | | any value is `disco-vm --debug`: one line on stderr for every API call, when it finished, how long it took, and its request and answer. A stream (Exec, an upload) is one line when it ends. |
| `ALL_PROXY` | | a `socks5://` or `socks5h://` proxy carries the gRPC connection instead, for an HTTP proxy without HTTP/2. The key exchange still goes through `HTTPS_PROXY`. |

A key is fenced to one org, so the org is the key's. There is no shim for a
boxd instance, so every command authenticates with its own environment, and a
credential only has to outlive the command it was given to. Inside a discobox,
whose `discobox-access run` hands out a sentinel that lives five minutes, that
is what makes an instance usable past its `run`. A single command longer than
that still outlives its sentinel. `ls` shows an instance boxd cannot be asked
about as `unknown`, and an engine `CreateOptions.Forwards` is refused:
nothing would hold the forwards' listeners. A shim an older disco-vm started for an instance is ended
the next time the instance is started, stopped or removed: it is asked for its
status with its token first, so a reused PID is never signaled, and the
machine is left as it is.

## Keys

`disco-vm key issue --ttl D` (`Driver.IssueKey`) creates a `member` API key
named `dvm-<project>--<name>`, fenced to the engine's key's org, expiring
after the TTL. `key ls` and `key revoke ID` read `ListApiKeys` and delete
only such a key of the caller's. What boxd's kinds reach, from its API's
documentation:

- **`member`** acts as its maker within one org: every machine of theirs
  there, not only this project's. disco-vm reports it as scope `org`. It is
  the narrowest key that reaches disco-vm's machines.
- **`org`** is a userless key for the org's shared fleet only, and needs an
  org admin. disco-vm's machines are private to their maker, so it reaches
  none of them.

Against boxd (by hand, 2026-10-24, from an API key): `CreateApiKey` and
`ListApiKeys` answered `PermissionDenied`, "this operation requires an
interactive session (`boxd login`) — API keys are fenced to one org and
cannot manage the account". So:

- **An issued key cannot issue another.** It is an API key, and boxd refuses
  key management to every API key.
- **The driver issues only from a `boxd login` session.** From an API key,
  `key issue` is `ErrCannotIssue`, with boxd's reason. A program that holds
  an API key in configuration can open an engine with it, but issues nothing
  from it.

Not run yet: issuing from a `boxd login` session, and an engine on another
host running the project's machines with the key it issued. Nor whether
boxd stops honoring the JWT an issued key was exchanged for (an hour's)
once the key is revoked or expires: until that is shown, a holder who
exchanged it may keep working for up to that hour, whatever the TTL.

## Isolation

`run --isolated` and `--network N` (`InstanceSpec.Isolated`, `Networks`)
are `CreateVm`'s `isolated` and `networks`. `isolated` strips the in-VM boxd
CLI, the git credential helper, the integrations, and the default network's
peers. It is immutable, and a restore from a snapshot of an isolated machine
is isolated (boxd's API docs). A restore takes no networks of its own, so the
driver sets them with `SetVmNetworks`, and refuses networks on a restore that
is not asked `--isolated`: until they are set it would peer on the default
network. An isolated machine with networks
peers with exactly the non-isolated machines on them, which is how a machine
that holds no token reaches one that does. Against boxd (2026-10-24): `run
--isolated` of the default image made a machine `GetVm` reports `isolated`.

## Egress

`run --egress HOST` (`InstanceSpec.Egress`; ADR 26-10-29-129) fences what the
machine reaches beyond its networks to boxd's egress allowlist: hostnames,
`*.domain` wildcards, public IPv4 addresses and ranges. `--egress none`, an
empty `Allow`, is boxd's `["none"]`, which reaches nothing on the internet
but the hosts of the host-bound secrets the machine holds: boxd admits those
on top of any allowlist. A machine holds every secret in its scope: the
org's `all` secrets, its `shared` ones on a shared machine, and its owner's
`private` ones on a private machine (disco-vm's are its key's owner's), and
`isolated` does not strip them, only keeps them from boot (boxd's
docs.boxd.sh/guides/env-secrets). A secret is scoped to the org or a person,
never to a network. disco-vm binds none, so a caller that must reach nothing
but its networks keeps host-bound secrets out of every scope its machines
are in. Not checked against boxd: the org of the run below held none.
boxd's empty allowlist is none at all. boxd enforces it outside the machine, where its
root cannot loosen it, and its resolver answers only the names the list
admits. Neither `CreateVm` nor `CreateVmFromSnapshot` takes it, so Create
calls `SetVmEgressAllow` as soon as either answers, before the idle timers,
the networks, and the proxy, and reads it back from `GetVm`'s
`egress_allow`: a machine that does not report it as asked is destroyed.
The read-back compares entries as written, in any order, so an entry boxd
rewrites (letter case, a range not at its network address) fails Create
rather than pass; write them as boxd keeps them. Between the create's answer
and the allowlist taking hold, a second or so, the machine runs unfenced:
that is the guest's own boot, or a restored guest's processes resuming,
never anything of the caller's, which Create has not returned to yet.
It is the spec's: a restore starts unrestricted, unless its snapshot is of
a boxd machine spec with `egress`, which disco-vm never makes.

The private URL (`url --private`) is where a peer on the machine's networks
reaches its service: `http://<machine>.boxd:7302`, boxd's internal DNS name
of the machine, which resolves only on machines of the same owner, and
follows the networks rule. Every port the guest listens on is reachable
there, and nothing crosses boxd's proxy. A machine on no networks has its
URL as its private URL.

Against boxd (2026-10-29, `computer:0.1.53`): an isolated machine on network
`dvm-t25` with `--egress none` reported egress `{}`. Its resolver answered
`dvm-vtest25--dvm-hub.boxd` (a non-isolated machine on the same network) and
no internet name, and a connection to `1.1.1.1:443` by address was closed in
the TLS handshake. It reached the hub's private URL and its port 8080, and
did after a stop and a start. Restores of its snapshot got the fence each
asked for: `none`, `example.com` (example.com answered, github.com did not
resolve), and none at all, which reached both.

## The URL

Every boxd machine has a default proxy at `<machine>.boxd.sh`, which
terminates TLS with a managed certificate and forwards HTTP, websockets
included, to one port of the machine. Left alone, it forwards to the first of
80, 443, 8080, 8000, 3000, 5000, and 5173 the guest listens on, else 8000.
Create pins it to 7302, the port every instance's service listens on
(`tcp:0.0.0.0:7302` in the guest's service file), so the service is the only
thing it reaches, and turns bot protection off. The URL is recorded with the
instance, so `disco-vm url` and `Engine.URL` answer without a credential, and
it stays the same across stop and start. Anyone who has it reaches the
service, which authenticates its callers itself (discobox ADR 26-10-09-143
§3).

## A guest cannot power its machine off

Found against boxd on 2026-10-07. `systemctl poweroff` in a boxd guest stops
every unit and runs `systemd-shutdown`, which syncs and asks the kernel to
power off. The machine never stops: boxd keeps reporting `running`, with
nothing left in it but boxd's own exec path. A machine restored from a snapshot
also comes back with `restart_policy: always`, whatever its source had, and a
restore cannot set one.

So nothing of disco-vm's asks the guest to power off (ADR 26-10-15-833). A
stop is `sync` in the guest, then `StopVm`, which leaves the machine
`stopped` despite `restart_policy: always`; `StartVm` then cold boots the
disk, with the guest answering again in about 10 seconds. A reboot is
`RebootVm`. A guest that powers itself off from inside is left `running` by
boxd, and so by disco-vm, until `disco-vm stop`. Until that ADR, Install put
a hook in `/usr/lib/systemd/system-shutdown/` that marked the halt for the
driver to finish; images built then still have it, and nothing reads it.

The same run found the agent, which boxd guests then carried, ignoring
SIGTERM, so systemd waited out its 90-second stop timeout on every shutdown.
The CLI turns SIGTERM into a cancelled context, and `disco-vm guest` closes
its listener on it.

## Confirmed against boxd

- With no agent (2026-10-15): every guest operation as a command line
  through sudo. `systemctl --no-block poweroff`'s Exec returns exit 0 before
  the shutdown takes the exec path down, so an orderly stop and a snapshot's
  commits go through. Exec's `rows` and `cols` (fields 8 and 7) size the pty.
  GNU `env` ends its options at the first `NAME=VALUE`, so its `--` comes
  before the variables. The first build of the since-removed `docs/examples/boxd.yaml` with no
  agent took 2.5 minutes, and a `run` 23 s.

- The URL (2026-10-09, then `TestEndpoint` with an image-declared port; not
  rerun since the port became a fixed 7302): `SetProxyPort` pins a restored
  machine's default proxy to the service port, a guest's HTTP server listening
  on all interfaces answers at `https://<machine>.boxd.sh` within a few
  retries, and after a stop and a start the proxy is still locked to the port
  and answers again. The default proxy is not named `default`: `SetProxyPort`
  with that name is NotFound, so the driver uses the name `ListProxies` gives
  the proxy it marks `is_default`.
- Exec carries tar streams and exit codes unmodified, without a TTY. A
  half-close reaches the command as EOF. `command` runs through a shell as
  the `boxd` user, who has passwordless sudo.
- Exec runs in its own process namespace: `ps` there shows only the exec'd
  command, and systemd is reachable only through `sudo systemctl`.
- `GetVm` reports `running` and `stopped`, and `CreateSnapshot` reaches `ready`
  through `GetSnapshot` within seconds.
- A restore gets its own name and hostname (`dvm-<id>-<rand>`).
- Timings for a 2 vCPU/8 GiB machine, measured with the agent: a cold `run`
  takes 15s, which includes the restore and power-off in Prepare. `stop`
  takes 4s and `start` 3s.

## Not verified yet

- `ResizeVm` on a stopped machine, which a boot at a size other than the
  snapshot's uses. It passes against the fake only.
- Committed VMs (ADR 26-11-03-606) on boxd itself: a snapshot named as its
  machine is, taken while the machine still exists. It passes against the
  fake only.
- The `suspended`/`standby` status strings, and `WakeVm` from `hibernated`.
  The driver turns off auto-suspend and auto-hibernate on every machine, so
  these show up only when something else suspends one.
- A guest reboot (`Shutdown(reboot=true)`), which the driver turns into a stop
  and a start. Nothing in disco-vm sends one today.
- That cancelling an Exec kills what sudo started in the guest, and a TTY
  resize (`window_change`), which the CLI does not send yet.
- Load. Each guest operation is one Exec call, waiting for a guest makes one
  a second, and a watched machine (one being stopped) makes one
  Exec and one `GetVm` call every 2s. Every engine call on an instance
  attaches first: one `GetVm` and one Exec for the shutdown marker.

## A machine given up

When Kill gives a machine up (`stop` or `rm -f` whose orderly shutdown
failed, and boxd then would not stop it within five minutes), the command
fails with an error that says the machine may still be running, and billing.
`ls` asks boxd, so it shows the machine as it is. A later `stop` or `rm -f`
tries again.

## Not done

- `internal/e2e` hard-codes `DISCO_VM_DRIVER=fake`, so criterion 2 needs it to
  take the driver from the environment first.
- No shared directories, and no display. boxd's `DesktopTunnel` is an xpra
  stream, which could back `Display` later.
- `MaxRunning` is unset. The org's machine quota (50 by default, 2 for a new
  org without a payment method) is not reported as a capability.
