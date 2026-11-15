package hcs

import (
	"fmt"
	"syscall"

	"github.com/Microsoft/go-winio/vhd"
)

// Virtual disks go through virtdisk.dll, which every Windows edition has; the
// Hyper-V PowerShell module (New-VHD, Mount-VHD) is absent on Home.

// createDiff creates a differencing VHDX at path over parent.
//
// A differencing disk records where its parent is. Create it where it will
// live: a child moved to another directory can resolve its relative locator
// to the wrong file (sandboxi saw it name itself, 0xC03A000E). A VM's disk is
// never moved: a committed VM's stays in its own directory (Commit).
func createDiff(path, parent string) error {
	if err := vhd.CreateDiffVhd(path, parent, 0); err != nil {
		return fmt.Errorf("create differencing disk %s over %s: %w", path, parent, err)
	}
	return nil
}

// createDynamic creates a dynamically expanding VHDX of size, which only a
// VM ever opens: Install's disk, which the guest's Setup partitions and
// fills.
func createDynamic(path string, size uint64) error {
	params := &vhd.CreateVirtualDiskParameters{Version: 2, Version2: vhd.CreateVersion2{MaximumSize: size}}
	h, err := vhd.CreateVirtualDisk(path, vhd.VirtualDiskAccessNone, vhd.CreateVirtualDiskFlagNone, params)
	if err != nil {
		return fmt.Errorf("create disk %s: %w", path, err)
	}
	return syscall.CloseHandle(h)
}
