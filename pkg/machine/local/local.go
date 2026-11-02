// Package local is a machine.Driver whose provider is this host's disk: the
// store and the supervisor that hcs, vz, and the fake driver share. Each of
// them is a Hypervisor, which knows only how to install, copy, boot, and
// freeze a disk in a directory it is given; this package keeps everything
// else, in a directory of its own (machine.Config.Dir), the way a Docker
// daemon keeps /var/lib/docker:
//
//	instances/<id>/machine/          the hypervisor's VM (InstanceSpec.Dir)
//	instances/<id>/machine.next/     its new disk, while an upgrade makes it
//	instances/<id>/machine.old/      its old disk, while an upgrade removes it
//	instances/<id>/instance.json     the VM: project, name, from, size
//	instances/<id>/shim.json         its shim, while one holds it
//	instances/.names/<project>/<name> the name's claim, holding the ID
//	caches/<project>/<key>.json      a cache key: its current generation
//	caches/generations/<id>/payload/ a cache generation's volume (Layer.Dir)
//	cache/<hypervisor>/              media the hypervisor downloads
//
// A committed VM (ADR 26-11-03-606) is its record and its disk, left where
// it is, with its volumes gone; that it is committed is the caller's to
// record. Nothing is kept anywhere else, and nothing needs a
// lock: a record is replaced by a rename, and a name is claimed by creating
// its file exclusively.
//
// A Virtualization.framework machine dies with the process that booted it,
// and an HCS machine belongs to vmcompute, so Start runs each instance under
// a shim process of its own (ShimMain), which boots it, holds it, and serves
// its guest operations, its agent's and service's ports, and its forwards
// on an owner-only Unix socket, and its URL on the host's loopback. Any process of the owner's
// attaches to it there (Attach).
package local

import (
	"context"
	"io"

	"github.com/discobox-ai/vm/pkg/machine"
)

// Layer is a committed VM's disk, in its own directory (its InstanceSpec.Dir,
// final from its Prepare or Install on), or a cache generation's: one link of
// a chain, for a hypervisor that Chains. A cache generation is written under
// a temporary name and renamed into place, so a hypervisor must not record a
// generation's own Dir inside its files. Recording a parent's Dir (a
// differencing disk's parent path) is fine: a parent is final before its
// child is written.
type Layer struct {
	ID  string
	Dir string
}

// InstallSpec is a machine.InstallSpec and where the hypervisor keeps the
// media it downloads (Media "latest") across installs, so a 20 GB restore
// image is fetched once. CacheDir may not exist yet.
type InstallSpec struct {
	machine.InstallSpec
	CacheDir string
}

// InstanceSpec is one instance as a hypervisor sees it.
type InstanceSpec struct {
	ID string
	// Dir is the hypervisor's for this instance. Prepare creates it.
	Dir     string
	GuestOS machine.OS
	// Chain is the committed VM the instance was created from last, after
	// the committed VMs its disk differences over, for a hypervisor that
	// Chains, which is given it at every call. A hypervisor that does not
	// chain gets that one alone, and only at Prepare: its copy is a disk of
	// its own, which outlives the committed VM. It is empty for a fresh VM,
	// which Prepare makes from the hypervisor's base.
	Chain []Layer
	// CacheDir is where the hypervisor caches what it downloads, which
	// Prepare may fill from its base (vz pulls a Linux guest image there).
	// It may not exist yet.
	CacheDir string
	// Egress is the fence the instance was created with, which its record
	// keeps: nil is none. A hypervisor holds it at every boot (vz: an empty
	// Allow is no network device).
	Egress *machine.Egress
	// Forwards are the instance's forwards, which its record keeps. A
	// hypervisor that must map a guest port to the host before its VM starts
	// (libkrun) maps each one's Port at every boot (ADR 26-11-03-247).
	Forwards []machine.Forward
	// Data and Cache are the instance's volumes, which its record keeps:
	// nil is none. Prepare makes them, at the guest's machine.VolumePath;
	// Commit removes them; Destroy removes them.
	Data  *machine.DataVolume
	Cache *machine.CacheVolume
	// CacheFrom is the generation Prepare clones the cache volume from, a
	// directory SaveCache wrote; nil is an empty volume. Only Prepare is
	// given it.
	CacheFrom *Layer
}

// Parent is the committed VM the instance is a copy of. A fresh VM has
// none.
func (s InstanceSpec) Parent() Layer { return s.Chain[len(s.Chain)-1] }

// BootOptions is one boot of a prepared instance.
type BootOptions struct {
	CPUs   int
	Memory uint64
	Shares []machine.Share
	// GUI opens a native window on the guest's display for this boot, in the
	// calling process. A hypervisor without Display refuses it.
	GUI bool
	// Title names the window.
	Title string
	// Console receives the hypervisor's own log of the boot.
	Console io.Writer
}

// MachineViewer is a hypervisor that opens a window on a running machine's
// desktop from any process, given the machine as an attached one reaches it
// (Driver.View).
type MachineViewer interface {
	View(ctx context.Context, m machine.Machine, title string) error
}

// Hypervisor is what runs on this host's disk: hcs, vz, libkrun, and fake.
type Hypervisor interface {
	Name() string
	Capabilities() machine.Capabilities
	Check(ctx context.Context) error
	// Chains reports that a VM's disk differences over the disk of the
	// committed VM it was created from (hcs's VHDX), so that one must
	// outlive the other: the driver gives Prepare, Boot, and Destroy the
	// whole chain, and refuses to destroy a committed VM another VM's disk
	// still needs, or to boot one, which would write to it
	// (machine.ErrInUse). A hypervisor whose copies are disks of
	// their own (vz's APFS clones, fake's trees) reports false.
	Chains() bool
	// Base is the guest OS of the hypervisor's base, what Prepare makes a
	// fresh VM from (machine.Config.Base), or false where it has none (hcs,
	// and vz for macOS guests): then a fresh VM is refused.
	Base() (machine.OS, bool)
	// Install lays a committed VM's disk into dst.Dir from installation
	// media. dst.Dir is the VM's own directory, final.
	Install(ctx context.Context, spec InstallSpec, dst Layer) error
	// Prepare creates the instance's disk in inst.Dir: a copy-on-write copy
	// of its Chain's last committed VM, or, with no Chain, a fresh disk from
	// the hypervisor's base; either boots from power-off with a fresh
	// machine identity.
	Prepare(ctx context.Context, inst InstanceSpec) error
	// Boot starts a prepared instance and returns once its guest operations
	// answer. The returned machine lives until it powers off or is killed; it
	// is not bound to ctx, which only bounds starting it.
	Boot(ctx context.Context, inst InstanceSpec, opts BootOptions) (machine.Machine, error)
	// Commit freezes a stopped instance in place: it removes the instance's
	// volumes and whatever only a running VM needs, and leaves inst.Dir a
	// disk that Prepare copies from (Layer{ID: inst.ID, Dir: inst.Dir}).
	// The caller never boots it again; a boot of it may fail.
	Commit(ctx context.Context, inst InstanceSpec) error
	// MoveVolumes moves a stopped instance's volumes from its old disk, in
	// from.Dir, to its new one, in to.Dir, which Prepare made with no
	// volumes, each at the guest's machine.VolumePath (machine.Driver's
	// Upgrade). An upgrade interrupted at any step calls it again, so it
	// moves what is left and keeps what has moved. A hypervisor that does
	// not list machine.VolumeData returns machine.ErrUnsupported.
	MoveVolumes(ctx context.Context, from, to InstanceSpec) error
	// SaveCache writes a stopped instance's cache volume into dst.Dir, as a
	// generation a later Prepare clones (InstanceSpec.CacheFrom). The
	// instance keeps its volume. A hypervisor that does not list
	// machine.VolumeCache returns machine.ErrUnsupported.
	SaveCache(ctx context.Context, inst InstanceSpec, dst Layer) error
	// Destroy removes everything Prepare and Boot created for the instance.
	Destroy(ctx context.Context, inst InstanceSpec) error
	// Service is where a prepared instance's service listens in its guest
	// (machine.Service.Listen), and the guest port the shim dials it on.
	Service(inst InstanceSpec) (listen string, port uint32, err error)
	// SharesLoopback reports that the hypervisor's guests are processes on
	// the host's own loopback (fake's), so an instance's peers reach its
	// service at the shim's URL, which is then its PrivateURL. One whose
	// guests reach neither each other nor the host's loopback (vz's, hcs's)
	// reports false, and its instances have no PrivateURL.
	SharesLoopback() bool
}
