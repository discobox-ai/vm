# boxd driver (any host, Linux guest in boxd's cloud)

**Scope:** `pkg/machine/boxd`. Nothing of disco-vm's runs in a boxd guest: its
operations (`machine.Guest`) are command lines over boxd's Exec stream (ADR
0001), and a machine's power is boxd's API alone (ADR 26-10-15-833).

**Done means:**

1. `machinetest.Run` passes against boxd itself: `TestConformance` with
   `BOXD_API_KEY` set.
2. `internal/e2e` passes with `DISCO_VM_DRIVER=boxd`.
3. `docs/examples/boxd.yaml` builds.

Status, 2026-10-07, with an agent in the guest: criteria 1 and 3 held. On
2026-10-15, with no agent (ADR 0001), criterion 3 held again, and the CLI ran
the image through the same commands, `exec -t` included; `TestConformance`
has not been run against boxd since, because a discobox's five-minute
credential does not outlive it. `TestConformance` passes against
boxd. `docs/examples/boxd.yaml` builds, and the CLI runs it through `run`,
`exec`, `cp`, `stop`, `start`, `rm`, and `rmi` with nothing left on boxd.
Criterion 2 waits on `internal/e2e`, which is written for the fake driver.
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
| `Install` | `CreateVm` (restart policy `never`, idle timers off), wait for the guest, `sync`, snapshot. Media must be `latest`; `options.image` picks a boxd image. No agent: `InstallSpec.Agent` is not used, and `Capabilities.Agent` is off, so `build` asks for no `--agent`. |
| layer | `layer.json`: the snapshot's name, ID, version, and size. `DeleteLayer` deletes the snapshot. |
| `LayerExists` | `GetSnapshot` on the layer's snapshot: gone, or `failed`, is a layer the build replaces, along with every layer after it. One call for each cached layer of a build. |
| `TagLayer`, `UntagLayer` | boxd cannot name or label a snapshot after the fact, and snapshots only a running machine. A tag restores the layer's snapshot into a machine, saves that as `dvm-<project>--<layer id>-tag-<ref>-<hash>`, and destroys the machine; untagging deletes the snapshot. Nothing restores from it: it is there so `boxd snap ls` says what each layer is. Each tag costs a restore and a whole snapshot. A tag made before this existed has none; tag it again to make one. |
| `Prepare` | restore the layer's snapshot, `sync` in the guest, `StopVm`, and wait for `stopped`. The first boot is a cold boot of the layer's disk. A guest that cannot be asked to `sync` is stopped anyway, and Prepare fails. |
| `Prepare` of a base | nothing but the instance's directory: the first `Boot` makes the machine. |
| `Boot` | for an instance of a base with no machine yet, make it: `CreateVm` with the base's image (restart policy `never`) or `CreateVmFromSnapshot` with its snapshot, at the boot's size (a snapshot keeps its own when none is asked for), idle timers off, the proxy pinned as `Prepare` pins it. Otherwise `StartVm` (after `ResizeVm` when the boot asks for another size), `ResumeVm`, or `WakeVm`, by state. A machine still `stopping` is waited out first. Returns once the guest answers: an Exec of `sudo -n systemctl is-system-running --wait` returns, once a second, for up to 5 minutes. A machine that stops meanwhile (`GetVm` every 2s) ends the wait. |
| `Attach` | (`Capabilities.Remote`: the engine runs no shim for boxd.) `GetVm`: `running` or `starting` is the machine; not found, `stopped`, any other state, or no machine yet (a base's instance never started) is `ErrNotRunning`. |
| `Machine.Done` | watching starts on the first `Done`, `Err`, or `Kill`, so a machine attached only to run a command polls nothing. `GetVm` every 2s: `stopped` is a power-off, and `failed`, `destroying`, or not-found is an error. |
| `Machine.Kill` | `StopVm`, retried until boxd takes it, then wait for `stopped`. Callers pass no deadline, so Kill has its own (5 minutes). Past it, Kill gives the machine up: `Done` closes, and `Err` says it may still be running. Its instance still names it, so `rm` destroys it later. |
| `Machine.Exec` | one Exec stream: `sudo -n -H -u USER -- env -C DIR -- K=V... ARGV...`, every word single-quoted (a program whose path has an `=` in it would be read by `env` as a variable). The user is root unless the request names one, and the directory `/` unless it names one. sudo gives the account's HOME, USER, LOGNAME, and `secure_path`; the request's variables are added to them. Stdin chunks, a half-close for EOF, stdout and stderr apart, and the exit code in the last chunk. A TTY sets `tty` with `rows` and `cols`, and a resize is a chunk with `window_change`. Closing the process cancels the call, which kills the command. |
| `Machine.CopyTo` | Exec of `tar -x --no-same-owner --no-overwrite-dir` as root into the directory, made first, with the tar as stdin: what it unpacks is root's, and a directory already there keeps its mode, as with an agent. |
| `Machine.CopyFrom` | Exec of a script that fails on a missing path, else writes one ready byte, then `tar -c` of the path rooted at its base name. |
| `Machine.Info` | Exec of `uname -m; hostname`: linux, the GOARCH of the machine, its hostname. No version: there is no agent. |
| `Machine.Shutdown` | a power-off is an Exec of `sync` as root, then `StopVm`; the watcher sees `stopped`. A reboot is `RebootVm`. Nothing in the guest is asked to power off. |
| `Commit` | the guest is stopped after an orderly shutdown. Start it, wait for the guest, and snapshot. |
| `Destroy` | `DestroyVm`. |
| `Orphans` | `ListVms` and `ListSnapshots` in the caller's context, keeping the names of the project (`dvm-<project>--`) that no instance's `vm.json`, layer's `layer.json`, or tag names. A snapshot a warm stage took at a size of its own, from before warm stages were removed, is one of them. Two calls on every `ls` and `images`. |
| `Bases` | `GetConfig`'s `default_image` (named and ID'd by the image), then every `ready` snapshot of `ListSnapshots` whose name does not start with `dvm-`, oldest first (named by its name, ID'd by its ID, versioned by its `version`, made from by its ID). Two calls on every `images`, and on a ref the store does not have. |
| `RemoveOrphan` | `DestroyVm` a machine, `DeleteSnapshot` a snapshot (`disco-vm rm` / `rmi` with an orphan's name). |
| `Prepare` | once the machine is restored (a base's instance: once its first `Boot` makes it), `SetBotProtection` off, `SetProxyPort` pins the default proxy (the one `ListProxies` marks `is_default`) to 7302, and its domain is recorded in `vm.json`. |
| `Service` | listen on `tcp:0.0.0.0:7302`; the URL is `https://<machine>.boxd.sh` from `vm.json`, with no API call. A base's instance that has never started has none: `ErrNotRunning`. |

Exec runs as boxd's `boxd` user, who has passwordless sudo, so every guest
operation goes through `sudo -n`, as root by default. An image installed
before the driver stopped carrying an agent still has
`disco-vm-guest.service`, and one installed before ADR 26-10-15-833 a
shutdown hook in `/usr/lib/systemd/system-shutdown/disco-vm`; nothing reads
either, and they do no harm. A rebuild keeps them, because the install
layer's cache key does not change; one with `--no-cache` leaves them out.

Machine names are `dvm-<project>--<instance id>-<random>`, at most 59
characters so that a name is a DNS label in its proxy's domain, and snapshot
names are `dvm-<project>--<layer id>-<random>`. A `--no-cache` rebuild or a
failed commit never reuses a name. A tag's snapshot is
`dvm-<project>--<layer id>-tag-<ref>-<hash>`, which sorts beside its layer's:
the ref with its punctuation as `-`, cut to keep the name within 66
characters, and four hex digits of the ref's SHA-256 to tell apart refs that
then read the same. A name from before projects (`dvm-<id>-<random>`, no `--`)
is the default project's. A tag snapshot made before projects is named
otherwise, so `images` lists it as an orphan; `rmi` with its name deletes it.

## Try it

From a checkout, on any host:

```sh
go tool task build                                      # or go build -o disco-vm ./cmd/disco-vm, on any host

export BOXD_API_KEY=$(boxd auth keys create disco-vm)   # once; keep it somewhere safe
export DISCO_VM_DRIVER=boxd

./disco-vm info                                         # check: ok means the key works
./disco-vm images                                       # boxd's default image, and your own snapshots
./disco-vm run --name stock <default image's name>      # no build: the machine is made from the image
./disco-vm rm -f stock
./disco-vm build -f docs/examples/boxd.yaml -t demo/boxd
./disco-vm images
./disco-vm run --name dev demo/boxd
./disco-vm exec dev cat /etc/motd.d/disco-vm
./disco-vm exec -it dev bash
./disco-vm cp ./README.md dev:/tmp
./disco-vm url dev                                      # https://<machine>.boxd.sh
./disco-vm stop dev && ./disco-vm start dev
./disco-vm rm -f dev
./disco-vm rmi demo/boxd
```

Each instance is a machine named `dvm-default--...` in `boxd ls`, and each
layer is a snapshot named `dvm-default--...` in `boxd snap ls` (`--project`
names the project). `rm` destroys the machine, and `rmi` deletes the
snapshots, a tag's with the tag. disco-vm's own state (the image store,
instances) is local, under `DISCO_VM_ROOT`, so use one host for one set of
images. Whatever of the project's is in boxd and not in that state, a store
deleted or another host's, is listed by `ls` and `images` as `<orphan>`.
boxd's default image, and any snapshot of the account not named `dvm-`, is a
base (ADR 26-10-15-833): `images` lists it by its name, and `run` or
`create` with that name or its ID makes a machine from it, with no build and
no layer. A spec's `from.image` builds on it, again when a snapshot is
captured anew; boxd gives an image no version, so a build on the default
image is cached while its name stays the same. `snapshot` of such an
instance makes an image. `tag` and `rmi` refuse a base, which is
boxd's and never deleted.

## Configuration

| env | default | |
|---|---|---|
| `BOXD_API_KEY` | | an API key. `BOXD_TOKEN`, which the boxd CLI reads, also works, and may hold a JWT. |
| `BOXD_GRPC_ADDR` | `boxd.sh:9443` | another cluster |
| `BOXD_TOKEN_URL` | `https://app.boxd.sh/api/v1/auth/token` | its key exchange |
| `HTTPS_PROXY` | | gRPC goes through it with HTTP CONNECT, as does the key exchange. A proxy that intercepts TLS has to offer HTTP/2 (ALPN `h2`) for gRPC to work through it. A discobox's egress proxy does, and the conformance suite passes through it (2026-10-08). |
| `DISCO_VM_DEBUG` | | any value is `disco-vm --debug`: one line on stderr for every API call, when it finished, how long it took, and its request and answer. A stream (Exec, an upload) is one line when it ends. |
| `ALL_PROXY` | | a `socks5://` or `socks5h://` proxy carries the gRPC connection instead, for an HTTP proxy without HTTP/2. The key exchange still goes through `HTTPS_PROXY`. |

A key is fenced to one org, so the org is the key's. There is no shim for a
boxd instance, so every command authenticates with its own environment, and a
credential only has to outlive the command it was given to. Inside a discobox,
whose `discobox-access run` hands out a sentinel that lives five minutes, that
is what makes an instance usable past its `run`. A single build longer than
that still outlives its sentinel. `ls` shows an instance boxd cannot be asked
about as `unknown`, and an engine `CreateOptions.Forwards` is refused:
nothing would hold the forwards' listeners. A shim an older disco-vm started for an instance is ended
the next time the instance is started, stopped or removed: it is asked for its
status with its token first, so a reused PID is never signaled, and the
machine is left as it is.

## The URL

Every boxd machine has a default proxy at `<machine>.boxd.sh`, which
terminates TLS with a managed certificate and forwards HTTP, websockets
included, to one port of the machine. Left alone, it forwards to the first of
80, 443, 8080, 8000, 3000, 5000, and 5173 the guest listens on, else 8000.
Prepare pins it to 7302, the port every instance's service listens on
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
  the shutdown takes the exec path down, so an orderly stop and a build's
  commits go through. Exec's `rows` and `cols` (fields 8 and 7) size the pty.
  GNU `env` ends its options at the first `NAME=VALUE`, so its `--` comes
  before the variables. The first build of `docs/examples/boxd.yaml` with no
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
  layer's uses. It passes against the fake only.
- The `suspended`/`standby` status strings, and `WakeVm` from `hibernated`.
  The driver turns off auto-suspend and auto-hibernate on every machine, so
  these show up only when something else suspends one.
- A guest reboot (`Shutdown(reboot=true)`), which the driver turns into a stop
  and a start. Nothing in disco-vm sends one today.
- That cancelling an Exec kills what sudo started in the guest, and a TTY
  resize (`window_change`), which the CLI does not send yet.
- Load. Each guest operation is one Exec call, waiting for a guest makes one
  a second, and a watched machine (a build's, or one being stopped) makes one
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
