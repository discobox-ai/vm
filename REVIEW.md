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
- **The boxd fake API accepts only what boxd's published API documents.**
  Every field it widens (a snapshot found by name or ID) is one that
  docs.boxd.sh/reference/grpc-proto says boxd takes, and the hand-kept proto
  carries that page's comment on the field. A fake widened to match what the
  driver sends passes a test that boxd itself would fail. (ADR 26-10-15-833:
  a base's machine is made from its snapshot's ID.)
- **A driver's `Machine.Shutdown` returns once the stop is accepted, not
  before.** A caller may exit after it, and a stop left in a goroutine of its
  own dies with that process, leaving the machine running. Work before the
  guest accepts (boxd's `sync` before `StopVm`) runs inside the call, which
  `Booted.Shutdown` bounds by the shutdown's whole timeout, not a short ask
  window; a failure is returned, so the caller falls back to `Kill`.
  (ADR 26-10-15-833.)
- **Nothing above the seam keeps state, and a driver keeps it only in its
  provider.** A field a VM needs goes on the provider's resource (a Docker
  label, a boxd name, a local driver's record beside its disk), never in a
  file or a table of the engine's; a remote driver writes nothing on the
  host. A fact a provider cannot keep (the snapshot a boxd machine was
  restored from) is reported missing, not remembered elsewhere. (ADR
  26-10-23-417.)
- **Whether a VM is committed is the caller's.** The seam reports no
  committed mark, since a provider like Archil keeps a committed sandbox as a
  stopped one. A driver refuses a committed VM only where its provider cannot
  do what was asked (docker's image, boxd's snapshot: `ErrCommitted`), or
  where it would corrupt a disk (`ErrInUse`); it never writes a marker of
  its own to tell. (ADR 26-11-03-606.)
- **A listing that fails is no answer.** A reaper above the engine reads a
  failed listing as "in use", never as "unused". (ADR 26-10-23-417.)
- **No VM names another above the seam but by `From`.** A hypervisor whose
  disks chain (hcs, `local.Hypervisor.Chains`) has `local` record the ID of
  the VM a copy was made from, and refuse to remove or boot that VM with
  `machine.ErrInUse` while another VM's record, in any pool, names it;
  nothing above the seam records or walks a chain. (ADR 26-11-03-606.)
- **`local` reads every VM record before it removes or boots a VM whose
  disk may be chained.** A record it cannot read (a parse error, a
  permission) is an error, never a record left out: a record skipped is a
  disk removed or written under its child. (ADR 26-11-03-606.)
- **A record read from an unchanged path is read whether or not the upgrade
  meant to.** A model change that keeps a record's file name (`local`'s
  `instance.json`, docker's labels) reads what the old model wrote unless it
  tells that apart and leaves it out, as ADR 26-10-23-417 says it must: an
  old record half-read loses the fields only it had, and rewriting it erases
  them. Tell an old record by a field only the old model wrote. (ADR
  26-11-03-606.)
- **A change to a shared method, or to a field's type, in `pkg/guest`,
  `pkg/machine`, or `pkg/machine/vz` itself needs a search of
  `pkg/machine/vz` by hand.** Its cgo files, its tests among them, compile
  only on a Mac, so neither Linux nor `GOOS=windows` vet sees a caller or a
  literal left behind there. (Commit 335ed4b; ADR 26-11-03-606, where
  `vz_test.go` kept a field's old type.)
- **A field removed from a struct that is read from a file and written back
  stays, as `json.RawMessage`.** vz's `vz.json` and hcs's `state.json` are
  read into a struct and rewritten whole, so a field that is no longer
  declared is erased on the next write, along with whatever it held: a warm
  user's only password, in vz's case. Keep it opaque, with a comment saying
  why. (ADR 26-10-15-851.)
- **A driver acts as the host it runs on only when its configuration says
  so.** boxd's in-machine token is the host machine's owner's, so it is
  used where the privileged `inMachine` field is set and no key is given,
  never inferred from variables a provider sets in its machines (which an
  embedder's process may lack, and which a driver does not read: the
  `pkg/machine` note on settings). A review found the first draft gated on
  boxd's environment, as boxd's SDK does. (vm#109.)
- **A driver reads its key from its source, on every call, and nowhere
  else.** Not from the environment (only `machine.KeyFromEnv`, which the
  command line passes, reads it), and not cached past a call except as the
  provider's own exchange of that same key (boxd's JWT, dropped when the key
  changes). A `machine.Key` reaching a log or an error prints without its
  secret; never format `Key.Secret` itself. (ADR 26-10-24-562.)
- **An issued key expires and cannot issue keys, or the driver issues
  none.** A provider whose keys could mint more is `ErrCannotIssue`; a
  driver never hands back the caller's own key in place of one it issued.
  (ADR 26-10-24-562.)
- **Finishing an interrupted step reads only what that step still needs.**
  An upgrade caught between its roots is finished by calling it again, so
  it must not demand what the caller may have removed in between (the
  committed VM it is from, once its new root is made), and before its new
  root is made it may be retargeted, since nothing of the VM has changed: a
  VM that can only be destroyed loses the data volume the upgrade exists to
  keep. (ADR
  26-11-03-782.)
- **What a pool asks of its engine never reaches another pool.**
  `CreateOptions` and `UpgradeOptions` take no forward, pool, or
  `Instance`, and a served driver's server refuses a forward that names a
  pool and a `From` of another (`pkg/machine/served`): another pool is the
  embedder's to name, on the `Engine` (`Forwards`, `From`) or in its
  served `Options.Create` hook (discobox's ADR 26-11-11-963 §3). A field
  that carries one passes every test of the embedder and lets a pool exec
  in another's VM through a peer forward
  (`TestAPoolsRequestNamesNoForwardAndNoPool`,
  `TestOnlyTheHookReachesAnotherPool`). The served protocol decodes
  `machine.Forward` whole, so a field added to it is a field a pool sends.
