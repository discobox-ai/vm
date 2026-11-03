// Package vz is the macOS driver: guests are Virtualization.framework virtual
// machines, driven through the Code-Hex/vz bindings (discobox's fork, which adds
// macOS 27 guest provisioning and fixes socket descriptor ownership).
//
// A macOS guest is a bundle of files that belong together, and every VM,
// committed or not, is one bundle in its own directory:
//
//   - disk.img, the disk the installer formats;
//   - aux.img, the guest's NVRAM, laid out for its hardware model;
//   - hardwaremodel, chosen by the restore image and fixed at install;
//   - machineidentifier, unique per running guest: two guests that share one
//     is undefined behavior in the guest;
//   - vz.json, what the install chose (see meta).
//
// A VM adds macaddress, created on its first boot. A committed VM keeps only
// its layer files (layerFiles): the identity and MAC are a running machine's,
// and a VM created from it gets its own.
//
// There are no chains. Every bundle is a full APFS clone of the committed VM
// it was created from, which shares its blocks until it writes, so a 128 GiB
// disk costs nothing to clone, and a committed VM can be removed while VMs
// created from it live. A fresh VM is a Linux guest from the configured guest
// image (machine.Config.Base, or DefaultLinuxBase).
//
// The real driver is cgo (the darwin && cgo files). A darwin build without cgo
// gets a stub whose every method says how to rebuild.
package vz
