// Package hcs is the Windows driver: guests are pure Host Compute Service
// virtual machines, created through vmcompute.dll with no Hyper-V management
// stack (vmms), no Windows Sandbox (CmService), and no WSL. It works on Home.
//
// Status: shell only. Every method returns machine.ErrNotImplemented. The work
// is specified in docs/drivers/hcs.md; the proven recipes it ports are in the
// Rust sandboxw engine (the sandboxi repository, crates/sandboxw-core).
package hcs
