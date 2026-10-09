---
name: verify-recipes
description: How to verify a disco-vm change at runtime — the CLI against real boxd (run, exec, cp, stop, start, ps, rm, build), including from inside a discobox whose boxd credential is a five-minute sentinel. Read before verifying anything that touches the engine, the CLI, or the boxd driver.
---

# Verifying disco-vm

The surface is the `disco-vm` CLI. Drive it against a real driver; the fake
driver is CI's (`go tool task test`), not evidence that a guest behaves.

## Build

```bash
go tool task build:linux-agent     # build/linux-amd64/disco-vm, static
```

Use this, not `go tool task build`, for boxd: install uploads the CLI binary
itself as the guest's agent, and the devShell's default build links a
`/nix/store` glibc that a boxd guest lacks. Install checks only the
architecture, so a wrong binary shows up as a five-minute wait for an agent
that never starts.

Keep the file named `disco-vm`. Inside a discobox, the credential judge reads
the command literally, and a binary called anything else is refused.

To test against the behavior before a change (an instance an older binary
started, say), build the base commit too: `git archive <ref> | tar -x` into a
directory of its own, then, since an older tree may not have the target,

```bash
CGO_ENABLED=0 GOOS=linux GOARCH=amd64 go build -o <its own dir>/disco-vm ./cmd/disco-vm
```

## boxd

- Credentials: `BOXD_API_KEY` outside a discobox. Inside one, ask
  `discobox-access` for `BOXD_TOKEN` (host `boxd.sh`) with a use naming
  disco-vm and boxd broadly, and run every command as
  `discobox-access run --use <id> -- disco-vm ...`. Each run gets a sentinel
  that lives five minutes.
- Isolate state: `export DISCO_VM_ROOT=<a scratch directory>/dvm
  DISCO_VM_DRIVER=boxd`. The image index lives there; the layers are boxd
  snapshots.
- A base image: `docs/examples/boxd.yaml`, or any spec with
  `from: {install: {os: linux, media: latest}}`. Install takes about a minute.
  A layer costs a shutdown, a cold boot, and a snapshot (~40s on top of its
  steps).

## Flows worth driving

- `run`, then `exec` and `cp` **more than five minutes later** (`sleep 330`):
  that is what proves nothing holds a stale credential.
- `stop`, `ps -a` (stopped), `start`, `exec`.
- A guest's own `systemctl poweroff` via `exec`, then `ps -a` (stopped), then
  `start` at once (boxd may still say `stopping`).
- Probes: `start` a running instance, `run --forward` on boxd (refused),
  `exec`/`stop` a stopped one, `rm -f` twice, two `exec`s at once.
- An instance started by the old binary: read its `instances/<id>/shim.json`,
  `ps -p <pid>`, then act on it with the new binary.

## Gotchas

- Inside a discobox the Bash tool is zsh: `$VAR:refs` loses its colon, and an
  unquoted `$CMD` holding spaces is one word. Use `${VAR}` and functions.
- `pgrep -f '<pattern>'` matches the wrapper shell whose command line contains
  the pattern. Check a PID with `ps -p` instead.
- A build longer than five minutes can outlive its sentinel, and the builder's
  boot wait has no deadline: it hangs rather than fails. Run long builds with a
  watchdog that kills a build whose log has been quiet for six minutes, and
  re-run: cached layers make the retry cheap.
- Clean up: `ps -a` should be empty at the end. A boxd machine left running is
  billed.
