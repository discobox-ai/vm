# pkg/guest review

- **The Linux base's initrd has no busybox.** initramfs-tools without it ships
  klibc's tools alone, which have no `cp` or `chmod`: a hook that needs a
  tool puts it in with `copy_exec` from a build hook
  (`linux/disco-vm-agent-hook`), and `verify` checks the initrd holds it. The
  first boot of the base found this.
- **Nothing runs from `/run` in the Linux base.** initramfs-tools mounts it
  noexec, and the root keeps that mount. The agent runs from `/run/disco-vm`,
  a tmpfs the init-bottom hook mounts for it; a second boot of the base found
  this.
- **Nothing pulls in `network-online.target` in the base itself**, so
  `systemd-networkd-wait-online` runs only once a unit wants that target (a
  bundle's). A check of its condition starts the target, or it proves
  nothing.
- **`guest.Protocol` moves with any change a host and an agent of either
  side cannot both speak.** vz's Linux guests refuse an agent of another
  protocol at every boot, so forgetting to move it is a guest that answers
  and then misbehaves.
- **Info, a file put, and exec stay compatible across every protocol.** vz
  moves a macOS guest's baked-in agent forward through the old agent's own
  `GET /v1/info`, `PUT /v1/files`, and `POST /v1/exec` (vz's `moveAgent`),
  so a change that an older agent's side of those three cannot serve
  strands every committed macOS VM short of an install. Such a change is
  not a protocol bump: it needs another way in, decided first. The test's
  older agent (`disco_vm_older_agent`) is today's code reporting the
  previous number, so it cannot catch this; review has to.
- **`disco-vm guest --protocol` prints the protocol and nothing else**, and
  exits 0. The move runs it in the guest on the uploaded binary and compares
  its output exactly before it replaces the running agent.
