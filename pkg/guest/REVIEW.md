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
- **A file put and exec stay compatible across every protocol.** vz puts a
  macOS guest installed before its agent stub on the stub through the old
  agent's own `PUT /v1/files` and `POST /v1/exec` (vz's `adoptStub`), so a
  change that an older agent's side of those two cannot serve strands every
  such committed macOS VM short of an install. Such a change is not a
  protocol bump: it needs another way in, decided first. The test's older
  agent (`disco_vm_older_agent`) is today's code reporting the previous
  number, so it cannot catch this; review has to.
