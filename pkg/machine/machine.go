// Package machine is the seam between disco-vm and a hypervisor.
//
// A Driver is the only code in disco-vm that knows which hypervisor it talks
// to. Everything above it — the engine, the CLI, and a discobox provider —
// speaks only the types in this package, so one program runs on
// Windows (hcs) and macOS (vz) with nothing but the driver differing.
//
// A driver is also the only place anything is kept (ADR 26-10-23-417). The
// engine holds no state: every image, tag, and instance is read from the
// driver, which reads it from its provider every time, and keeps what it knows
// about each on the provider's own resources, in the provider's way: labels on
// Docker's images and containers, the names of boxd's snapshots and machines,
// and files beside each disk in the directory of a driver whose provider is
// this host's disk (hcs, vz, fake; package local).
//
// The contract is the intersection of what the hypervisors can do honestly.
// Where they differ — host directory sharing, a display, and macOS guests
// capped at two per host — the difference is reported through Capabilities
// rather than faked. A caller that needs a capability checks for it; a driver
// that lacks one returns ErrUnsupported instead of emulating it badly.
package machine

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"path/filepath"
	"runtime"
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

// ErrNotFound reports an image, a tag, or an instance the provider does not
// have.
var ErrNotFound = errors.New("not found")

// ErrExists reports an instance name the provider already has in the project.
var ErrExists = errors.New("already exists")

// ErrInUse reports an image the driver will not delete because another
// image's disk still needs it (Driver.DeleteImage).
var ErrInUse = errors.New("its disk is still another image's")

// ErrNotRunning reports that an instance has no running machine to attach to.
var ErrNotRunning = errors.New("machine: the instance is not running")

// OS is a guest operating system. It uses GOOS spelling, because the guest
// agent is the disco-vm binary built for that GOOS.
type OS string

const (
	Windows OS = "windows"
	Darwin  OS = "darwin"
	Linux   OS = "linux"
)

// Capabilities is what a driver can do on this host. It is reported, never
// assumed, so that pools and a discobox provider plan around the real limits
// of each hypervisor.
type Capabilities struct {
	// GuestOS is every guest OS this driver can boot.
	GuestOS []OS `json:"guestOS"`
	// MaxRunning caps concurrently running guests per guest OS. Zero means no
	// cap. Virtualization.framework allows two macOS guests per host.
	MaxRunning map[OS]int `json:"maxRunning,omitempty"`
	// SharedDirectories reports host directory sharing (virtiofs, Plan9).
	SharedDirectories bool `json:"sharedDirectories"`
	// Display reports a viewable guest framebuffer.
	Display bool `json:"display"`
	// Forward reports that guest processes can connect out to the host: an
	// instance created with Forwards has them served whenever it runs.
	Forward bool `json:"forward"`
	// Agent reports that the driver puts the disco-vm agent in the guest and
	// reaches the guest's operations through it: Install bakes it in
	// (InstallSpec.Agent) on hcs and vz, and docker adds it to a base's image
	// at Create. boxd reaches them through its own API, and needs none.
	Agent bool `json:"agent"`
	// Install reports that the driver makes an image from installation
	// media (Install). One that does not has bases instead (Bases, Base).
	Install bool `json:"install"`
}

// SupportsOS reports whether the driver can run the given guest OS.
func (c Capabilities) SupportsOS(os OS) bool { return slices.Contains(c.GuestOS, os) }

// Config is what a driver is made with.
type Config struct {
	// Project scopes every image, tag, and instance the driver lists and
	// makes: it is in the name of each, or on its label. Empty is
	// DefaultProject.
	Project string
	// Dir is where a driver whose provider is this host's disk (hcs, vz,
	// fake) keeps its images, instances, and the media it downloads. Empty
	// is DefaultDir(). A driver whose provider is a service (boxd, docker)
	// keeps nothing on the host and ignores it.
	Dir string
	// ShimCommand starts the process that holds a running instance, for a
	// driver whose machine dies with the process that booted it (vz) or
	// belongs to the host (hcs): a program and its leading arguments, to
	// which the shim's own are appended. Empty is this executable's `shim`
	// subcommand. A program that embeds the engine points it at a
	// subcommand of its own that calls local.ShimMain, so it ships no second
	// binary.
	ShimCommand []string
}

// DirEnv overrides DefaultDir.
const DirEnv = "DISCO_VM_ROOT"

// DefaultDir is where a driver whose provider is this host's disk keeps its
// state: $DISCO_VM_ROOT, or %LOCALAPPDATA%\disco-vm on Windows,
// ~/Library/Application Support/disco-vm on macOS, and
// $XDG_DATA_HOME/disco-vm elsewhere.
func DefaultDir() string {
	if dir := os.Getenv(DirEnv); dir != "" {
		return dir
	}
	switch runtime.GOOS {
	case "windows":
		if dir := os.Getenv("LOCALAPPDATA"); dir != "" {
			return filepath.Join(dir, "disco-vm")
		}
	case "darwin":
		if home, err := os.UserHomeDir(); err == nil {
			return filepath.Join(home, "Library", "Application Support", "disco-vm")
		}
	default:
		if dir := os.Getenv("XDG_DATA_HOME"); dir != "" {
			return filepath.Join(dir, "disco-vm")
		}
		if home, err := os.UserHomeDir(); err == nil {
			return filepath.Join(home, ".local", "share", "disco-vm")
		}
	}
	return filepath.Join(os.TempDir(), "disco-vm")
}

// DefaultProject is the project of everything made without one named, and of
// everything made before there were projects.
const DefaultProject = "default"

// ValidateProject checks a project name: 1 to 32 lowercase letters, digits,
// and single hyphens, starting and ending with a letter or digit. That fits
// in a DNS label beside the rest of a resource's name, as a boxd machine's
// name must, and a driver can end it with "--" and split it back off.
func ValidateProject(project string) error {
	if !label(project, 32) || strings.Contains(project, "--") {
		return fmt.Errorf("machine: project %q is not 1 to 32 lowercase letters, digits, and single hyphens, starting and ending with a letter or digit", project)
	}
	return nil
}

// ValidateName checks an instance's name: 1 to 63 lowercase letters, digits,
// and single hyphens, starting and ending with a letter or digit. It is in
// the name of what the driver makes for the instance (a boxd machine, a
// container), which must be a DNS label in a proxy's domain, so a driver may
// refuse a name that is valid here but too long beside its project.
func ValidateName(name string) error {
	if !label(name, 63) || strings.Contains(name, "--") {
		return fmt.Errorf("machine: instance name %q is not 1 to 63 lowercase letters, digits, and single hyphens, starting and ending with a letter or digit", name)
	}
	return nil
}

// label reports whether s is 1 to n lowercase letters, digits, and hyphens,
// starting and ending with a letter or digit.
func label(s string, n int) bool {
	if len(s) < 1 || len(s) > n || strings.HasPrefix(s, "-") || strings.HasSuffix(s, "-") {
		return false
	}
	for _, r := range s {
		if (r < 'a' || r > 'z') && (r < '0' || r > '9') && r != '-' {
			return false
		}
	}
	return true
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

// Image is one immutable, committed disk state: an install from media, or a
// snapshot of a stopped instance (ADR 26-10-23-410). It is a differencing
// VHDX, an APFS-cloned macOS bundle, a boxd snapshot, a Docker image. Above
// the seam every image stands alone: it has no parent, an instance runs from
// one image, and removing one never touches another. A driver whose disks
// chain below the seam (an hcs snapshot differences over the disk of the
// image its instance ran from) keeps the chain to itself. Its ID is random:
// nothing reproducible made it.
type Image struct {
	ID      string `json:"id"`
	GuestOS OS     `json:"guestOS"`
	// Comment says what made the image: "install windows", "snapshot dev".
	// A driver that cannot keep it reports none.
	Comment string    `json:"comment,omitempty"`
	Created time.Time `json:"created,omitzero"`
}

// Tag is one name:tag, normalized, and the image it names.
type Tag struct {
	Ref   string `json:"ref"`
	Image string `json:"image"`
}

// Base is something a driver already has that an instance can run from with
// no image.
type Base struct {
	// Name is what `images` lists, and a reference may name the base by it.
	Name string `json:"name"`
	// ID is the service's own ID for it, which a reference may name too.
	ID      string `json:"id"`
	GuestOS OS     `json:"guestOS"`
	// Options are the driver's own: what it makes a machine from (boxd: an
	// image or a snapshot; docker: an image).
	Options map[string]string `json:"options,omitempty"`
	Created time.Time         `json:"created,omitzero"`
}

// Forward sends the connections a guest process opens to the host on a port
// (guest.DialHost: vsock to the host on vz, hvsocket on HCS) to a Unix socket
// on the host, which something there serves. It is how a guest reaches a host
// service without a TCP listener, as libkrun maps a vsock port to a Unix
// socket.
type Forward struct {
	Port   uint32 `json:"port"`
	Socket string `json:"socket"`
}

// InstanceSpec is a new instance.
type InstanceSpec struct {
	// Name is unique in the project (ValidateName).
	Name string
	// Image is the reference it was asked for by, kept to be shown.
	Image string
	// ImageID is the image it is a clone of, or empty for an instance of
	// Base.
	ImageID string
	// Base is the driver's base it runs from instead of an image: one Bases
	// listed or Base found.
	Base    *Base
	GuestOS OS
	// CPUs and Memory size every boot; zero is the driver's default.
	CPUs   int
	Memory uint64
	// Forwards are served whenever the instance runs, by a driver that lists
	// Capabilities.Forward.
	Forwards []Forward
}

// Instance is one machine made from an image or a base, as its provider has
// it.
type Instance struct {
	// ID is the provider's own: a container's ID, a boxd machine's, the
	// directory of a local driver's instance.
	ID   string `json:"id"`
	Name string `json:"name"`
	// Image is the reference it was made from, or what the provider says it
	// was made from when it kept no reference.
	Image string `json:"image"`
	// ImageID is its image, or empty for an instance of a base, or where the
	// provider does not say.
	ImageID string    `json:"imageID,omitempty"`
	Base    *Base     `json:"base,omitempty"`
	GuestOS OS        `json:"guestOS"`
	CPUs    int       `json:"cpus,omitempty"`
	Memory  uint64    `json:"memory,omitempty"`
	Created time.Time `json:"created,omitzero"`
	// Forwards are served whenever it runs.
	Forwards []Forward `json:"forwards,omitempty"`
}

// InstallSpec creates an image from installation media.
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
	// GUI opens a window on the guest's display for every boot the install
	// makes. Drivers without Display ignore it.
	GUI bool
	// Log receives human-readable install progress.
	Log io.Writer
}

// Service is an instance's one service: where a program in the guest listens,
// and where any process reaches it. The driver picks it, never the caller or
// the image, and it is the same for every boot of an instance.
type Service struct {
	// Listen is the address a guest program listens on, in guest.Listen's
	// form: vsock:PORT (AF_VSOCK on vz, the hvsocket service ID of the port
	// on hcs), tcp:ADDR. The engine writes it to the guest's
	// guest.ServiceFile at every start.
	Listen string
	// URL is where any process reaches the service over HTTP with no
	// credential of the driver's: boxd's https://<machine>.boxd.sh, a port
	// the Docker daemon publishes on the host's loopback, a loopback port a
	// local driver's shim serves.
	URL string
}

// Share is a host directory exported to the guest.
type Share struct {
	Tag      string
	HostPath string
	ReadOnly bool
}

// BootOptions is one boot of an instance.
type BootOptions struct {
	// GUI opens a native window on the guest's display for this boot, in the
	// process that holds the machine. Drivers without Display refuse it.
	GUI bool
	// Title names the window.
	Title string
	// Console receives the driver's own log of the boot, not guest output.
	Console io.Writer
}

// Driver is one hypervisor, and the provider that keeps everything disco-vm
// knows: the images, tags, and instances of the project it was made for
// (Config.Project). Every listing and lookup reads the provider; nothing is
// cached between calls.
type Driver interface {
	Name() string
	Capabilities() Capabilities
	// Check reports whether this host can run the driver: the hypervisor
	// feature, privileges, an entitlement, a credential. It should name the
	// fix.
	Check(ctx context.Context) error

	// Install creates the image img (its ID, GuestOS, and Comment) from
	// installation media, and returns it as the provider keeps it. A driver
	// that does not list Capabilities.Install returns ErrUnsupported.
	Install(ctx context.Context, spec InstallSpec, img Image) (Image, error)
	// Image is the image of an ID, or ErrNotFound. An image whose disk the
	// provider no longer has (a snapshot deleted behind disco-vm's back) is
	// not found.
	Image(ctx context.Context, id string) (Image, error)
	// Images lists the project's images.
	Images(ctx context.Context) ([]Image, error)
	// DeleteImage deletes an image. The caller deletes one only when no tag
	// or instance names it. A driver whose disks chain below the seam (hcs)
	// refuses, with ErrInUse, an image whose disk another image's still
	// differences over, and deletes nothing. An image already gone is not an
	// error.
	DeleteImage(ctx context.Context, id string) error

	// Tags lists the project's tags.
	Tags(ctx context.Context) ([]Tag, error)
	// Tag points ref, a normalized name:tag, at an image, moving it from any
	// other.
	Tag(ctx context.Context, ref, image string) error
	// Untag drops a tag. A tag that is not there is not an error.
	Untag(ctx context.Context, ref string) error

	// Bases lists what the driver already has that an instance can run from
	// with no image (InstanceSpec.Base): boxd's default image, and the
	// snapshots in the account that disco-vm did not make. A driver with
	// nothing of the kind, or nothing it can list (docker's registries),
	// returns none.
	Bases(ctx context.Context) ([]Base, error)
	// Base is the base ref names, by its name or ID, or found false. A
	// driver may know bases it cannot list: docker's is any image the daemon
	// has or its registry answers for, which Create pulls. It never pulls or
	// makes anything itself.
	Base(ctx context.Context, ref string) (base Base, found bool, err error)

	// Create makes a stopped instance: a copy-on-write clone of its image,
	// or a machine made from its base, that boots from power-off with a machine
	// identity of its own. A name the project already has is ErrExists.
	Create(ctx context.Context, spec InstanceSpec) (Instance, error)
	// Instance is the project's instance of a name, or ErrNotFound.
	Instance(ctx context.Context, name string) (Instance, error)
	// Instances lists the project's instances.
	Instances(ctx context.Context) ([]Instance, error)
	// Destroy removes an instance and everything made for it, stopping it
	// first if it runs. One already gone is not an error.
	Destroy(ctx context.Context, inst Instance) error

	// Boot starts a stopped instance in this process and returns once its
	// guest operations answer. The machine is this process's: it lives
	// until it powers off or is killed, and a driver whose machines die
	// with their process (vz) ends it when this process ends. It is not
	// bound to ctx, which only bounds starting it.
	Boot(ctx context.Context, inst Instance, opts BootOptions) (Machine, error)
	// Start starts a stopped instance that outlives this process, and
	// returns once its guest operations answer: under a shim of its own
	// where the machine would die with its process, or as its service
	// owns it. The machine returned is attached, as Attach's is.
	Start(ctx context.Context, inst Instance, opts BootOptions) (Machine, error)
	// Attach returns the running machine of an instance, or ErrNotRunning.
	// Any process may call it, any number of times, with its own
	// credentials. A guest that shut itself down while nothing watched reads
	// as not running. The machine watches for its own stop only once Done,
	// Err, or Kill is called, so attaching to run a command costs nothing
	// that outlives the call.
	Attach(ctx context.Context, inst Instance) (Machine, error)
	// Commit captures a stopped instance's disk as the image img (its ID,
	// GuestOS, and Comment): a snapshot. It removes the instance, and returns
	// the image as the provider keeps it.
	Commit(ctx context.Context, inst Instance, img Image) (Image, error)
	// Service is the instance's service. A URL that exists only while the
	// instance runs (a port the daemon publishes, a shim's) is asked for
	// while it is stopped: then the driver returns ErrNotRunning. A URL
	// that outlives a stop (boxd's) is the same across stop and start.
	Service(ctx context.Context, inst Instance) (Service, error)
}

// FindBase is the base in bases that ref names, by name or ID: Driver.Base
// for a driver that knows only the bases it lists.
func FindBase(bases []Base, ref string) (Base, bool) {
	for _, b := range bases {
		if b.Name == ref || b.ID == ref {
			return b, true
		}
	}
	return Base{}, false
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

// Machine is a running guest. Boot and Start return one whose guest
// operations answer. Attach returns one that is running, which may still be
// starting (a boxd machine starting, a guest that rebooted), so its
// operations can fail until it has.
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
