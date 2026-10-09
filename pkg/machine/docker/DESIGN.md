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
- **The base layer is shifted once.** Install chowns the base image's files
  into the guest's uids and commits it. Every later layer is written by the
  guest, in its uids already, so `docker commit` is the layer commit.
- **The driver is remote.** The daemon owns a running guest, so the engine
  runs no shim, and every command attaches by inspecting the container. A
  machine waits on the container only once something asks whether it stopped.
- **Dial is a docker exec of `disco-vm pipe`**, on the container's side of
  the namespace, which reaches the agent's root-only socket through the
  guest's root directory, as boxd's relay does through its Exec.
- **It is not a VM.** The kernel is the host's, `/proc` shows the host's
  resources, and the network namespace is Docker's, which the guest cannot
  reconfigure. The driver lists cold clones only, because a cold boot takes
  under a second.
