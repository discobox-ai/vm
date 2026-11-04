# pkg/machine/docker

A Linux guest can be a container instead of a VM: systemd is its init, its
root has every capability over the guest, and on the host it is an
unprivileged uid. Docker offers a user namespace only for a whole daemon, so
the driver makes one per container, without `--privileged` or an added
capability. See [docs/drivers/docker.md](../../../docs/drivers/docker.md).

- **The container's PID 1 is `disco-vm docker-init`.** It hands the
  container's cgroup to the guest, starts systemd in new user, mount, pid,
  cgroup, uts, and ipc namespaces with guest uids 0-65535 at host uid
  16777216 and up, and ends the container when systemd powers off. The
  container runs with seccomp, AppArmor, and Docker's masked `/proc` paths
  off, and a writable cgroup: what protects the host is the user namespace.
- **The base is a configured registry image, shifted once per fresh VM**
  (`machine.Config.Base`, the command line's `$DISCO_VM_BASE`; ADR
  26-11-03-606). There is no install, and with no base a fresh VM is
  `ErrUnsupported`. A fresh Create pulls the image, adds systemd and the
  agent (`$DISCO_VM_AGENT`, or this binary on linux), chowns its files into
  the guest's uids, and commits that as the VM's own Docker image,
  `disco-vm-base:<pool>--<name>--<random>`, which Destroy removes with
  every other such tag of the name older than a readying could be that no
  container runs. The container that readies it is listed as a VM while it
  runs, so one a crash leaves can be removed. A committed VM is written by
  the guest, in its uids already, so `docker commit` is the commit. The
  container's root directory is the exception: Docker makes it for each
  container, as the host's root, so docker-init chowns it at every start.
- **A committed VM is a Docker image, and its container is removed.**
  Commit `docker commit`s the stopped container as
  `disco-vm-vm:<pool>--<name>`, labeled `ai.discobox.vm.committed` with
  its name, size, and the committed VM it came from, then removes the
  container and its directories. A VM created from it is a container of that
  image, by ID, so it stands alone: removing the committed VM removes its
  tag with `force`, which the daemon turns into an untag while a container
  runs it, and that container's Destroy removes the untagged image. A name
  is the pool's across containers and committed images: Create refuses
  one either holds, and checks again once its container is made. The
  driver tells a committed VM by its ID, the image's `sha256:...`; the
  seam reports no committed mark, which is the caller's to record. Boot,
  Start, Commit, and SaveCache of one are `ErrCommitted`, and Attach and
  Service `ErrNotRunning`: an image has no container to run.
- **The daemon owns everything.** It owns a running guest, so there is no
  shim, and every command attaches by inspecting the container. A machine
  waits on the container only once something asks whether it stopped. And it
  keeps what the driver knows (ADR 26-10-23-417): a VM is a container
  `dvm-<pool>--<name>` labeled with its name, size, volumes, and the
  committed VM it came from, and a committed VM the Docker image above.
  Only volumes and caches are kept on the daemon's host, in directories
  (below).
- **The guest's operations are the agent's**, reached by a docker exec of
  `disco-vm pipe` for each connection, on the container's side of the
  namespace, which reaches the agent's root-only socket through the guest's
  root directory. No other guest port is dialed: the service's URL is a port
  the daemon publishes, and its private URL is the service port at the
  container's IPv4 address on its network, where another container dials it,
  and none for a container with no IPv4 address (ADR 26-10-29-129). There is no egress fence.
- **A volume is a directory on the daemon's host** (ADR 26-11-03-782 §6),
  under the driver's root (`Driver.Root`, `$DISCO_VM_DOCKER_ROOT`, default
  `/var/lib/discobox`), laid out as discobox's own docker driver lays out
  its tree: `data/pools/<pool>/vms/<name>`,
  `cache/pools/<pool>/vms/<name>`, and a key's generations at
  `cache/pools/<pool>/keys/<key>/<generation>`, its pointer in
  `current.json` beside them, replaced by a rename once a generation is
  whole and renamed into place. A VM's directories are bind-mounted at their
  paths; docker-init gives the guest's root their mount points at every
  start, so files keep the guest's shifted uids. The daemon may be remote,
  so the driver never touches them: a helper container of the agent alone
  (`disco-vm-helper:<sha256 prefix>`, imported from `$DISCO_VM_AGENT`), with
  no network, bind-mounts the root and runs `disco-vm docker-volumes
  REQUEST` for each step, and copies on the host. Another version's helper
  image is never removed: another disco-vm on the daemon may be using it. A
  key's `.lock` is a flock: a clone holds it shared, and a save's guard,
  publish and prune, and a delete, hold it exclusive. A save copies into
  `.save-<generation>` holding `.save-<generation>.lock`, and a prune sweeps
  a copy whose lock nobody holds, what a save that died left. Create stages the clone and makes the
  directories before its container (a bind source must exist), and only
  once its container holds the name empties the data directory and puts the
  clone in place, so what a create that lost left is never a later VM's.
  Commit and Destroy remove a VM's directories, Destroy before its
  containers. A VM from before the ADR mounts named volumes
  (`dvm-<pool>--<name>--data-<random>`, `--cache-<random>`): it boots,
  SaveCache reads its cache from one, Upgrade copies them into its
  directories, and Destroy removes them; old generation volumes are not read.
- **Upgrade makes a new container and renames it into place.** The new one,
  of the committed VM's image or one readied from the base, carries the
  VM's labels and mounts its same directories, under
  `dvm-<pool>--<name>--upgrade`; then the old one is removed, then the
  new one renamed. The old one is checked stopped again once the new one is
  made, since readying a base takes minutes. While caught between, the VM
  is its old container until that is gone, then the new one; from the
  moment the new one exists Boot, Start, and Commit refuse the VM until an
  Upgrade from the same `from` finishes it, which needs the committed VM no
  more, since a container is whole once made. Destroy removes both and the
  directories. The new container's `ai.discobox.vm.replaces` lists the IDs
  it replaced, so an `Instance` from before still names it, and
  `.replaced-image` the image to remove once nothing needs it.
- **It is not a VM.** The kernel is the host's, `/proc` shows the host's
  resources, and the network namespace is Docker's, which the guest cannot
  reconfigure. The driver lists cold clones only, because a cold boot takes
  under a second.
