# pkg/machine/boxd review notes

- **An empty `org` on an API key or a machine's token is the org it is
  fenced to, never the personal scope.** boxd's credential fence resolves
  "" to the fenced org, so a call made with no org to reach "the caller's
  own" scope reaches the org's again. The fake resolves "" the same way.
  (A `Check` that listed personal secrets with no org read the org's
  `shared` ones as personal and refused the key.)
- **A boxd call documented for a login or an API key is not one a machine's
  token is known to make.** `inMachine` drivers use such a token; a call
  boxd does not document for it either degrades (no cap) or is listed as
  unverified, and the fake refuses it where a test says so. (A `Check` that
  failed on a refused `GetOrgMachineLimit` would have broken every
  in-machine driver.)
- **A NotFound for a machine the driver has just made, or is cleaning up
  after, is not proof that it is gone.** boxd answered one to a call on a
  machine `CreateVm` had just made (region fra, 2026-11-15), so a create
  that counted it final, or a cleanup that counted a destroy's NotFound as
  done, left the machine running unseen with its default proxy open. Retry
  a create's steps on NotFound, and confirm a destroy's NotFound against
  `ListVms`. (vm#169.)
- **A proxy lock boxd answered OK is not yet a lock.** boxd applied
  `SetProxyPort` after it answered (region fra, 2026-11-21), and a create
  that read the proxy back once failed. `lockProxy` reads it back until it
  shows, for a bounded while, asking again only seldom, since boxd does not
  say whether a lock asked again restarts a pending one; it fails naming
  what it read. (vm#210.)
- **On a machine whose ingress URL is off, nothing that loosens access to
  its proxies runs until they read back closed.** Create closes them with
  the guest already running, a restore's servers already listening, so a
  step that loosens first (bot protection off before the port is locked to
  0) leaves an open proxy unguarded, and a failed create whose destroy
  fails too can leave it so. (Bot protection was first turned off before
  the close.)
- **A wait for the guest accepts only what a settled guest prints, and keeps
  what it was told when it gives up.** Early in a boot, systemd answers
  `offline` or a bus error at once, so a wait that took any answer for
  settled let Commit snapshot a guest still initializing, and one that drops
  stderr fails after minutes with no reason. Its exit code alone decides
  nothing: `is-system-running` exits 0 only for `running`. (vm#180.)
- **A wait for the guest is proven on boxd itself, on the image it is for,
  not on the fake alone.** The fake answers whatever the test tells it, and
  systemd's early-boot answers differ by image: the default image never
  showed `--wait`'s hang or its early `offline`. (vm#179, vm#180.)
- **A refusal `untilRunning` tolerates is one of the calls that boot the
  machine, and only for a while.** Its loop writes one `err` from every call
  it makes, so a case added to the error switch at its end catches the
  resize's `StopVm` too, whose refusal no later read undoes, and the wait
  then lasts its full limit. Mark the calls a refusal may race (`booting`),
  and bound the tolerance by time, not a count of refusals: how long boxd's
  reads lag a move it has begun is not known (the proto's `GetVm`), so a
  count of polls is a guess at it in units of `waitEvery`. (vm#214.)
