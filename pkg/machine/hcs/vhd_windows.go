package hcs

import (
	"fmt"
	"regexp"
	"strconv"
	"syscall"
	"time"
	"unsafe"

	"github.com/Microsoft/go-winio/vhd"
	"golang.org/x/sys/windows"
)

// Virtual disks go through virtdisk.dll, which every Windows edition has; the
// Hyper-V PowerShell module (New-VHD, Mount-VHD) is absent on Home.

// createDiff creates a differencing VHDX at path over parent.
//
// A differencing disk records where its parent is. Create it where it will
// live: a child moved to another directory can resolve its relative locator
// to the wrong file (sandboxi saw it name itself, 0xC03A000E). A move that
// cannot be avoided is followed by setParent.
func createDiff(path, parent string) error {
	if err := vhd.CreateDiffVhd(path, parent, 0); err != nil {
		return fmt.Errorf("create differencing disk %s over %s: %w", path, parent, err)
	}
	return nil
}

// createFixed creates a fully allocated VHDX. Install writes into its disk
// while it is attached to the host, and a dynamic disk expanding under DISM
// is the write pattern that tripped vhdmp bus resets and hung the machine.
func createFixed(path string, size uint64) error {
	params := &vhd.CreateVirtualDiskParameters{Version: 2, Version2: vhd.CreateVersion2{MaximumSize: size}}
	h, err := vhd.CreateVirtualDisk(path, vhd.VirtualDiskAccessNone, vhd.CreateVirtualDiskFlagFullPhysicalAllocation, params)
	if err != nil {
		return fmt.Errorf("create fixed disk %s: %w", path, err)
	}
	return syscall.CloseHandle(h)
}

// createDynamicFrom copies source into a new dynamically expanding VHDX. Once
// Install has detached its disk, nothing writes to it through vhdmp again, so
// the fixed allocation has done its job and the base can shrink to what it
// holds.
func createDynamicFrom(path, source string) error {
	src, err := windows.UTF16PtrFromString(source)
	if err != nil {
		return err
	}
	params := &vhd.CreateVirtualDiskParameters{Version: 2, Version2: vhd.CreateVersion2{SourcePath: src}}
	h, err := vhd.CreateVirtualDisk(path, vhd.VirtualDiskAccessNone, vhd.CreateVirtualDiskFlagNone, params)
	if err != nil {
		return fmt.Errorf("copy %s to a dynamic disk: %w", source, err)
	}
	return syscall.CloseHandle(h)
}

// attached is a VHDX surfaced on the host as a physical disk. The attach lives
// only as long as the handle, so a killed process releases it: the disk can
// never be left attached by a crash, only by a power loss, which the
// breadcrumb covers.
type attached struct {
	handle     syscall.Handle
	diskNumber int
}

var physicalDrive = regexp.MustCompile(`(?i)PhysicalDrive(\d+)$`)

// attach surfaces path on the host. Only Install does this.
func attach(path string) (*attached, error) {
	h, err := vhd.OpenVirtualDisk(path, vhd.VirtualDiskAccessNone, vhd.OpenVirtualDiskFlagNone)
	if err != nil {
		return nil, fmt.Errorf("open %s: %w", path, err)
	}
	params := &vhd.AttachVirtualDiskParameters{Version: 2}
	if err := vhd.AttachVirtualDisk(h, vhd.AttachVirtualDiskFlagNone, params); err != nil {
		_ = syscall.CloseHandle(h)
		return nil, fmt.Errorf("attach %s: %w", path, err)
	}
	a := &attached{handle: h, diskNumber: -1}
	// Surfacing goes through PnP, so the physical path can lag the call.
	for i := 0; i < 100 && a.diskNumber < 0; i++ {
		if p, err := vhd.GetVirtualDiskPhysicalPath(h); err == nil {
			if m := physicalDrive.FindStringSubmatch(p); m != nil {
				a.diskNumber, _ = strconv.Atoi(m[1])
				break
			}
		}
		time.Sleep(100 * time.Millisecond)
	}
	if a.diskNumber < 0 {
		a.detach()
		return nil, fmt.Errorf("attach %s: it never surfaced as a physical disk", path)
	}
	return a, nil
}

func (a *attached) detach() {
	_ = vhd.DetachVirtualDisk(a.handle)
	_ = syscall.CloseHandle(a.handle)
}

var (
	virtdisk                      = windows.NewLazySystemDLL("virtdisk.dll")
	procSetVirtualDiskInformation = virtdisk.NewProc("SetVirtualDiskInformation")
)

// setVirtualDiskInfoParentPathWithDepth is SET_VIRTUAL_DISK_INFO, version
// PARENT_PATH_WITH_DEPTH. The union after Version is 8-aligned (it holds a
// pointer), and its largest arm is a GUID and a pointer.
type setVirtualDiskInfoParentPathWithDepth struct {
	version    uint32
	_          uint32
	childDepth uint32
	_          uint32
	parentPath *uint16
	_          [8]byte
}

// setParent rewrites a differencing disk's parent locator to parent. It is
// what makes moving a child across directories safe: the locator is written
// fresh for the child's new home instead of being resolved from its old one.
func setParent(path, parent string) error {
	h, err := vhd.OpenVirtualDiskWithParameters(path, vhd.VirtualDiskAccessNone, vhd.OpenVirtualDiskFlagNoParents,
		&vhd.OpenVirtualDiskParameters{Version: 2})
	if err != nil {
		return fmt.Errorf("open %s: %w", path, err)
	}
	defer syscall.CloseHandle(h)
	parentp, err := windows.UTF16PtrFromString(parent)
	if err != nil {
		return err
	}
	const parentPathWithDepth = 3
	info := setVirtualDiskInfoParentPathWithDepth{version: parentPathWithDepth, childDepth: 1, parentPath: parentp}
	if rc, _, _ := procSetVirtualDiskInformation.Call(uintptr(h), uintptr(unsafe.Pointer(&info))); rc != 0 {
		return fmt.Errorf("point %s at parent %s: %w", path, parent, syscall.Errno(rc))
	}
	return nil
}
