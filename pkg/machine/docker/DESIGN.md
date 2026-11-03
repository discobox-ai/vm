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
  `disco-vm-base:<project>--<name>--<random>`, which Destroy removes with
  every other such tag of the name older than a readying could be that no
  container runs. The container that readies it is listed as a VM while it
  runs, so one a crash leaves can be removed. A committed VM is written by
  the guest, in its uids already, so `docker commit` is the commit. The
  container's root directory is the exception: Docker makes it for each
  container, as the host's root, so docker-init chowns it at every start.
- **A committed VM is a Docker image, and its container is removed.**
  Commit `docker commit`s the stopped container as
  `disco-vm-vm:<project>--<name>`, labeled `ai.discobox.vm.committed` with
  its name, size, and the committed VM it came from, then removes the
  container and its volumes. A VM created from it is a container of that
  image, by ID, so it stands alone: removing the committed VM removes its
  tag with `force`, which the daemon turns into an untag while a container
  runs it, and that container's Destroy removes the untagged image. A name
  is the project's across containers and committed images: Create refuses
  one either holds, and checks again once its container is made. The
  driver tells a committed VM by its ID, the image's `sha256:...`; the
  seam reports no committed mark, which is the caller's to record. Boot,
  Start, Commit, and SaveCache of one are `ErrCommitted`, and Attach and
  Service `ErrNotRunning`: an image has no container to run.
- **The daemon owns everything.** It owns a running guest, so there is no
  shim, and every command attaches by inspecting the container. A machine
  waits on the container only once something asks whether it stopped. And it
  keeps what the driver knows (ADR 26-10-23-417): a VM is a container
  `dvm-<project>--<name>` labeled with its name, size, volumes, and the
  committed VM it came from, and a committed VM the Docker image above.
  Nothing is kept on the host.
- **The guest's operations are the agent's**, reached by a docker exec of
  `disco-vm pipe` for each connection, on the container's side of the
  namespace, which reaches the agent's root-only socket through the guest's
  root directory. No other guest port is dialed: the service's URL is a port
  the daemon publishes, and its private URL is the service port at the
  container's IPv4 address on its network, where another container dials it,
  and none for a container with no IPv4 address (ADR 26-10-29-129). There is no egress fence.
- **A volume is a named Docker volume** (ADR 26-11-02-180), mounted with no
  copy of what the image holds at its path. An instance's data and cache
  volumes are `dvm-<project>--<name>--data-<random>` and `--cache-<random>`,
  removed with its container; docker-init gives the guest's root their
  mount points, which Docker makes as the host's root, as it gives it `/`.
  A cache generation is `dvm-<project>--cachegen--<key>--<id>`, and a key's
  pointer to it `--cachekey--<key>--<id>`, an empty volume made once the
  generation is whole, since labels never change; a key's current generation
  is its newest pointer's. A cache is copied between volumes as a tar stream
  through the daemon's archive API, the guest's owners in its headers, with
  a container made and never started to mount the other end: O(its size).
- **It is not a VM.** The kernel is the host's, `/proc` shows the host's
  resources, and the network namespace is Docker's, which the guest cannot
  reconfigure. The driver lists cold clones only, because a cold boot takes
  under a second.
