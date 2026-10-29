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
- **A base is any image a registry has, shifted once per instance.** There
  is no install. `Base` finds a ref the daemon has or its registry answers
  for (`/distribution`; only a 401, 403, or 404 is "none"), without pulling;
  Create of an instance of it pulls the image, adds systemd and the agent
  (`$DISCO_VM_AGENT`, or this binary on linux), chowns its files into the
  guest's uids, and commits that as the instance's own Docker image,
  `disco-vm-layer:<project>--base-<name>--<random>`, which Destroy removes
  with every other such tag of the name older than a readying could be that
  no container runs; the `-` in
  its tag keeps it out of the images. The container that readies it is
  listed as an instance while it runs, so one a crash leaves can be removed. An image is written by the guest, in its uids already,
  so `docker commit` is the snapshot. The container's root directory is the
  exception: Docker makes it for each container, as the host's root, so
  docker-init chowns it at every start.
- **The daemon owns everything.** It owns a running guest, so there is no
  shim, and every command attaches by inspecting the container. A machine
  waits on the container only once something asks whether it stopped. And it
  keeps what the driver knows (ADR 26-10-23-417): an image is a Docker image
  tagged `disco-vm-layer:<project>--<id>` and labeled with its comment, a tag
  a Docker image tag under `disco-vm/<project>/`, and an instance a container
  `dvm-<project>--<name>` labeled with its image reference and size, whose
  image is the one whose Docker image it runs. Nothing is kept on the host.
- **The guest's operations are the agent's**, reached by a docker exec of
  `disco-vm pipe` for each connection, on the container's side of the
  namespace, which reaches the agent's root-only socket through the guest's
  root directory. No other guest port is dialed: the service's URL is a port
  the daemon publishes, and its private URL is the service port at the
  container's IPv4 address on its network, where another container dials it,
  and none for a container with no IPv4 address (ADR 26-10-29-129). There is no egress fence.
- **It is not a VM.** The kernel is the host's, `/proc` shows the host's
  resources, and the network namespace is Docker's, which the guest cannot
  reconfigure. The driver lists cold clones only, because a cold boot takes
  under a second.
