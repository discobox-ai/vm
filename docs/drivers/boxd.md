# boxd driver (any host, Linux guest in boxd's cloud)

**Scope:** `pkg/machine/boxd`. Nothing of disco-vm's runs in a boxd guest: its
operations (`machine.Guest`) are command lines over boxd's Exec stream (ADR
0001), and a machine's power is boxd's API alone (ADR 26-10-15-833).

**Done means:**

1. `machinetest.Run` passes against boxd itself: `TestConformance` with
   `BOXD_API_KEY` set, starting from boxd's default image, in pool
   `conformance`.
2. `internal/e2e` passes with `DISCO_VM_DRIVER=boxd`.
3. A fresh VM of boxd's default image is shaped through `exec`, stopped, and
   committed, and a VM created from the committed one runs.

`go tool task test:boxd` runs 1 and 2, from any host with a key.

Status: on 2026-10-07, with an agent in the guest, `TestConformance` passed
against boxd. On 2026-10-15 and 16, with no agent (ADR 0001), the CLI built
and ran images through `run`, `exec` (`-t` included), `cp`, `stop`, `start`,
`snapshot`, `rm`, and `rmi`, with nothing left on boxd; images were built
then with `disco-vm build`, which is gone (ADR 26-10-23-410), and the
default image ran and was snapshotted with no build. `TestConformance` has
not been run against boxd since, because a discobox's five-minute credential
does not outlive it. Since ADR 26-11-03-606 (images, tags, and bases give
way to committed VMs) the driver has run against the fake API only.
`TestConformance` makes its VMs in pool `conformance`, so it never lists or
removes one of someone's own, and runs the issued-key case with a key that
issues (a `boxd login` session's). `internal/e2e` has a layout for
`DISCO_VM_DRIVER=boxd` (2026-11-12): `e2e-base` is a fresh VM of boxd's
default image, or the one `DISCO_VM_E2E_IMAGE` names, committed, and the
suite writes under `/srv`. Neither has run against boxd itself yet.
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
  `https://app.boxd.sh/api/v1/auth/token` for a JWT that lives an hour, or,
  on a machine that is not isolated, with `inMachine`, a JWT its metadata
  adapter mints
  ([On a boxd machine](#on-a-boxd-machine)).

## How the seam maps

| seam | boxd |
|---|---|
| `Install` | not offered (`Capabilities.Install` is off): a fresh VM starts from boxd's image. No agent either: `Capabilities.Agent` is off. |
| `Create` | the machine `dvm-<pool>--<name>`, at most 63 characters: fresh, with `CreateVm` (restart policy `never`) of the driver's base (`machine.Config.Base`), or of `GetConfig`'s `default_image` when it has none; or, from a committed VM, restored from its snapshot by name (`CreateVmFromSnapshot`, at the VM's size when it asks for one); a `From` that is no ready snapshot of its name and ID, a machine or a snapshot gone, is `ErrNotFound`, since boxd restores only a snapshot. An `Egress` set at once and read back ([Egress](#egress)), idle timers off, then, unless its ingress URL is asked off, the proxy pinned (`SetBotProtection` off, `SetProxyPort` pins the default proxy, the one `ListProxies` marks `is_default`, to 7302), and for one asked off the default proxy closed, locked to port 0 (`SetProxyPort`), every other proxy removed (`DeleteProxy`), `ListProxies` read back with the default one alone and closed, and only then `SetBotProtection` off, then, once the guest is settled (as for `Commit`, but for up to 2 minutes in all, after which the create fails), `sync` in the guest, `StopVm`, and wait for `stopped`: the first boot is a cold boot of the disk. A step that fails destroys the machine. A name the pool has, as a machine or a committed VM's snapshot, is `ErrExists`: the snapshot of the name is read first (`GetSnapshot`, at once with the `From`'s own on a restore), and a machine of the name is boxd's to refuse, which it does with `FAILED_PRECONDITION` to `CreateVm` and `ALREADY_EXISTS` to `CreateVmFromSnapshot` (nyc, 2026-11-16), so a create refused with either reads the machine of the name (`GetVm`) to tell; any other error is the create's own. |
| VM | `Instances` is `ListVms` and `ListSnapshots`; `Instance` reads one name instead, `GetSnapshot` of it first (a ready snapshot is the committed VM) and `GetVm` of it only when there is no ready one, and the listing only when neither finds the name (boxd's `GetVm` has answered NotFound for a machine just made), so a lookup that finds its VM lists nothing; keeping the pool's names whose rest is a VM name (`machine.ValidateName`). That a VM is committed is the caller's to record (ADR 26-11-03-606); the driver tells a committed VM by its kind, the ready snapshot of its name whose ID is the VM's, when a call on it as a machine fails. A VM is its machine: its ID, its name less the pool's prefix, its size, when it was made, whether it is bare (boxd's `isolated`), and its networks and egress allowlist. boxd does not say which snapshot a machine was restored from, so `From` is empty. A committed VM is a `ready` snapshot of the same name: its ID is the snapshot's, its size and creation the snapshot's. A snapshot and a machine of one name are a `Commit` that has not deleted the machine yet: the committed VM is listed, and the machine is not. A `failed` snapshot with no machine of its name is listed the same way, a VM nothing runs, so that `Destroy` can remove it. `Instance` finds a name among them. |
| `Boot`, `Start` | a committed VM, a snapshot with no machine (a listed `failed` one too), is `ErrCommitted`. `Start` asks `StartVm` first, with nothing read before it: the expected machine is stopped, and boxd answers `StartVm` OK for one starting or running too, and resumes a suspended one. A `StartVm` refused is `ErrCommitted` for a committed VM's snapshot, and is otherwise left to the wait. The wait polls `GetVm` every 200 ms: a machine still `stopping` is waited out and started (after `ResizeVm` when the instance asks for another size), a suspended one resumed, a hibernated one woken. A machine `StartVm` started at another size than the instance asks boots at its old size first, so it is flushed (`sync`), stopped, resized and started again, once: a size change costs a second boot. One the wait first finds running, which Boot did not start, is left at its size, and one that stops once it has come up ends the wait with an error rather than being started again. Returns once `GetVm` says `running`: nothing in the guest is waited for. boxd holds an Exec sent while the guest boots until the guest answers (seen 2026-11-16, nyc and fra: about a second after `StartVm`); one it answers `Unavailable` before the guest answers anything is `machine.ErrUnreachable`, which the engine tries again. boxd owns the machine, so `Start` is `Boot`, but for one `suspended`, in `standby`, or `hibernated`, which `Start` resumes or wakes and `Boot` refuses with `ErrPaused`: `Boot` reads `GetVm` before anything else, and one NotFound for a machine just made reads as stopped and goes on to `StartVm`. |
| `State` | as `Attach`: `suspended`, `standby`, or `hibernated` is paused (the driver turns boxd's idle timers off, so only someone else's `SuspendVm` leaves one so), running is running, and anything else is stopped. |
| `Pause` | `SuspendVm`, then `GetVm` until it is `suspended`; one suspended, in standby, or hibernated is left so, and one not running is `ErrNotRunning` (`Capabilities.Pause`). `Start` resumes it. |
| `Attach` | a committed VM is `ErrNotRunning`. `GetVm`: `running` or `starting` is the machine; `suspended`, `standby`, or `hibernated` is `ErrPaused`, left as it is; not found, `stopped`, or any other state is `ErrNotRunning`. |
| `Machine.Done` | watching starts on the first `Done`, `Err`, or `Kill`, so a machine attached only to run a command polls nothing. `GetVm` every 2s: `stopped` is a power-off, and `failed`, `destroying`, or not-found is an error. |
| `Machine.Kill` | `StopVm`, retried until boxd takes it, then wait for `stopped`. Callers pass no deadline, so Kill has its own (5 minutes). Past it, Kill gives the machine up: `Done` closes, and `Err` says it may still be running. The machine is still listed, so `rm` destroys it later. |
| `Machine.Exec` | one Exec stream: `sudo -n -H -u USER -- env -C DIR -- K=V... ARGV...`, every word single-quoted (a program whose path has an `=` in it would be read by `env` as a variable). The user is root unless the request names one, and the directory `/` unless it names one. sudo gives the account's HOME, USER, LOGNAME, and `secure_path`; the request's variables are added to them. Stdin chunks, a half-close for EOF, stdout and stderr apart, and the exit code in the last chunk. A TTY sets `tty` with `rows` and `cols`, and a resize is a chunk with `window_change`. Closing the process cancels the call, which kills the command. |
| `Machine.CopyTo` | Exec of `tar -x --no-same-owner --no-overwrite-dir` as root into the directory, made first, with the tar as stdin: what it unpacks is root's, and a directory already there keeps its mode, as with an agent. |
| `Machine.CopyFrom` | Exec of a script that fails on a missing path, else writes one ready byte, then `tar -c` of the path rooted at its base name. |
| `Machine.Info` | Exec of `uname -m; hostname`: linux, the GOARCH of the machine, its hostname. No version: there is no agent. |
| `Machine.Shutdown` | a power-off is an Exec of `sync` as root, tried again while it is `machine.ErrUnreachable` for up to 2 minutes, then `StopVm`; the watcher sees `stopped`. A reboot is `RebootVm`. Nothing in the guest is asked to power off. |
| `Commit` | a committed VM is `ErrCommitted`, and the machine must be `stopped`, after an orderly shutdown. Start it, wait until the guest is settled (an Exec of `systemctl is-system-running`, which does not wait, every 200 ms, each for up to 5 s and in all up to 5 minutes, until it prints `running`, `degraded` or `maintenance`; its exit code and stderr count for nothing), snapshot it as `dvm-<pool>--<name>`, the machine's own name, wait for the snapshot `ready`, and destroy the machine: boxd snapshots only a running machine. A snapshot that boxd has answered for and then does not find was deleted (a `Destroy` of the machine), and the commit fails. The snapshot's disk is the one the guest flushed. The committed VM keeps nothing else. |
| `Destroy` | `DestroyVm`; one already gone is not an error. The snapshot of the machine's name is read first (`GetSnapshot`): `ready` is a `Commit` that finished, the committed VM, and stays; `pending` or `failed` is deleted (`DeleteSnapshot`) before `DestroyVm`, since boxd makes a pending snapshot `ready` once its machine is destroyed; and one that appears meanwhile, the machine's own `Commit`'s, is deleted after. A committed VM, the snapshot of its name and ID (ready, or a listed failed one): `DeleteSnapshot`, and `DestroyVm` of any machine of its name a `Commit` left. A machine restored from the snapshot has a disk of its own, and outlives it. |
| `Service` | a committed VM is `ErrCommitted`. Listen on `tcp:0.0.0.0:7302`; the ingress URL is `https://` and the default proxy's domain, `ListProxies`, the same across stop and start, and none for a machine whose default proxy is closed, locked to port 0 (one whose ingress URL was asked off), or that has none. The pool URL is `http://<machine>.boxd:7302` for a machine on networks, and the ingress URL for one on none. |

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
26-10-23-417): an image's snapshot, `dvm-<pool>--<64-digit ID>`, a tag's,
with `--t-` in its name, and a snapshot named before pools
(`dvm-<id>-<random>`) are not listed; delete them with `boxd snap rm`. A
machine named as before is listed under the name after its pool's
prefix where that is a VM name, and `rm` removes it.

## Try it

From a checkout, on any host:

```sh
go tool task build                                      # or go build -o disco-vm ./cmd/disco-vm, on any host

export BOXD_API_KEY=$(boxd auth keys create disco-vm)   # once; keep it somewhere safe
export DISCO_VM_DRIVER=boxd
export BOXD_REGION=fra                                  # or nyc: required, where the VMs live

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
committed VM a snapshot of the same name in `boxd snap ls` (`--pool`
names the pool). `rm` destroys the machine, or deletes a committed VM's
snapshot; a VM created from it keeps running. disco-vm keeps nothing on the
host, so every host with the account's key sees the same VMs. A fresh VM is
made from the driver's base, `machine.Config.Base`, or boxd's default image
when it has none (ADR 26-11-03-606): a caller never names the image at
`create`, and a snapshot the account made by hand is not one disco-vm
reads.

## Configuration

| env | default | |
|---|---|---|
| `BOXD_API_KEY` | | an API key; with none and `BOXD_IN_MACHINE`, the boxd machine's own token ([On a boxd machine](#on-a-boxd-machine)). `BOXD_TOKEN`, which the boxd CLI reads, also works, and may hold a JWT. The command line reads them (`machine.KeyFromEnv`); a program that embeds the engine gives the driver a `machine.KeySource` instead, and the driver reads neither. |
| `BOXD_GRPC_ADDR` | `boxd.sh:9443` | another cluster: the configuration's `addr` (`--boxd-addr`; ADR 26-11-11-215), always over TLS, which the command line reads from it and the driver never does. `addr` and `tokenUrl` are privileged: they are sent the key |
| `BOXD_TOKEN_URL` | `https://app.boxd.sh/api/v1/auth/token` | its key exchange: the configuration's `tokenUrl` (`--boxd-token-url`), `https`, or `http` to a loopback address |
| `BOXD_IN_MACHINE` | `false` | the configuration's `inMachine` (`--boxd-in-machine`): with no key, authenticate as the boxd machine the driver runs on ([On a boxd machine](#on-a-boxd-machine)). Privileged: it acts as the machine's owner |
| `BOXD_REGION` | none: required | the configuration's `region` (`--boxd-region`): the boxd region the driver's machines and snapshots live in, such as `fra` or `nyc` ([Regions](#regions)) |
| `HTTPS_PROXY` | | gRPC goes through it with HTTP CONNECT, as does the key exchange. A proxy that intercepts TLS has to offer HTTP/2 (ALPN `h2`) for gRPC to work through it. A discobox's egress proxy does, and the conformance suite passes through it (2026-10-08). |
| `DISCO_VM_DEBUG` | | any value is `disco-vm --debug`: one line on stderr for every API call, when it finished, how long it took, and its request and answer. A stream (Exec, an upload) is one line when it ends. |
| `ALL_PROXY` | | a `socks5://` or `socks5h://` proxy carries the gRPC connection instead, for an HTTP proxy without HTTP/2. The key exchange still goes through `HTTPS_PROXY`. |

A key is fenced to one org, so the org is the key's. There is no shim for a
boxd instance, so every command authenticates with its own environment, and a
credential only has to outlive the command it was given to. Inside a discobox,
whose `discobox-access run` hands out a sentinel that lives five minutes, that
is what makes an instance usable past its `run`. A single command longer than
that still outlives its sentinel. `ls` shows an instance boxd cannot be asked
about as `unknown`, and an engine's `Forwards` (`Engine.Forwards`) are refused:
nothing would hold the forwards' listeners. A shim an older disco-vm started for an instance is ended
the next time the instance is started, stopped or removed: it is asked for its
status with its token first, so a reused PID is never signaled, and the
machine is left as it is.

## On a boxd machine

With the configuration's `inMachine` set (`BOXD_IN_MACHINE=true`,
`--boxd-in-machine`) and no key (no `BOXD_API_KEY` or `BOXD_TOKEN` on the
command line, no `machine.KeySource` from an embedder), the driver
authenticates as the machine it runs on: it asks boxd's metadata adapter,
`GET http://<default gateway>:9002/v1/auth/token`, which needs no key and
mints a JWT of the machine's owner, fenced to its org, living 15 minutes. The
driver keeps it until a minute before its `expires_at`, and mints another
when boxd refuses it. Without `inMachine` it never asks, and no key
is "no credentials" as before. `inMachine` is privileged: an embedder lets
no one set it who could not act as the host machine's owner. It dials the adapter through an embedder's dialer, never a
proxy: the adapter knows the machine by where the request comes from. A
configured key is always used instead.

What it can do, from discobox-ai/discobox#1090's spike on boxd itself
(2026-11-13, fra and nyc, the driver on such a token: `Check`, an isolated
`Create` on a network with an egress allowlist, `Commit`, `Create` from it,
`Destroy`):

- **A private machine's token is its owner's `member` credential in the
  org.** It lists, makes, snapshots, fences and deletes every machine of the
  owner's there, other pools' included, and reads the org's env values: what
  the owner's `member` key reaches, except key management (`key issue` is
  `ErrCannotIssue`) and its own machine's egress allowlist. Nothing revokes
  it but deleting the machine, and root on the machine mints one at will.
- **A shared machine's token is `org`-scoped**, with no `Whoami`, snapshots
  or egress fence: the driver fails `Check` there, so a pool cannot run on
  one.
- **An isolated machine has no adapter**: with no key, its calls fail with
  "no credentials".

The adapter's lifetime and address are boxd's SDK's and what was seen, not a
documented contract. Its `api_url` (`http://boxd.sh:9443`) is not read: calls
go to the configuration's `addr`, over TLS.

## Keys

`disco-vm key issue --ttl D` (`Driver.IssueKey`) creates a `member` API key
named `dvm-<pool>--<name>`, fenced to the engine's key's org, expiring
after the TTL. `key ls` and `key revoke ID` read `ListApiKeys` and delete
only such a key of the caller's. What boxd's kinds reach, from its API's
documentation:

- **`member`** acts as its maker within one org: every machine of theirs
  there, not only this pool's. disco-vm reports it as scope `org`. It is
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
host running the pool's machines with the key it issued. Nor whether
boxd stops honoring the JWT an issued key was exchanged for (an hour's)
once the key is revoked or expires: until that is shown, a holder who
exchanged it may keep working for up to that hour, whatever the TTL.

## A bare machine, and networks

`create --bare` and `--network N` (`InstanceSpec.Bare`, `Networks`;
`Capabilities.Bare` strip, `Networks`) are `CreateVm`'s `isolated` and
`networks`. `isolated` strips the in-VM boxd
CLI, the git credential helper, the integrations, and the default network's
peers. It is immutable, and a restore from a snapshot of an isolated machine
is isolated (boxd's API docs). A restore takes no networks of its own, so the
driver sets them with `SetVmNetworks`, and refuses networks on a restore that
is not asked `--bare`: until they are set it would peer on the default
network. An isolated machine with networks
peers with exactly the non-isolated machines on them, which is how a machine
that holds no token reaches one that does. Against boxd (2026-10-24): `run
--isolated` (now `--bare`) of the default image made a machine `GetVm` reports `isolated`.

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
never to a network. disco-vm binds none, so the fence holds only while no
host-bound secret is in a scope its machines are in, and `Check` refuses
the key until none is (ADR 26-11-15-274). It lists the secrets with no org
(`ListVars`), which boxd resolves as it does a create, which names none
either: to the org an API key or a machine's token is fenced to ("" resolves
to it, boxd's credential fence), and for a login to its own scope. It fails
naming each host-bound one of scope `all` or `private`, or of no scope (boxd
ignores a personal secret's scope and does not say what it lists one as),
with its scope and hosts; boxd returns no value. `shared` ones reach only
shared machines, and `unattached` and `private-unattached` only a machine
whose spec lists them, which disco-vm's never do. An in-machine token is
checked the same way. Checked against the fake API only: that `ListVars`
with no org answers an API key and a machine's token with the fenced org's
secrets, and whether it lists only the caller's own `private` ones or every
member's, are not confirmed against boxd.
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

The pool URL (`url --scope pool`) is where a peer on the machine's networks
reaches its service: `http://<machine>.boxd:7302`, boxd's internal DNS name
of the machine, which resolves only on machines of the same owner, and
follows the networks rule. Every port the guest listens on is reachable
there, and nothing crosses boxd's proxy. A machine on no networks has its
ingress URL as its pool URL.

Against boxd (2026-10-29, `computer:0.1.53`): an isolated machine on network
`dvm-t25` with `--egress none` reported egress `{}`. Its resolver answered
`dvm-vtest25--dvm-hub.boxd` (a non-isolated machine on the same network) and
no internet name, and a connection to `1.1.1.1:443` by address was closed in
the TLS handshake. It reached the hub's pool URL and its port 8080, and
did after a stop and a start. Restores of its snapshot got the fence each
asked for: `none`, `example.com` (example.com answered, github.com did not
resolve), and none at all, which reached both.

## The org's machine limit

boxd caps the machines an org may have at once: `GetOrgMachineLimit`'s
`max_vms`, 50 by default and 2 for a new org without a payment method (100
for the org it was read from on 2026-11-15). It
counts every non-destroyed machine billed to the org (`vm_count`), stopped
ones included, shared and private, every member's and every pool's; a
snapshot, so a committed VM, does not count. boxd refuses a create past it,
and never a boot. So it is not `MaxRunning`, which boxd leaves unset, but
`MaxMachines[linux]` (ADR 26-11-15-274). `Check` reads it at the home
endpoint and keeps it, and `Capabilities` reports none before. boxd
documents it readable with a login or an API key issued for the org, not
with a token minted inside a machine, so a refusal (`PermissionDenied`,
`Unimplemented`) leaves no cap and passes `Check`: boxd still enforces it. boxd does not
document the code it refuses a create with, so on any refused create the
driver reads the limit again: with `vm_count` at `max_vms`, the refusal is
`ErrCapacity`, naming the org and its count, and the kept limit is the new
one. `machinetest`'s max-machines subtest fills the limit, so the run
against boxd itself skips it (`SkipMaxMachines`): it would refuse every
other member's machines meanwhile. `TestConformanceFakeAPI` runs it at a
limit of 4, counted across regions.

## Regions

boxd runs in regions, Frankfurt (`fra`, the home region) and New York
(`nyc`), and a machine or snapshot lives in the one it was made in. Each has
its own API endpoint; `ListRegions` on the home endpoint (`boxd.sh:9443`)
names them, with each one's `api_endpoint` and whether the key's org has it
`enabled`. Machine and snapshot calls go to a region's endpoint, and account
and API key calls, and the key exchange, to the home endpoint.

A driver is given its region in its configuration, the field `region`
(`--boxd-region`, `$BOXD_REGION`; ADR 26-11-11-215), and it is required: a
configuration with none is refused. It does not follow boxd's own default,
the org's default region set in the console, which `boxd new` uses: that
default can change under a pool, which would then lose sight of the VMs it
made in the old one while they are still billed, and not every credential
may read it, so two drivers of one pool could land in two regions. A pool
made before the region was required, with none, has its VMs at the home
endpoint: `fra`. The driver asks `ListRegions` once, the first time it needs
the region, and sends every machine and snapshot call to that region's
endpoint (the home endpoint itself, for the home region); a region boxd does
not list, or one the org has not enabled, fails `Check` and every call,
naming why, and is asked again next time. Since a region's
endpoint lists only that region's machines and snapshots, a pool sees its
region's VMs alone: the same pool in two regions is two pools, and a name
can be used in both, as boxd's names can. Keys are the account's, so a key
issued from any region reaches the org's machines in every one.

A machine's ingress URL is its default proxy's domain, which boxd reports in
the machine's region's zone (`<machine>.nyc.boxd.sh`). A machine's pool URL,
`<machine>.boxd`, is reached from its region alone, as boxd's networks are.

## The ingress URL

Every boxd machine has a default proxy at `<machine>.boxd.sh`, which
terminates TLS with a managed certificate and forwards HTTP, websockets
included, to one port of the machine. Left alone, it forwards to the first of
80, 443, 8080, 8000, 3000, 5000, and 5173 the guest listens on, else 8000.
Unless an instance's ingress URL is asked off, Create pins it to 7302, the port every
instance's service listens on (`tcp:0.0.0.0:7302` in the guest's service
file), so the service is the only thing it reaches, and turns bot protection
off. For one asked off, Create closes it. boxd never deletes a default proxy
(`DeleteProxy` answers `FailedPrecondition` for it, by its name `@`, and
`NotFound` for the empty name `ListProxies` gives it), so Create locks it to
port 0 with `SetProxyPort`: no guest can listen on TCP port 0, and boxd
answers 502 there, across stop and start. Create removes any other proxy the
machine has with `DeleteProxy`, and reads back that `ListProxies` lists the
default one alone, locked to 0; a refusal, or a proxy still open, destroys the
machine and fails the create. Once that read-back holds, and not before,
Create turns bot protection off on the closed proxy too: port 0 is what
keeps the guest unreached, and boxd's browser
challenge (`503`, `x-boxd-proxy: challenge`) would answer in place of the
closed proxy's `502`, hiding that it is closed. Such an instance has no ingress URL (ADR
26-11-12-604, ADR 26-11-14-610, ADR 26-11-14-454). The
service port belongs to the guest's root, not the service, so the proxy is
an inbound path from the
internet that the egress allowlist never sees; a caller that must not have
one asks for it off, strictly. That a closed default proxy stays closed, and answers for no
port the guest listens on, across boots, is `TestNoIngressURL`'s to confirm
against boxd. The URL is recorded with the
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

- The org's machine limit and its secrets, read-only with an API key
  (2026-11-15, `disco-vm info`, nothing created): `GetOrgMachineLimit`
  with no org answered the key's org by name with `max_vms` 100 and no
  machines counted, reported as `MaxMachines[linux]` 100; `ListVars` with
  no org and kind `secret` answered, empty for an org that holds no
  secrets, and `Check` passed.

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

- Regions on boxd itself: that `ListRegions` names `nyc`'s endpoint in a
  form the driver dials (a host and port, or a URL), that a machine made
  there lists from it and not from `fra`, and that its proxy's domain is in
  `nyc`'s zone. It passes against the fake only, which serves each region on
  an endpoint of its own.
- `ResizeVm` on a stopped machine, which a boot at a size other than the
  snapshot's uses. It passes against the fake only.
- Committed VMs (ADR 26-11-03-606) on boxd itself: a snapshot named as its
  machine is, taken while the machine still exists. It passes against the
  fake only.
- The org's machine limit and its secrets with a machine's token (boxd
  does not document `GetOrgMachineLimit` for one, so a refusal is taken as
  no cap); that `ListVars` with no org lists the fenced org's secrets, and
  how it lists a secret set with no scope (counted as reaching, and the
  error says to set one explicitly; if boxd lists it with none rather than
  `shared`, a shared one is refused until it is), which take an org that
  holds secrets; and the code boxd refuses a create past the limit with.
  They pass against the fake only.
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

- No shared directories, and no display. boxd's `DesktopTunnel` is an xpra
  stream, which could back `Display` later.

