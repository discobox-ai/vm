// Package hcs is the Windows driver: guests are pure Host Compute Service
// virtual machines, created through computecore.dll with no Hyper-V
// management stack (vmms), no Windows Sandbox (CmService), and no WSL. It
// works on Home.
//
// A VM's disk is a VHDX (disk.vhdx) in its directory: an install is a whole
// disk that Install wrote from a retail ISO and booted once through Windows
// Setup, and a VM created from a committed one is a differencing disk over
// that one's, which commit leaves where it is (ADR 26-11-03-606). The
// hypervisor Chains, so package local hands Prepare the whole chain, and
// refuses to remove or boot a VM another VM's disk still needs. Guests
// get a NIC on an HNS network of the driver's own, and the agent is reached
// over hvsocket.
//
// The work is specified in docs/drivers/hcs.md; the recipes it ports were
// proven in Rust in the sandboxi repository (crates/sandboxw-core).
package hcs
