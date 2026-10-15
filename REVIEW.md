# disco-vm review notes

- **A `machine.Guest` handed out holds no connection between calls.**
  `Engine.Guest` returns one with nothing to release it, so `guest.Client`
  keeps no connections alive. An idle connection is a hijacked conn and a
  goroutine in a shim, or a `docker exec` in a guest, for every call an
  embedder makes. (Commit 335ed4b.)
- **The boxd fake API reads a command line the way the guest's tools do.**
  Its Exec parses what `pkg/machine/boxd` sends; a parser written from the
  driver's own idea of the line agrees with the driver's mistakes. GNU `env`
  ends its options at the first `NAME=VALUE`, so a `--` after the variables
  was the command, and the fake passed it until boxd itself refused it.
  (Commit 335ed4b.)
- **A change to a shared method in `pkg/guest` or `pkg/machine` needs a
  search of `pkg/machine/vz` by hand.** Its cgo files, its tests among them,
  compile only on a Mac, so neither Linux nor `GOOS=windows` vet sees a
  caller left behind there. (Commit 335ed4b.)
- **A Windows link's kind is fixed when it is made.** `CreateSymbolicLinkW`
  makes a file link or a directory link, and a file link to what is later a
  directory cannot be walked through, while `os.Readlink` still reads right
  and the record calls it applied. `pkg/converge` places files in declaration
  order, so a file can be placed through a link declared before it; do not
  reorder links to fix a Windows link's kind, which breaks that on every OS.
  (Issue #11, PR #12; `TestLinkTargets` covers what a test can.)
