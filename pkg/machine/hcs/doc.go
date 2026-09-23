// Package hcs is the Windows driver: guests are pure Host Compute Service
// virtual machines, created through computecore.dll with no Hyper-V
// management stack (vmms), no Windows Sandbox (CmService), and no WSL. It
// works on Home.
//
// A layer is a VHDX (disk.vhdx) in the layer's directory: the base is a whole
// disk that Install wrote from a retail ISO and booted once through Windows
// Setup, and every later layer is a differencing disk over its parent. Guests
// get a NIC on an HNS network of the driver's own, and the agent is reached
// over hvsocket. Fork clones come from a live template held by the warm shim.
//
// The work is specified in docs/drivers/hcs.md; the recipes it ports were
// proven in Rust in the sandboxi repository (crates/sandboxw-core).
package hcs
