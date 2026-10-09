# boxd driver (any host, Linux guest in boxd's cloud)

**Scope:** `pkg/machine/boxd`, plus two OS-neutral pieces of `pkg/guest`: the
`unix:` listen address and `guest.Pipe` (`disco-vm pipe`). Nothing above the
`machine.Driver` seam changed.

**Done means:**

1. `machinetest.Run` passes against boxd itself: `TestConformance` with
   `BOXD_API_KEY` set.
2. `internal/e2e` passes with `DISCO_VM_DRIVER=boxd`.
3. `docs/examples/boxd.yaml` builds.

Status, 2026-10-07: criteria 1 and 3 hold. `TestConformance` passes against
boxd. `docs/examples/boxd.yaml` builds, and the CLI runs it through `run`, `exec`,
`cp`, `stop`, `start`, `warm`, a resumed `run`, `rm`, and `rmi` with nothing
left on boxd. Criterion 2 waits on `internal/e2e`, which is written for the
fake driver. `TestConformanceFakeAPI` runs the same suite in CI against an
in-process fake of boxd's API (`fakeapi_test.go`), whose "machines" are agent
processes over directory roots.

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
| `Install` | `CreateVm` (restart policy `never`, idle timers off), check `uname -m` against the agent's ELF, upload the agent, install `disco-vm-guest.service` and the shutdown hook with sudo, wait for the agent, snapshot. Media must be `latest`; `options.image` picks a boxd image. |
| layer | `layer.json`: the snapshot's name, ID, version, and size. `DeleteLayer` deletes the snapshot. |
| `Prepare` cold | restore the layer's snapshot, power the guest off through the agent, and `StopVm` once the shutdown hook says it halted. The first boot is a cold boot of the layer's disk. A guest that cannot be shut down in order is stopped anyway, and Prepare fails. |
| `Prepare` resume | restore the stage's snapshot and suspend it. A missing stage or snapshot is `ErrNotWarm`. |
| `Boot` | `StartVm` (after `ResizeVm` when the boot asks for another size), `ResumeVm`, or `WakeVm`, by state. A machine still `stopping` is waited out first. |
| `Attach` | (`Capabilities.Remote`: the engine runs no shim for boxd.) `GetVm`: not found, `stopped`, or any state but `running` or `starting` is `ErrNotRunning`. While it is `running`, an Exec reads the shutdown hook's marker: a guest that halted while nothing watched is stopped with `StopVm` and reads as not running, and one that rebooted is started again. |
| `Machine.Done` | watching starts on the first `Done`, `Err`, or `Kill`, so a machine attached only to dial polls nothing. `GetVm` every 2s: `stopped` is a power-off, and `failed`, `destroying`, or not-found is an error. While it is `running`, an Exec reads the shutdown hook's marker, and a guest that has halted is stopped with `StopVm` (and started again if it rebooted). |
| `Machine.Kill` | `StopVm`, retried until boxd takes it, then wait for `stopped`. Callers pass no deadline, so Kill has its own (5 minutes). Past it, Kill gives the machine up: `Done` closes, and `Err` says it may still be running. Its instance still names it, so `rm` destroys it later. A guest reboot that the watcher is turning into a stop and start never leaves a machine started after Kill. Kill waits for a start already under way, which is bounded to 30s, and then stops the machine. |
| `Machine.Dial(port)` | Exec `sudo -n /usr/local/libexec/disco-vm pipe ADDR`: the agent's unix socket for port 7300, `tcp:127.0.0.1:PORT` otherwise. |
| `Commit` | the guest is stopped after an orderly shutdown. Start it, wait for the agent, and snapshot. |
| `Destroy` | `DestroyVm`. |
| `Warm` | at the layer's size, point `stage.json` at the layer's snapshot. At another size, boot a cold clone at that size and snapshot it (`own`, deleted by `Cool`). |
| `Warmth` | `{resume, -1}` while the stage's snapshot is ready. |
| `Prepare` with a service | once the machine is restored, `SetBotProtection` off, `SetProxyPort` pins the default proxy (the one `ListProxies` marks `is_default`) to the service port, and its domain is recorded in `vm.json`. |
| `Endpoint` | `https://<machine>.boxd.sh` from `vm.json`, with no API call. |

The agent runs as root under systemd and listens on
`unix:/run/disco-vm/agent.sock` (mode 0600). Exec runs as boxd's `boxd` user,
who has passwordless sudo, so the relay reaches the socket through
`sudo -n`.

Machine names are `dvm-<instance id>-<random>`, and snapshot names are
`dvm-<layer id>-<random>`. A `--no-cache` rebuild or a failed commit never
reuses a name.

## Try it

From a checkout, on any host:

```sh
go build -o disco-vm ./cmd/disco-vm
# The guest agent is disco-vm for linux/amd64. On a linux/amd64 host the
# binary you just built is it; anywhere else, build one and pass --agent.
GOOS=linux GOARCH=amd64 CGO_ENABLED=0 go build -o disco-vm-linux ./cmd/disco-vm

export BOXD_API_KEY=$(boxd auth keys create disco-vm)   # once; keep it somewhere safe
export DISCO_VM_DRIVER=boxd

./disco-vm info                                         # check: ok means the key works
./disco-vm build -f docs/examples/boxd.yaml -t demo/boxd     # add --agent ./disco-vm-linux off linux/amd64
./disco-vm images
./disco-vm run --name dev demo/boxd
./disco-vm exec dev cat /etc/motd.d/disco-vm
./disco-vm exec -it dev bash
./disco-vm cp ./README.md dev:/tmp
./disco-vm endpoint dev                                 # an image with `service: {port: N}` only
./disco-vm stop dev && ./disco-vm start dev
./disco-vm warm demo/boxd && ./disco-vm run --name fast demo/boxd   # resumes from the snapshot
./disco-vm rm -f dev fast
./disco-vm warm --rm demo/boxd && ./disco-vm rmi demo/boxd
```

Each instance is a machine named `dvm-...` in `boxd ls`, and each layer is a
snapshot named `dvm-...` in `boxd snap ls`. `rm` destroys the machine, and
`rmi` deletes the snapshots. disco-vm's own state (the image store, instances)
is local, under `DISCO_VM_ROOT`, so use one host for one set of images.

## Configuration

| env | default | |
|---|---|---|
| `BOXD_API_KEY` | | an API key. `BOXD_TOKEN`, which the boxd CLI reads, also works, and may hold a JWT. |
| `BOXD_GRPC_ADDR` | `boxd.sh:9443` | another cluster |
| `BOXD_TOKEN_URL` | `https://app.boxd.sh/api/v1/auth/token` | its key exchange |
| `HTTPS_PROXY` | | gRPC goes through it with HTTP CONNECT, as does the key exchange. A proxy that intercepts TLS has to offer HTTP/2 (ALPN `h2`) for gRPC to work through it. A discobox's egress proxy does, and the conformance suite passes through it (2026-10-08). |
| `ALL_PROXY` | | a `socks5://` or `socks5h://` proxy carries the gRPC connection instead, for an HTTP proxy without HTTP/2. The key exchange still goes through `HTTPS_PROXY`. |

A key is fenced to one org, so the org is the key's. There is no shim for a
boxd instance, so every command authenticates with its own environment, and a
credential only has to outlive the command it was given to. Inside a discobox,
whose `discobox-access run` hands out a sentinel that lives five minutes, that
is what makes an instance usable past its `run`. A single build longer than
that still outlives its sentinel. `ps` shows an instance boxd cannot be asked
about as `unknown`, and `run --forward` is refused: nothing would hold the
forwards' listeners. A shim an older disco-vm started for an instance is ended
the next time the instance is started, stopped or removed: it is asked for its
status with its token first, so a reused PID is never signaled, and the
machine is left as it is.

## The endpoint

Every boxd machine has a default proxy at `<machine>.boxd.sh`, which
terminates TLS with a managed certificate and forwards HTTP, websockets
included, to one port of the machine. Left alone, it forwards to the first of
80, 443, 8080, 8000, 3000, 5000, and 5173 the guest listens on, else 8000. For
an instance whose image declares `service: {port: N}`, Prepare pins it to N,
so the service is the only thing it reaches, and turns bot protection off.
The URL is recorded with the instance, so `disco-vm endpoint` and
`Engine.Endpoint` answer without a credential, and it stays the same across
stop and start. Anyone who has it reaches the service, which authenticates
its callers itself (discobox ADR 26-10-09-143 §3).

A machine with no service keeps the default, so a guest server on one of
those ports is public at `<machine>.boxd.sh` all the same.

## A guest cannot power its machine off

Found against boxd on 2026-10-07. `systemctl poweroff` in a boxd guest stops
every unit and runs `systemd-shutdown`, which syncs and asks the kernel to
power off. The machine never stops: boxd keeps reporting `running`, with
nothing left in it but boxd's own exec path. A machine restored from a snapshot
also comes back with `restart_policy: always`, whatever its source had, and a
restore cannot set one.

So the agent's orderly shutdown ends in a halted guest that only the API can
stop. Install drops a hook into `/usr/lib/systemd/system-shutdown/`, which
systemd runs once disks are synced, just before its final power-off call. The
hook writes `poweroff`, `halt`, or `reboot` to `/run/disco-vm/halted`. The
driver reads that marker through Exec (in `Machine.Done`'s watcher, and in a
cold Prepare), then calls `StopVm`, and for a reboot `StartVm` as well. Verified
on boxd: the marker survives the halt, `StopVm` leaves the machine `stopped`
despite `restart_policy: always`, and `StartVm` then cold boots the disk, with
the agent up in about 10 seconds.

The same run found the agent ignoring SIGTERM, so systemd waited out its
90-second stop timeout on every shutdown. The CLI turns SIGTERM into a
cancelled context, and `disco-vm guest` now closes its listener on it.

## Confirmed against boxd

- The endpoint (2026-10-09, `TestEndpoint`): `SetProxyPort` pins a restored
  machine's default proxy to the service port, a guest's HTTP server listening
  on all interfaces answers at `https://<machine>.boxd.sh` within a few
  retries, and after a stop and a start the proxy is still locked to the port
  and answers again. The default proxy is not named `default`: `SetProxyPort`
  with that name is NotFound, so the driver uses the name `ListProxies` gives
  the proxy it marks `is_default`.
- Exec carries the agent's HTTP, tar streams, and exit codes through
  `disco-vm pipe` unmodified, without a TTY. A half-close reaches the command
  as EOF. `command` runs through a shell as the `boxd` user, who has
  passwordless sudo.
- Exec runs in its own process namespace: `ps` there shows only the exec'd
  command, and systemd is reachable only through `sudo systemctl`.
- `GetVm` reports `running` and `stopped`, and `CreateSnapshot` reaches `ready`
  through `GetSnapshot` within seconds.
- A restore gets its own name and hostname (`dvm-<id>-<rand>`), even resumed
  from memory.
- Timings for a 2 vCPU/8 GiB machine: a cold `run` takes 15s, which includes
  the restore and power-off in Prepare. `stop` takes 4s and `start` 3s. `warm`
  at the layer's size takes 1s, and a resumed `run` 4s.

## Not verified yet

- `ResizeVm` on a stopped machine, which a boot at a size other than the
  layer's uses, and `Warm` at another size. Both pass against the fake only.
- The `suspended`/`standby` status strings, and `WakeVm` from `hibernated`.
  The driver turns off auto-suspend and auto-hibernate on every machine, so
  these show up only when something else suspends one.
- A guest reboot (`Shutdown(reboot=true)`), which the driver turns into a stop
  and a start. Nothing in disco-vm sends one today.
- `exec -t`, the TTY path, through the relay.
- Load. Each Dial is a new Exec call, `WaitReady` dials every 250ms, and a
  watched machine (a build's, or one being stopped) makes one Exec and one
  `GetVm` call every 2s. Every engine call on an instance attaches first: one
  `GetVm` and one Exec for the shutdown marker.

## A machine given up

When Kill gives a machine up (`stop` or `rm -f` whose orderly shutdown
failed, and boxd then would not stop it within five minutes), the command
fails with an error that says the machine may still be running, and billing.
`ps` asks boxd, so it shows the machine as it is. A later `stop` or `rm -f`
tries again.

## Not done

- `internal/e2e` hard-codes `DISCO_VM_DRIVER=fake`, so criterion 2 needs it to
  take the driver from the environment first.
- No shared directories, and no display. boxd's `DesktopTunnel` is an xpra
  stream, which could back `Display` later.
- `MaxRunning` is unset. The org's machine quota (50 by default, 2 for a new
  org without a payment method) is not reported as a capability.
