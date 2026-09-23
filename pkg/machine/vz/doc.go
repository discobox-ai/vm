// Package vz is the macOS driver: guests are Virtualization.framework virtual
// machines, driven through the Code-Hex/vz bindings (discobox's fork, which adds
// macOS 27 guest provisioning and fixes socket descriptor ownership).
//
// A macOS guest is a bundle of files that belong together, and a layer, an
// instance, and a staged clone are each one bundle in their own directory:
//
//   - disk.img, the disk the installer formats;
//   - aux.img, the guest's NVRAM, laid out for its hardware model;
//   - hardwaremodel, chosen by the restore image and fixed at install;
//   - machineidentifier, unique per running guest: two guests that share one
//     is undefined behavior in the guest;
//   - vz.json, what the install chose (see meta).
//
// An instance adds macaddress, created on its first boot, and a resume clone
// arrives with state.bin, its saved memory. A layer never has either: both are
// a running machine's identity, and a clone of a layer gets its own.
//
// There are no chains. Every bundle is a full APFS clone of its parent, which
// shares the parent's blocks until it writes, so a 128 GiB disk costs nothing to
// clone and a driver only ever reads a chain's last layer.
//
// The real driver is cgo (the darwin && cgo files). A darwin build without cgo
// gets a stub whose every method says how to rebuild.
package vz
