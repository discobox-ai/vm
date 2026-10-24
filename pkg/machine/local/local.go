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
// its guest operations, its URL, and its forwards on the host's loopback. Any
// process attaches to it there (Attach).
package local

import (
	"context"
	"io"
	"net"

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

// Machine is a running guest whose ports the host can reach, which the shim
// serves the instance's URL through.
type Machine interface {
	machine.Machine
	// Dial opens a stream to a guest port: vsock on vz, hvsocket on HCS with
	// the service ID derived from the port by the vsock template, the host's
	// loopback on fake.
	Dial(ctx context.Context, port uint32) (net.Conn, error)
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
	Boot(ctx context.Context, inst InstanceSpec, opts BootOptions) (Machine, error)
	// Commit captures a stopped instance's disk as the new image dst. The
	// instance is destroyed afterwards, so a hypervisor may move rather than
	// copy.
	Commit(ctx context.Context, inst InstanceSpec, dst Layer) error
	// Destroy removes everything Prepare and Boot created for the instance.
	Destroy(ctx context.Context, inst InstanceSpec) error
	// Bases lists what the hypervisor already has that an instance can run
	// from with no image.
	Bases(ctx context.Context) ([]machine.Base, error)
	// Service is where a prepared instance's service listens in its guest
	// (machine.Service.Listen), and the guest port the shim dials it on.
	Service(inst InstanceSpec) (listen string, port uint32, err error)
}
