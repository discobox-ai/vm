// Package hcs is the Windows driver: guests are pure Host Compute Service
// virtual machines, created through computecore.dll with no Hyper-V
// management stack (vmms), no Windows Sandbox (CmService), and no WSL. It
// works on Home.
//
// An image is a VHDX (disk.vhdx) in the image's directory: an install is a
// whole disk that Install wrote from a retail ISO and booted once through
// Windows Setup, and a snapshot is the differencing disk of an instance, over
// the disk of the image the instance ran from. Above the seam every image
// stands alone; the hypervisor Chains, so package local keeps each image's
// parent, hands Prepare the whole chain, and refuses to delete a disk another
// image's still needs. Guests
// get a NIC on an HNS network of the driver's own, and the agent is reached
// over hvsocket.
//
// The work is specified in docs/drivers/hcs.md; the recipes it ports were
// proven in Rust in the sandboxi repository (crates/sandboxw-core).
package hcs
