# pkg/machine/archil review notes

- **Every Archil listing is read to its last page.** `GET /api/disks` pages
  by `nextCursor`, and a listing read short is a failed one that reads as
  success: a VM's data disk on page two would be missed by Destroy and
  Commit, and left billed. `GET /api/sandboxes` is followed the same way,
  though Archil sends every sandbox at once today, and a cursor that comes
  back again fails the listing rather than end it. The fake API pages both
  two at a time under the conformance suite. A lookup of one name uses
  Archil's `?name=` filter instead of a listing.
- **Asking about a VM changes nothing about it.** `Engine.State` attaches to
  every VM a listing shows, so Attach neither resumes a paused sandbox nor
  starts a watcher; only an operation that needs the guest resumes it, and
  only `Done`/`Err`/`Kill` watch. A poller started by Start, or a resume by
  `ps`, ran for as long as an embedder kept the VM.
- **Output waits for its reader, and a kill ends the wait.** A process's
  unread output is bounded (`pipeLimit`), so the WebSocket reader blocks
  behind a slow writer, and `Kill` aborts the buffers so a reader that has
  gone away never holds the connection's goroutine.
- **The fake process API sends frames as Archil does.** An output frame's
  offset counts both streams together, one cursor for the process (the
  SDK's `SandboxProcess._cursor`). A fake with an offset per stream passed a
  driver that kept a cursor per stream, a protocol Archil does not speak.
- **Archil drops process output that outruns its connection.** It keeps a
  bounded buffer per process and skips ahead, so a 16 MB burst read at full
  speed arrived with 6 MB missing and exit 0. Bulk data out of a guest goes
  a requested chunk at a time (`copyOutScript`, as the SDK downloads a
  file) and fails at the first gap a later frame shows, since it would
  otherwise wait for bytes that never come (a chunk whose tail was dropped
  shows none, and still waits: a chunk is kept well under the buffer Archil
  overran, about 1 MB, for that reason); a process whose connection ended
  without an exit is still killed in the guest; an exec whose output has a gap fails once it exits
  rather than pass a short stream as whole. The fake that serves a copy
  out drops part of a chunk to prove it (`TestCopyFrom`).
- **Every name sent to Archil is checked against its OpenAPI limit.** A
  sandbox's is 63 characters, a disk's 100; a name past one fails with
  Archil's raw 400 after the checks passed it.
- **A guest script runs on BusyBox as well as GNU.** A base without apt
  and without systemd (Alpine) is refused at Create now, but one with
  systemd may still carry BusyBox's tools. Alpine was the case that showed
  it: copy-in passed GNU tar's `--no-overwrite-dir`,
  and every Alpine VM failed to start, since Start writes the service file
  with it. A chunk read from a pipe with BusyBox's `head` takes more than it
  asked for (stdio), so copy out reads a regular file with `dd`. Run a
  changed script under BusyBox's `sh`, `tar`, `head`, and `dd`, beside
  util-linux (`busybox sh` on a Debian base with `busybox` installed).
- **A data disk with no sandbox is a VM, never a leftover.** An upgrade
  deletes the sandbox before it makes it again, and one stopped between
  leaves only the disk: listing, Create (`ErrExists`), Start, and Destroy
  treat it as that VM, and nothing removes it as stale. Create once deleted
  such a disk, which would now delete the data of a VM mid-upgrade.
- **The fake API (`fakeapi_test.go`) answers as Archil was seen to.** A
  name taken is `sandbox_name_conflict`, a fork boots at once with its
  source's mounts and policy, a sandbox with forks is `sandbox_has_forks`, a
  policy is set only on a running sandbox, and a disk's create answers the
  disk of its name. A behavior added to the driver is added to the fake
  from what Archil does, checked on Archil, not from what the driver
  expects.
- **The fake reads the driver's command lines with sh, not a Go parser.**
  `parseLine` gives the line to `sh` with its `exec` made a `printf` of
  each word, so a word the driver quotes wrong reaches the fake as a guest's
  shell would run it. A parser written from the driver's own idea of the
  line agrees with the driver's mistakes, as boxd's fake did (the root
  `REVIEW.md`); `TestCommandLinesAreWhatShRuns` round-trips words that need
  quoting.
- **Nothing opens 7302 but a token.** No call exposes a port, a sandbox is
  made with `enable_service_ingress: false`, and a request without a token
  must neither reach the guest nor wake it. A change that exposes a port, or
  mints a token anywhere but `Service`, opens the machine to the internet.
- **The driver keeps no token.** A token's secret is returned once; a
  driver that cached one would keep state (ADR 26-10-23-417), and one that
  minted at a start would mint for no holder. Every path that deletes,
  freezes, or forks a sandbox revokes its tokens first, and a revoke that
  fails fails the call.
- **The fake keeps tokens worse than Archil does, on purpose.** They outlive
  their sandbox and a fork copies its source's, where Archil was seen to do
  neither (2026-11-18), so a revoke the driver drops shows as a token left
  honored. It is the one place the fake departs from what Archil was seen
  to do, and its comment says so.
- **A migration covers every state an old VM can be in.** A VM made
  before ports were left unexposed has 7302 public, and a request there
  wakes it with no call of the driver's, so unexposing it at a cold start
  alone left running, paused, woken, and committed ones public while
  `Service` handed out a credential for them. Every Start, Commit, and
  Service unexposes it, and so does every copy made from a VM, a Create or
  an Upgrade from it, for both the source and the copy: a committed VM is
  never started, so its copies are what fence it.
- **Every line a guest operation sends enters systemd's namespaces.** A
  process the driver starts runs in Archil's, where `systemctl` reaches no
  systemd and `/run` is not systemd's; only `ensureScript`, which makes
  systemd run, and the readiness `true` may run there. A new guest script
  goes through `command`, and the fake refuses a line that does not.
- **Nothing installs at a boot.** systemd is installed at Create, under the
  fence, and the boot service only runs it: a VM fenced from its mirror
  needs a base with systemd, and a boot must not depend on the network.
- **The fake container harness stands in for Archil's supervisor.**
  `fakeArchilClient` and `fakeInit` (`container_test.go`) play what vm#86
  saw of `archil-client services`; a change to how the driver uses it is
  checked on Archil, not against the stand-in alone.
- **An upgrade can fail only after its sandbox is deleted on what the new
  root holds.** Archil can't rename a sandbox, so the new root boots under
  the name only once the old one is gone, and a fresh base without systemd
  fenced from its mirror fails there. Anything that can be known without
  booting it is checked before the delete; nothing new is to be added after
  it.
- **A command entered into systemd's namespaces stays in its nsenter's
  process group.** Archil's kill SIGKILLs the group (seen 2026-11-21: no
  trap ran, and nothing was left), and that is what ends a killed command
  in there; `nsenter` forwards nothing. Don't `setsid` or change the group
  on that path. `setpriv --pdeathsig TERM`, set again under `runuser`
  (whose fork and change of account clear it), covers `nsenter` dying
  alone.
