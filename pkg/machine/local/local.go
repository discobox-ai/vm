// Package local is a machine.Driver whose provider is this host's disk: the
// store and the supervisor that hcs, vz, and the fake driver share. Each of
// them is a Hypervisor, which knows only how to install, clone, boot, and
// commit a disk in a directory it is given; this package keeps everything
// else, in a directory of its own (machine.Config.Dir), the way a Docker
// daemon keeps /var/lib/docker:
//
//	images/layers/<id>/payload/      an image's disk (Layer.Dir)
//	images/layers/<id>/layer.json    the image: project, guest OS, parent
//	images/tags/<project>/<ref>.json a tag, named by its ref in hex
//	instances/<id>/machine/          the hypervisor's instance (InstanceSpec.Dir)
//	instances/<id>/instance.json     the instance: project, name, image, size
//	instances/<id>/shim.json         its shim, while one holds it
//	instances/.names/<project>/<name> the name's claim, holding the ID
//	cache/<hypervisor>/              media the hypervisor downloads
//
// Nothing is kept anywhere else, and nothing needs a lock: an image is
// published by renaming its directory into place, a tag by renaming its file,
// and a name is claimed by creating its file exclusively.
//
// A Virtualization.framework machine dies with the process that booted it,
// and an HCS machine belongs to vmcompute, so Start runs each instance under
// a shim process of its own (ShimMain), which boots it, holds it, and serves
// its guest operations, any guest port, and its forwards on an owner-only
// Unix socket, and its URL on the host's loopback. Any process of the owner's
// attaches to it there (Attach).
package local

import (
	"context"
	"io"

	"github.com/discobox-ai/vm/pkg/machine"
)

// Layer is a committed image's disk, in the directory the hypervisor wrote it
// to: one link of a chain, for a hypervisor that Chains. Dir is final once
// the image is committed, but an image is written under a temporary name and
// renamed into place, so a hypervisor must not record the image's own Dir
// inside its files. Recording a parent's Dir (a differencing disk's parent
// path) is fine: a parent is final before its child is written.
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
	// Chain is the instance's image last, after the images its disk
	// differences over, for a hypervisor that Chains; a hypervisor that does
	// not gets the image alone. It is empty when the instance runs from
	// Base.
	Chain []Layer
	// Base is the hypervisor's base the instance runs from instead of an
	// image: one Bases listed.
	Base *machine.Base
	// CacheDir is where the hypervisor caches what it downloads, which
	// Prepare may fill from a base (vz pulls a Linux guest image there). It
	// may not exist yet.
	CacheDir string
	// Egress is the fence the instance was created with, which its record
	// keeps: nil is none. A hypervisor holds it at every boot (vz: an empty
	// Allow is no network device).
	Egress *machine.Egress
}

// Parent is the image the instance is a clone of. An instance of a Base has
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

// Hypervisor is what runs on this host's disk: hcs, vz, and fake.
type Hypervisor interface {
	Name() string
	Capabilities() machine.Capabilities
	Check(ctx context.Context) error
	// Chains reports that a committed image's disk differences over the disk
	// of the image its instance ran from (hcs's VHDX), so that one must
	// outlive the other: the driver records the parent, gives Prepare, Boot,
	// and Destroy the whole chain, and refuses to delete a disk another
	// image's still needs. A hypervisor whose images are copies of their own
	// (vz's APFS clones, fake's trees) reports false.
	Chains() bool
	// Install creates an image in dst.Dir from installation media.
	Install(ctx context.Context, spec InstallSpec, dst Layer) error
	// Prepare creates the instance's disk from its chain in inst.Dir: a
	// copy-on-write clone of the image that boots from power-off with a fresh
	// machine identity.
	Prepare(ctx context.Context, inst InstanceSpec) error
	// Boot starts a prepared instance and returns once its guest operations
	// answer. The returned machine lives until it powers off or is killed; it
	// is not bound to ctx, which only bounds starting it.
	Boot(ctx context.Context, inst InstanceSpec, opts BootOptions) (machine.Machine, error)
	// Commit captures a stopped instance's disk as the new image dst. The
	// instance is destroyed afterwards, so a hypervisor may move rather than
	// copy.
	Commit(ctx context.Context, inst InstanceSpec, dst Layer) error
	// Destroy removes everything Prepare and Boot created for the instance.
	Destroy(ctx context.Context, inst InstanceSpec) error
	// Bases lists what the hypervisor already has that an instance can run
	// from with no image, given the directory it caches what it downloads
	// in.
	Bases(ctx context.Context, cacheDir string) ([]machine.Base, error)
	// Base is the base ref names, by its name or ID, or found false. A
	// hypervisor may know bases it does not list (vz: any reference to a
	// Linux guest image, which Prepare pulls into cacheDir). It never pulls
	// or makes anything itself.
	Base(ctx context.Context, cacheDir, ref string) (base machine.Base, found bool, err error)
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
