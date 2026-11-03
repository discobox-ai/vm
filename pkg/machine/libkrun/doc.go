// Package libkrun is the Linux driver (ADR 26-11-03-530): guests are libkrun
// microVMs on KVM, each run by a launcher process, krun-launch, which its
// shim starts and holds. No disco-vm process loads libkrun: the binary stays
// static, and stays the guest agent.
//
// The host side is a runtime, one directory pulled by digest into the
// driver's cache (cache/libkrun/runtime/<digest>), or named by
// $DISCO_VM_KRUN_RUNTIME: the launcher, passt, and libkrun with every library
// it needs, glibc and its loader among them. The launcher is run through that
// loader, so the host's libc sets no floor (§4).
//
// The guest is disco-vm's Linux guest image (pkg/guest/linux), its amd64
// variant, booted with the kernel it carries for libkrun (krun/vmlinux, built
// from libkrunfw's source), and every VM, committed or not, is one bundle in
// its own directory:
//
//   - vmlinux, initrd.img, disk.img (the root, raw ext4), and krun.json, which
//     a committed VM keeps;
//   - machineid and macaddress, a VM's own, given at Create;
//   - launch.conf, boot-initrd.img, and console.log (and the boot before's,
//     console.prev.log), written at every boot.
//
// There are no chains: a VM's files are a reflink copy of the committed VM's
// where the filesystem has them (btrfs, XFS), and a sparse copy otherwise.
// The package is linux only, and pure Go.
package libkrun
