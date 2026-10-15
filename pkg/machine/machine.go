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
	"fmt"
	"io"
	"net"
	"slices"
	"strings"
	"time"
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
	// Auto asks the engine for the fastest mode the image is warm for, and
	// cold when it is warm for none. The engine resolves it before Prepare, so
	// a driver never sees it.
	Auto CloneMode = "auto"
	// Cold copies the disk copy-on-write and boots it from power-off with a
	// fresh machine identity. Every driver supports it.
	Cold CloneMode = "cold"
	// Resume copies the disk and a saved memory state, and resumes the saved
	// machine. The clone runs with the saved machine's identity, which is why a
	// vz stage keeps one template per guest that can run at once; a boxd
	// snapshot restores any number of times. It needs a warm image.
	Resume CloneMode = "resume"
	// Fork clones a live, paused template: memory is shared copy-on-write and
	// many clones can be forked from one template (HCS). It needs a warm image.
	Fork CloneMode = "fork"
)

// Capabilities is what a driver can do on this host. It is reported, never
// assumed, so that pools, the builder, and a discobox provider plan around the
// real limits of each hypervisor.
type Capabilities struct {
	// GuestOS is every guest OS this driver can install and boot.
	GuestOS []OS `json:"guestOS"`
	// CloneModes is every mode Prepare accepts. Cold is always present; any
	// other mode needs a warm image, so a driver that lists one implements
	// Warmer.
	CloneModes []CloneMode `json:"cloneModes"`
	// MaxRunning caps concurrently running guests per guest OS. Zero means no
	// cap. Virtualization.framework allows two macOS guests per host.
	MaxRunning map[OS]int `json:"maxRunning,omitempty"`
	// SharedDirectories reports host directory sharing (virtiofs, Plan9).
	SharedDirectories bool `json:"sharedDirectories"`
	// Display reports a viewable guest framebuffer.
	Display bool `json:"display"`
	// Forward reports that guest processes can connect out to the host: a
	// running Machine implements HostListener.
	Forward bool `json:"forward"`
	// Remote reports that a running machine is owned by a service, not by
	// the process that booted it (boxd's API), so any process can reach it
	// again: the driver implements Attacher, and the engine runs no shim for
	// its instances. A driver that does not list it returns machines that
	// implement Dialer.
	Remote bool `json:"remote"`
	// Agent reports that Install bakes the disco-vm agent into the guest
	// (InstallSpec.Agent), because the driver reaches the guest's operations
	// through it. boxd reaches them through its own API, and needs none.
	Agent bool `json:"agent"`
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
	// Project is the layer's project, which a driver puts in the name of
	// whatever it keeps for the layer outside Dir.
	Project string
}

// DefaultProject is the project of everything made without one named, and of
// everything made before there were projects.
const DefaultProject = "default"

// ValidateProject checks a project name: 1 to 32 lowercase letters, digits,
// and single hyphens, starting and ending with a letter or digit. That fits
// in a DNS label beside the rest of a resource's name, as a boxd machine's
// name must, and a driver can end it with "--" and split it back off.
func ValidateProject(project string) error {
	ok := len(project) >= 1 && len(project) <= 32 &&
		!strings.HasPrefix(project, "-") && !strings.HasSuffix(project, "-") && !strings.Contains(project, "--")
	for _, r := range project {
		if (r < 'a' || r > 'z') && (r < '0' || r > '9') && r != '-' {
			ok = false
		}
	}
	if !ok {
		return fmt.Errorf("machine: project %q is not 1 to 32 lowercase letters, digits, and single hyphens, starting and ending with a letter or digit", project)
	}
	return nil
}

// ResourcePrefix is how a driver starts the name of everything it makes for
// a project, after its own prefix (boxd's "dvm-"): the project and "--". The
// default project's names before projects had no project in them, so
// ResourceProject reads such a name as the default project's.
// An empty project is the default.
func ResourcePrefix(prefix, project string) string {
	if project == "" {
		project = DefaultProject
	}
	return prefix + project + "--"
}

// ResourceProject is the project a name made with ResourcePrefix belongs to:
// the default project for a name under prefix with no "--" in it, and "" for
// a name that is not under prefix at all.
func ResourceProject(prefix, name string) string {
	rest, ok := strings.CutPrefix(name, prefix)
	if !ok {
		return ""
	}
	project, _, ok := strings.Cut(rest, "--")
	if !ok {
		return DefaultProject
	}
	return project
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
	// Agent is the disco-vm binary built for GuestOS, given to a driver that
	// lists Capabilities.Agent. The driver bakes it into the image and
	// arranges for `disco-vm guest` to start at boot, as a SYSTEM service on
	// Windows and a launchd daemon on macOS.
	Agent string
	// Options carries driver-specific settings the spec passes through
	// untouched, such as a product key or a locale.
	Options map[string]string
	// CacheDir is the driver's to keep media it downloads (Media "latest")
	// across installs, so a 20 GB restore image is fetched once. It may not
	// exist yet.
	CacheDir string
	// GUI opens a window on the guest's display for every boot the install
	// makes. Drivers without Display ignore it.
	GUI bool
	// Log receives human-readable install progress.
	Log io.Writer
}

// InstanceSpec is one instance as a driver sees it.
type InstanceSpec struct {
	ID string
	// Project is the instance's project, as Layer.Project.
	Project string
	// Dir is private to the driver for this instance, and empty on Prepare.
	Dir     string
	GuestOS OS
	// Chain is the instance's image, base layer first and parent layer last.
	Chain []Layer
	Mode  CloneMode
	// WarmDir is the image's stage (WarmSpec.Dir) when Mode is not cold. Only
	// Prepare and the first Boot after it draw on the stage; a later boot of
	// the same instance is cold.
	WarmDir string
}

// Service is an instance's one service: where a program in the guest listens,
// and where the driver exposes it. The driver picks it, never the caller or
// the image, and it is the same for every boot of a prepared instance.
type Service struct {
	// Listen is the address a guest program listens on, in guest.Listen's
	// form: vsock:PORT (AF_VSOCK on vz, the hvsocket service ID of the port
	// on hcs), tcp:ADDR. The engine writes it to the guest's
	// guest.ServiceFile at every start.
	Listen string
	// Port is the guest port Dialer.Dial reaches the service on.
	Port uint32
	// URL is where any process reaches the service over HTTP with no
	// credential of the driver's (boxd's https://<machine>.boxd.sh, a
	// port the Docker daemon publishes on the host's loopback). It is empty
	// when the engine's shim serves the service on the host's loopback
	// instead, by dialing Port.
	URL string
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
	// GUI opens a native window on the guest's display for this boot, in the
	// calling process. Drivers without Display ignore it.
	GUI bool
	// Title names the window.
	Title string
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
	// Boot starts a prepared instance and returns once its guest operations
	// answer. The returned machine lives until it powers off or is killed; it
	// is not bound to ctx, which only bounds starting it.
	Boot(ctx context.Context, inst InstanceSpec, opts BootOptions) (Machine, error)
	// Commit captures a stopped instance's disk as the new layer dst. The
	// instance is destroyed afterwards, so a driver may move rather than copy.
	Commit(ctx context.Context, inst InstanceSpec, dst Layer) error
	// Destroy removes everything Prepare and Boot created for the instance.
	Destroy(ctx context.Context, inst InstanceSpec) error
	// DeleteLayer releases anything the driver holds for a layer beyond its
	// Dir, which the store removes itself.
	DeleteLayer(ctx context.Context, layer Layer) error
	// LayerExists reports whether what the driver holds for a layer beyond
	// its Dir is still there. A boxd snapshot or a daemon's image can be
	// deleted behind the store's back, and the store then drops the layer so
	// that a build makes it again. A driver whose layers live only in Dir
	// reports true.
	LayerExists(ctx context.Context, layer Layer) (bool, error)
	// TagLayer records that ref, a normalized name:tag, names layer, where
	// the driver keeps layers somewhere a person browses, so that what is
	// there says what it is for (boxd: a snapshot named after the layer and
	// the tag). Tagging the same layer and ref again is not an error. A
	// driver whose layers live only in the store does nothing.
	TagLayer(ctx context.Context, layer Layer, ref string) error
	// UntagLayer drops what TagLayer recorded for ref on layer. A tag it
	// never recorded is not an error.
	UntagLayer(ctx context.Context, layer Layer, ref string) error
	// Orphans lists what the driver holds outside the store, under the names
	// it gives what it makes (boxd's dvm- machines and snapshots, the daemon's
	// labeled containers and disco-vm-layer images), that nothing in known
	// accounts for: what a deleted store, a crash between making something
	// and recording it, or another host's store left there. Something being
	// made right now (an install's machine, a tag's snapshot) is one until it
	// is recorded or released. It reads the service on every call, never a
	// cache. A driver whose layers and instances live only in their Dirs
	// returns none.
	Orphans(ctx context.Context, known Known) ([]Orphan, error)
	// RemoveOrphan deletes one resource Orphans listed: destroys the machine,
	// or deletes the snapshot or image. The caller passes only what Orphans
	// returned for its project just before. Something already gone is not an
	// error.
	RemoveOrphan(ctx context.Context, orphan Orphan) error
	// Service is the prepared instance's service. A URL the driver gives
	// may exist only while the instance runs (a port the daemon publishes at
	// each start); asked for one while it is stopped, the driver returns
	// ErrNotRunning. A URL that outlives a stop (boxd's) is the same across
	// stop and start.
	Service(ctx context.Context, inst InstanceSpec) (Service, error)
}

// Known is everything a store holds metadata for: what Orphans leaves out. A
// driver reads what it recorded in each Dir to learn which of its resources
// these are.
type Known struct {
	// Project is the project Orphans lists: what the driver made for others
	// is theirs, not an orphan of this one.
	Project string
	// Instances carry ID and Dir, the build's temporary instances included.
	Instances []InstanceSpec
	Layers    []Layer
	// Tags are the store's tags, each on its layer, as TagLayer was given
	// them.
	Tags []Tag
	// Stages are the Dirs of every warm stage (WarmSpec.Dir).
	Stages []string
}

// Tag is one tag a layer carries in the store.
type Tag struct {
	Layer Layer
	Ref   string
}

// OrphanKind says which list an orphan belongs in.
type OrphanKind string

const (
	// OrphanMachine is a machine or container, listed beside instances.
	OrphanMachine OrphanKind = "machine"
	// OrphanLayer is a snapshot or image, listed beside images.
	OrphanLayer OrphanKind = "layer"
)

// Orphan is one resource a driver holds that the store does not know.
type Orphan struct {
	Kind OrphanKind `json:"kind"`
	// Name is what the service calls it, and what `disco-vm rm` and `rmi`
	// take to remove it.
	Name string `json:"name"`
	ID   string `json:"id"`
	// Status is the service's word for its state: running, stopped, ready.
	Status  string    `json:"status,omitempty"`
	Created time.Time `json:"created,omitzero"`
}

// ErrNotWarm reports that nothing is staged for the clone mode Prepare was
// asked for: the image was never warmed, its stage was used up, or the stage
// was rejected (a saved state from before a host update). The engine answers
// it by cloning cold when the mode was auto.
var ErrNotWarm = errors.New("machine: the image is not warm for this clone mode")

// WarmSpec is one image's stage: what Warm builds and what a warm Prepare
// draws on.
type WarmSpec struct {
	GuestOS OS
	// Chain is the image, base layer first, as in InstanceSpec.
	Chain []Layer
	// Dir is private to the driver for this image's stage. It exists, and it
	// is the same directory for Warm, Warmth, Cool, and InstanceSpec.WarmDir.
	Dir string
	// Count is how many clones to stage where a stage is a pool used once per
	// clone (fake)
	// (resume). Warm tops an existing stage up to Count. A mode whose stage
	// serves any number of clones (fork) ignores it.
	Count int
	// CPUs and Memory size the staged machine; zero is the driver's default.
	// A clone of a stage runs at the stage's size.
	CPUs   int
	Memory uint64
	// User, when set, is an account created in the stage and logged in on its
	// display, so every clone of the stage starts as that user. A driver that
	// cannot create one returns ErrUnsupported.
	User *User
	// Log receives human-readable progress.
	Log io.Writer
}

// User is a guest account a driver creates.
type User struct {
	Name     string `json:"name"`
	FullName string `json:"fullName,omitempty"`
	// UID is the account's numeric ID where the guest OS has one (macOS), so
	// that it can match the host's; zero lets the guest pick.
	UID int `json:"uid,omitempty"`
}

// Warmth is what a stage can serve now.
type Warmth struct {
	// Mode is the clone mode the stage serves, or Cold when nothing is staged.
	Mode CloneMode `json:"mode"`
	// Clones is how many Prepares the stage can serve: zero when nothing is
	// staged, and -1 when it serves any number (a fork template).
	Clones int `json:"clones"`
	// Held says why the stage cannot serve as it should right now: a vz
	// restore needs the Mac's screen unlocked. With Clones zero nothing is
	// served; otherwise clones are served some slower way (vz boots them cold
	// from the stage).
	Held string `json:"held,omitempty"`
}

// Warmer is a driver that can stage an image so that clones skip the cold
// boot: vz saves machine states to resume, and HCS freezes a live template to
// fork. Every driver that lists a clone mode other than cold implements it.
type Warmer interface {
	// Warm stages spec.Chain in spec.Dir and returns once the stage is
	// usable. The driver picks the mode, the fastest it has.
	//
	// A stage that lives on disk (saved states) returns a nil Stage. A stage
	// that lives in memory (an HCS template) returns one, and the caller keeps
	// its process alive for as long as the image stays warm: the engine runs
	// Warm in a warm shim for exactly this.
	Warm(ctx context.Context, spec WarmSpec) (Stage, error)
	// Warmth reports what the stage can serve now. It is cheap and changes
	// nothing; a stage that is gone or stale reports zero.
	Warmth(ctx context.Context, spec WarmSpec) (Warmth, error)
	// Cool releases everything Warm staged. A resident stage has already been
	// closed.
	Cool(ctx context.Context, spec WarmSpec) error
}

// Stage is a warm stage that lives in memory.
type Stage interface {
	// Done is closed once the stage is gone.
	Done() <-chan struct{}
	// Close releases the stage and waits for it to be gone.
	Close(ctx context.Context) error
}

// ErrNotRunning reports that an instance has no running machine to attach to.
var ErrNotRunning = errors.New("machine: the instance is not running")

// Attacher is a driver whose running machines outlive the process that booted
// them, because a service owns them. Every driver that lists
// Capabilities.Remote implements it.
type Attacher interface {
	// Attach returns the running machine of a booted instance, or
	// ErrNotRunning. Any process may call it, any number of times, with its
	// own credentials. A guest that shut itself down while nothing watched
	// is finished here, as Done would have finished it, and reads as not
	// running. The machine watches for its own stop only once Done, Err, or
	// Kill is called, so attaching to run a command costs nothing that
	// outlives the call.
	Attach(ctx context.Context, inst InstanceSpec) (Machine, error)
}

// HostListener is a running machine whose guest processes can connect out to
// the host. A driver that lists Capabilities.Forward returns machines that
// implement it.
type HostListener interface {
	// Listen accepts the connections guest processes open to the host on a
	// port: AF_VSOCK to the host (CID 2) on vz, hvsocket to the parent
	// partition on HCS. A guest dials it with guest.DialHost. It lasts until
	// it is closed or the machine stops.
	Listen(port uint32) (net.Listener, error)
}

// Machine is a running guest. Boot returns one whose guest operations
// answer. Attach returns one that is running, which may still be starting
// (a boxd machine starting, a guest that rebooted), so its operations can
// fail until it has.
type Machine interface {
	Guest
	// Kill powers the guest off immediately. An orderly shutdown goes through
	// Shutdown instead; Kill is the fallback.
	Kill(ctx context.Context) error
	// Done is closed once the guest has stopped, or once the driver can no
	// longer tell: a remote driver whose API stops answering gives the
	// machine up rather than hold its caller forever. Err is non-nil then,
	// and the guest may still be running.
	Done() <-chan struct{}
	// Err is why the guest stopped, once Done is closed; nil for a power-off.
	// It is non-nil when the driver gave the machine up without seeing it stop.
	Err() error
}
