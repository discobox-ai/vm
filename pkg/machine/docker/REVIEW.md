# pkg/machine/docker review

- **A mount names a volume the daemon makes if it is missing.** A container
  created with a `type=volume` mount of a volume that is gone gets a new one,
  empty and unlabeled, under that name, and no error. Since ADR
  26-11-03-782 only a VM from before it mounts named volumes, and a helper
  (Upgrade's copy into directories, SaveCache's of its cache) mounts one only
  while that VM's container still holds it, so nothing can remove it in
  between. A review once found a Create racing a
  save into an empty cache this way, when caches were named volumes.
- **A container inherits its image's labels, and `docker commit` copies the
  container's into the image.** Every label an instance's container carries
  about itself is blanked by the commit and set, empty if need be, at
  Create; one that is not leaks into every VM created from a committed one
  (`.committed` included, which would make a container read as committed).
  Verify found such VMs reporting their source's data and cache volumes
  this way.
- **Find what a VM owns by its name, not through its container, and
  remove it before the container.** A Destroy that removes the container
  first and fails after has nothing left to inspect, so its retry would leak
  the rest: the directories, at paths named for the VM, go first, and a VM
  from before ADR 26-11-03-782 has its named volumes found by
  `ai.discobox.vm.pool` and `.instance`. A name is every instance of it,
  so a Create empties its directories only once its container holds the
  name, and a removal of old volumes by label leaves one a container mounts
  (the daemon refuses it).
- **A committed VM's tag is removed with `force`, never its image by ID.**
  Forced, the daemon only untags an image a container still runs, so a VM
  created from it stands alone; that container's Destroy removes the
  untagged image. A forced removal by ID would delete what a container
  runs, and an unforced one by tag is refused while one does.
- **A VM is its container and, mid-upgrade, its `--upgrade` sibling.**
  Every path that runs, commits, or removes a VM's container looks under
  both names (`vmContainers`, `whole`, and Destroy of a committed VM), never
  one inspect by name or ID: Commit once left the sibling behind, where it
  blocked every Create of the name once the committed VM was removed. (ADR 26-11-03-782.)
- **A daemon connection that costs a process is never kept idle, and its
  dial has a deadline of its own.** `GuestDaemon`'s connections are relays
  the guest runs; `KeepIdle` is false so `net/http` closes each once its
  answer is read, and `processConn.Close` ends the Exec session, which the
  agent answers by killing and reaping the relay. A transport that pools
  them leaves a `disco-vm pipe` running in the host VM per request. And
  `net/http` dials on a context detached from the request (Go 1.27,
  `context.WithoutCancel`), so a request canceled while its relay starts
  does not stop the dial: `relayReady` does, and a dial that waits on ctx
  alone leaks a relay per canceled request (a review found it; ADR
  26-11-04-412 §3). Every daemon connection must also half-close: an exec's
  stdin ends that way.
