---
name: verify-recipes
description: How to verify a disco-vm change at runtime — the CLI against real boxd or a real Docker daemon (run, exec, cp, stop, start, ps, rm, snapshot, rmi), including from inside a discobox whose boxd credential is a five-minute sentinel. Read before verifying anything that touches the engine, the CLI, or the boxd or docker driver.
---

# Verifying disco-vm

The surface is the `disco-vm` CLI. Drive it against a real driver; the fake
driver is CI's (`go tool task test`), not evidence that a guest behaves.

## Build

```bash
go tool task build:linux-agent     # build/linux-amd64/disco-vm, static
```

boxd puts nothing of disco-vm's in its guests, so any build drives it. Docker
needs this static one (below). `go tool task build` and `go tool task
install` are static too, off macOS.

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
  that lives five minutes. Spell commands as the use names them: `ps`, not
  its alias `ls`, and no `--debug`, or the judge may refuse the run.
  `unset BOXD_API_KEY` first: a box may carry a stale one, and the driver
  reads it before `BOXD_TOKEN`, so every call is a 401. A use that says it
  makes only `dvm-*` machines gets `--name` refused unless the instance's
  name starts with `dvm-` too (`run --name dvm-dev`), although the machine
  is named `dvm-<project>--...` either way.
- Isolate state: `export DISCO_VM_PROJECT=<a scratch project, e.g. vtest>
  DISCO_VM_DRIVER=boxd`. boxd keeps
  everything (ADR 26-10-23-417): images and tags are snapshots, instances are
  machines, all named `dvm-<project>--...`, so the project is what keeps a
  run apart.
- Start from a base: `images` lists boxd's default image and the account's
  own snapshots, and `run --name dvm-dev <that name>` takes one (about 6s).
  An image is `stop` then `snapshot dvm-dev NAME`: a cold boot and a
  snapshot (~40s).

## docker

- Needs a rootful Docker daemon on cgroup v2; a discobox has one. Build with
  `go tool task build:linux-agent`, static, as for boxd: creating an
  instance of a registry image copies the CLI binary into it as the guest's
  agent and the container's init, where it runs on the image's libc. A plain
  `go build` links glibc dynamically (the devShell's from `/nix/store`), and
  it fails at create or boot on an image without that libc. Off linux,
  `DISCO_VM_AGENT` names the linux build.
- Isolate state: `export DISCO_VM_PROJECT=<a scratch project>
  DISCO_VM_DRIVER=docker`. The daemon
  keeps everything: images are Docker images named
  `disco-vm-layer:<project>--<id>` (and an instance's readied base
  `...--base-<name>--<random>`), tags `disco-vm/<project>/<ref>`, and
  instances containers named `dvm-<project>--<name>`, labeled
  `ai.discobox.vm.*`.
- Start from any registry image: `run --name dev debian:trixie`. Create
  takes 30s to 2 minutes (apt installs systemd); a snapshot costs a shutdown
  and a `docker commit`.
- `internal/e2e` against docker takes the static binary too:
  `DISCO_VM_TEST_BINARY=$PWD/build/linux-amd64/disco-vm DISCO_VM_DRIVER=docker
  go test ./internal/e2e` (about 3 minutes). Its own `go build` is dynamic,
  and readying debian fails with `exec: /usr/local/libexec/disco-vm: not
  found`.
- Inside a discobox, `exec` of anything that uses the network needs the
  proxy, because the guest's systemd starts services with a clean
  environment: `exec -e http_proxy=http://172.17.0.1:17008 -e
  https_proxy=http://172.17.0.1:17008 dev ...`. Readying the image needs
  nothing: the box's runc injects its CA and proxy into every container.
  Fedora is the exception, where that injection fails and `dnf` hangs.
- Worth driving beyond the common flows: `exec dev systemctl
  is-system-running` (`running`), `ps -eo uid,comm` on the host (the guest's
  processes at uid 16777216 and up), `cat /etc/machine-id` in two instances
  (different), a guest's own `systemctl reboot` (back, still `running`), and
  `docker kill` or `docker rm -f` on the container behind disco-vm's back,
  then `ls -a`, `start`, `rm -f`. No `disco-vm shim` process should exist.
- Clean up: `ls -a` and `images` list nothing, and no `dvm-*` containers or
  `disco-vm-layer` images are left (`docker ps -a`, `docker images`). `rmi`
  untags an image an instance runs and keeps it; `rmi` an untagged leftover
  by its ID.

## The Linux guest image, without a Mac

vz needs a Mac, but its Linux base (`pkg/guest/linux`) boots under QEMU/KVM
on a Linux box with `/dev/kvm` and `/dev/vhost-vsock` (a discobox has both),
the way vz boots it:

- `go tool task build:linux-guest` (amd64 here) and `go tool task
  build:linux-agent`; QEMU from `nix build --print-out-paths nixpkgs#qemu_kvm`.
- The boot initrd and command line are `writeBootInitrd` and `linuxCmdline`
  (`pkg/machine/vz/linuxguest.go`), which are unexported: call them from a
  throwaway `_test.go` in the package that writes the files, and delete it.
- Copy `root.ext4` to a scratch `disk.img` (`cp --sparse=always`, then
  `truncate -s 8G`), and boot: `qemu-system-x86_64 -enable-kvm -cpu host -m 2048
  -kernel vmlinux -initrd boot-initrd.img -append "<cmdline>" -drive
  file=disk.img,format=raw,if=virtio -device vhost-vsock-pci,guest-cid=42
  -nic none` (or `-nic user,model=virtio-net-pci`) `-device virtio-serial-pci
  -chardev file,id=c0,path=console.log -device virtconsole,chardev=c0
  -display none -daemonize`.
- Reach the agent with a throwaway Go program: `guest.NewClient` over an
  AF_VSOCK connect to CID 42 port 7300. `net.FileConn` refuses a vsock fd, so
  wrap the `*os.File` in a `net.Conn` of your own.
- `systemd.journald.forward_to_console=1` on the command line puts the
  agent's unit's failures in `console.log`.
- Nothing in the bare base wants `network-online.target`; `systemctl start
  network-online.target` before reading wait-online's result.

## Flows worth driving

- `run`, then `exec` and `cp` **more than five minutes later** (`sleep 330`):
  that is what proves nothing holds a stale credential.
- `stop`, `ls -a` (stopped), `start`, `exec`.
- A guest's own `systemctl poweroff` via `exec`, then `ls -a` (stopped), then
  `start` at once (boxd may still say `stopping`).
- Probes: `start` a running instance, `url` of a stopped one,
  `exec`/`stop` a stopped one, `rm -f` twice, two `exec`s at once.
- An instance started by the old binary: read its `instances/<id>/shim.json`,
  `ps -p <pid>`, then act on it with the new binary.
- Statelessness: everything one root or host made is seen from another (a
  second `DISCO_VM_ROOT`, the same project).

## Gotchas

- Inside a discobox the Bash tool is zsh: `$VAR:refs` loses its colon, and an
  unquoted `$CMD` holding spaces is one word. Use `${VAR}` and functions.
- `pgrep -f '<pattern>'` matches the wrapper shell whose command line contains
  the pattern. Check a PID with `ps -p` instead.
- A command longer than five minutes can outlive its sentinel, and a wait on
  a guest that cannot be asked can hang rather than fail. Run long ones with
  a watchdog that kills a command whose output has been quiet for six
  minutes, and re-run.
- Clean up: `ls -a` should be empty at the end. A boxd machine left running is
  billed.
