# pkg/machine/archil review notes

- **Every Archil listing is read to its last page.** `GET /api/disks` pages
  by `nextCursor`, and a listing read short is a failed one that reads as
  success: a VM's data disk on page two would be missed by Destroy and
  Commit, and left billed. A lookup of one name uses Archil's `?name=`
  filter instead of a listing.
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
- **A guest script runs on BusyBox as well as GNU.** The image is the
  caller's, Alpine included: copy-in passed GNU tar's `--no-overwrite-dir`,
  and every Alpine VM failed to start, since Start writes the service file
  with it. A chunk read from a pipe with BusyBox's `head` takes more than it
  asked for (stdio), so copy out reads a regular file with `dd`. Run a
  changed script on an Alpine guest.
