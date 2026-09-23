// Package machine is the seam between disco-vm and a hypervisor.
//
// A Driver is the only code in disco-vm that knows which hypervisor it talks
// to. Everything above it — the image store, the builder, the engine, the CLI,
// and a discobox provider — speaks only the types in this package, so one
// program runs on Windows (hcs) and macOS (vz) with nothing but the driver
// differing.
//
// The contract is the intersection of what Host Compute Service and
// Virtualization.framework can both do honestly. Where they differ — a live
// template that forks many clones exists only on HCS, a saved state that
// resumes once exists only on vz, and macOS guests are capped at two per host —
// the difference is reported through Capabilities rather than faked. A caller
// that needs a capability checks for it; a driver that lacks one returns
// ErrUnsupported instead of emulating it badly.
package machine

import (
	"context"
	"errors"
	"io"
	"net"
	"slices"
)

// ErrUnsupported reports that a driver does not offer a capability at all, on
// any host. Callers are expected to have checked Capabilities first.
var ErrUnsupported = errors.New("machine: not supported by this driver")

// ErrNotImplemented reports driver work that is planned but not written yet.
// It is distinct from ErrUnsupported so that "never" and "not yet" read
// differently in an error message and in a test.
var ErrNotImplemented = errors.New("machine: not implemented yet")

// OS is a guest operating system. It uses GOOS spelling, because the guest
// agent is the disco-vm binary built for that GOOS.
type OS string

const (
	Windows OS = "windows"
	Darwin  OS = "darwin"
	Linux   OS = "linux"
)

// CloneMode is how an instance's disk and state are derived from its image.
type CloneMode string

const (
	// Cold copies the disk copy-on-write and boots it from power-off with a
	// fresh machine identity. Every driver supports it.
	Cold CloneMode = "cold"
	// Resume copies the disk and a saved memory state, and resumes the saved
	// machine. The clone keeps the saved machine's identity, so each saved
	// state is meant to be resumed once (vz).
	Resume CloneMode = "resume"
	// Fork clones a live, paused template: memory is shared copy-on-write and
	// many clones can be forked from one template (HCS).
	Fork CloneMode = "fork"
)

// Capabilities is what a driver can do on this host. It is reported, never
// assumed, so that pools, the builder, and a discobox provider plan around the
// real limits of each hypervisor.
type Capabilities struct {
	// GuestOS is every guest OS this driver can install and boot.
	GuestOS []OS `json:"guestOS"`
	// CloneModes is every mode Prepare accepts. Cold is always present.
	CloneModes []CloneMode `json:"cloneModes"`
	// MaxRunning caps concurrently running guests per guest OS. Zero means no
	// cap. Virtualization.framework allows two macOS guests per host.
	MaxRunning map[OS]int `json:"maxRunning,omitempty"`
	// SharedDirectories reports host directory sharing (virtiofs, Plan9).
	SharedDirectories bool `json:"sharedDirectories"`
	// Display reports a viewable guest framebuffer.
	Display bool `json:"display"`
}

// SupportsOS reports whether the driver can run the given guest OS.
func (c Capabilities) SupportsOS(os OS) bool { return slices.Contains(c.GuestOS, os) }

// SupportsMode reports whether Prepare accepts the given clone mode.
func (c Capabilities) SupportsMode(mode CloneMode) bool {
	return mode == "" || slices.Contains(c.CloneModes, mode)
}

// Layer is one immutable, committed disk state in the image store. Dir belongs
// to the driver: it holds whatever the driver needs to boot or derive from this
// layer (a differencing VHDX, an APFS-cloned macOS bundle, a directory tree).
//
// Dir is final once the layer is committed, but a layer is written under a
// temporary name and renamed into place, so a driver must not record the
// layer's own Dir inside the layer's files. Recording a parent's Dir (a
// differencing disk's parent path) is fine: parents are final before children
// are written.
type Layer struct {
	ID  string
	Dir string
}

// InstallSpec creates a base layer from installation media.
type InstallSpec struct {
	GuestOS OS
	// Media is the installer: a Windows ISO, a macOS IPSW, or "latest" where
	// the driver can fetch one itself.
	Media string
	// Edition selects an image inside multi-image media (install.wim).
	Edition   string
	DiskBytes int64
	CPUs      int
	Memory    uint64
	// Agent is the disco-vm binary built for GuestOS. The driver bakes it into
	// the image and arranges for `disco-vm guest` to start at boot, as a
	// SYSTEM service on Windows and a launchd daemon on macOS.
	Agent string
	// Options carries driver-specific settings the spec passes through
	// untouched, such as a product key or a locale.
	Options map[string]string
	// Log receives human-readable install progress.
	Log io.Writer
}

// InstanceSpec is one instance as a driver sees it.
type InstanceSpec struct {
	ID string
	// Dir is private to the driver for this instance, and empty on Prepare.
	Dir     string
	GuestOS OS
	// Chain is the instance's image, base layer first and parent layer last.
	Chain []Layer
	Mode  CloneMode
}

// Parent is the layer the instance is derived from.
func (s InstanceSpec) Parent() Layer { return s.Chain[len(s.Chain)-1] }

// Share is a host directory exported to the guest.
type Share struct {
	Tag      string
	HostPath string
	ReadOnly bool
}

// BootOptions is one boot of a prepared instance.
type BootOptions struct {
	CPUs   int
	Memory uint64
	Shares []Share
	// GUI asks for the guest's display. Drivers without Display ignore it.
	GUI bool
	// Console receives the driver's own log of the boot, not guest output.
	Console io.Writer
}

// Driver is one hypervisor.
type Driver interface {
	Name() string
	Capabilities() Capabilities
	// Check reports whether this host can run the driver: the hypervisor
	// feature, privileges, an entitlement. It should name the fix.
	Check(ctx context.Context) error
	// Install creates a base layer in dst.Dir from installation media.
	Install(ctx context.Context, spec InstallSpec, dst Layer) error
	// Prepare creates the instance's disk from its chain in inst.Dir.
	Prepare(ctx context.Context, inst InstanceSpec) error
	// Boot starts a prepared instance. The returned machine lives until it
	// powers off or is killed; it is not bound to ctx, which only bounds
	// starting it.
	Boot(ctx context.Context, inst InstanceSpec, opts BootOptions) (Machine, error)
	// Commit captures a stopped instance's disk as the new layer dst. The
	// instance is destroyed afterwards, so a driver may move rather than copy.
	Commit(ctx context.Context, inst InstanceSpec, dst Layer) error
	// Destroy removes everything Prepare and Boot created for the instance.
	Destroy(ctx context.Context, inst InstanceSpec) error
	// DeleteLayer releases anything the driver holds for a layer beyond its
	// Dir, which the store removes itself.
	DeleteLayer(ctx context.Context, layer Layer) error
}

// Machine is a running guest.
type Machine interface {
	// Dial opens a stream to a guest port: vsock on vz, hvsocket on HCS with
	// the service ID derived from the port by the vsock template.
	Dial(ctx context.Context, port uint32) (net.Conn, error)
	// Kill powers the guest off immediately. An orderly shutdown goes through
	// the guest agent instead; Kill is the fallback.
	Kill(ctx context.Context) error
	// Done is closed once the guest has stopped.
	Done() <-chan struct{}
	// Err is why the guest stopped, once Done is closed; nil for a power-off.
	Err() error
}
