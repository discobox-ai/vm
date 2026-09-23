// Package vz is the macOS driver: guests are Virtualization.framework virtual
// machines, driven through the Code-Hex/vz bindings (discobox's fork, which adds
// macOS 27 guest provisioning).
//
// Status: shell only. Every method returns machine.ErrNotImplemented. The work
// is specified in docs/drivers/vz.md; the proven recipes it ports are discobox's
// macvm proof of concept (branch poc/macos-guest, server/internal/macvm).
package vz
