# pkg/guest review

- **The Linux base's initrd has no busybox.** initramfs-tools without it ships
  klibc's tools alone, which have no `cp` or `chmod`: a hook that needs a
  tool puts it in with `copy_exec` from a build hook
  (`linux/disco-vm-agent-hook`), and `verify` checks the initrd holds it. The
  first boot of the base found this.
- **The Linux base's initrd compression is what `verify` reads, not what
  `initramfs.conf` says.** `update-initramfs` falls back to gzip, with only a
  warning, when the compressor `COMPRESS` names is not in the root: the base
  shipped gzip under `COMPRESS=zstd` until its root carried `zstd`. A change to the root's packages
  or to `COMPRESS` is judged by `verify`'s check of the built initrd.
- **A `verify` check reads its tool's own exit status, never a pipeline's,
  and is shown failing on a bad artifact as well as passing on a good one.**
  `sh` has no `pipefail`, so `$(tool | tail | cut) || fail` sees only `cut`.
  The initrd's compression check first walked early archives with GNU `cpio`
  in 512-byte blocks, which sid's mkinitramfs (3cpio, 4-byte padding) broke,
  and its 3cpio replacement first ignored 3cpio's own failure.
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
- **`hardlockup_panic` reading 1 does not mean a hard lockup resets the
  guest.** The sysctl is set by the command line whether or not a detector
  runs: Linux turns the detector off in an x86 KVM guest, and the perf
  detector does not start without a guest PMU. Check `nmi_watchdog` reads 1
  and dmesg shows the detector starting, and prove the reset by stopping one
  vCPU thread from the host (the buddy detector catches that; the perf one
  cannot, since its NMI runs on the stopped CPU).
