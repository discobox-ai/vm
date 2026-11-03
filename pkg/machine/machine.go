// Package machine is the seam between disco-vm and a hypervisor.
//
// A Driver is the only code in disco-vm that knows which hypervisor it talks
// to. Everything above it — the engine, the CLI, and a discobox provider —
// speaks only the types in this package, so one program runs on
// Windows (hcs) and macOS (vz) with nothing but the driver differing.
//
// A driver is also the only place anything is kept (ADR 26-10-23-417). The
// engine holds no state: every VM, committed or not (ADR 26-11-03-606), is
// read from the driver, which reads it from its provider every time, and
// keeps what it knows about each on the provider's own resources, in the
// provider's way: labels on Docker's images and containers, the names of
// boxd's snapshots and machines, and files beside each disk in the directory
// of a driver whose provider is this host's disk (hcs, vz, fake; package
// local).
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

// ErrNotFound reports a VM the provider does not have.
var ErrNotFound = errors.New("not found")

// ErrExists reports a VM name the provider already has in the project,
// committed or not.
var ErrExists = errors.New("already exists")

// ErrInUse reports a VM the driver will not remove or boot because a VM
// created from it still needs its disk as it is (Driver.Destroy, Boot, and
// Start, on a driver whose copies chain below the seam).
var ErrInUse = errors.New("a VM created from it still needs its disk")

// ErrCommitted reports a committed VM where its provider has no machine
// left to run (docker's image, boxd's snapshot). Whether a VM is committed
// is the caller's to record (ADR 26-11-03-606); a driver says so only where
// its provider cannot do what was asked.
var ErrCommitted = errors.New("the VM is committed, and its provider has nothing to run")

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
	// Ports reports that an instance's guest ports reach past its one
	// service, in three directions, all through the process that holds the
	// machine: a caller dials any guest port (Machine.Dial), guest processes
	// connect out to the host (Forwards to a Socket), and to another
	// instance of the project's guest port (Forwards to an Instance).
	Ports bool `json:"ports"`
	// Agent reports that the driver puts the disco-vm agent in the guest and
	// reaches the guest's operations through it: Install bakes it in
	// (InstallSpec.Agent) on hcs and vz, and docker adds it to its base
	// at Create. boxd reaches them through its own API, and needs none.
	Agent bool `json:"agent"`
	// Install is every guest OS the driver makes a committed VM of from
	// installation media (Install). An OS it boots but does not install
	// comes from its base (Config.Base), or from a VM committed from one.
	Install []OS `json:"install,omitempty"`
	// Keys is whether the driver issues keys (IssueKey), and the narrowest
	// scope one reaches.
	Keys KeyCapabilities `json:"keys"`
	// Isolation reports that an instance can be created stripped of what
	// the provider gives a machine by default (InstanceSpec.Isolated) and
	// placed on networks of the caller's (InstanceSpec.Networks).
	Isolation bool `json:"isolation"`
	// Egress is every fence an instance can be created with
	// (InstanceSpec.Egress), held outside the guest, where its root cannot
	// loosen it: EgressNone, nothing beyond its networks, and EgressAllow, a
	// list beyond them. Empty is none at all.
	Egress []EgressFence `json:"egress,omitempty"`
	// Volumes is every volume the driver attaches to an instance
	// (InstanceSpec.Data, InstanceSpec.Cache), mounted at VolumePath. Empty
	// is none.
	Volumes []Volume `json:"volumes,omitempty"`
}

// Attaches reports whether the driver attaches the volume v.
func (c Capabilities) Attaches(v Volume) bool { return slices.Contains(c.Volumes, v) }

// EgressFence is a kind of fence a driver holds (Capabilities.Egress).
type EgressFence string

const (
	// EgressNone is an empty Egress.Allow: nothing beyond the instance's
	// networks. vz holds it as an instance with no network device.
	EgressNone EgressFence = "none"
	// EgressAllow is an Egress.Allow that lists hosts, wildcards,
	// addresses, or ranges.
	EgressAllow EgressFence = "allow"
)

// SupportsOS reports whether the driver can run the given guest OS.
func (c Capabilities) SupportsOS(os OS) bool { return slices.Contains(c.GuestOS, os) }

// Installs reports whether the driver installs the given guest OS from
// media.
func (c Capabilities) Installs(os OS) bool { return slices.Contains(c.Install, os) }

// Fences reports whether the driver holds the fence e asks for: EgressNone
// for an empty Allow, EgressAllow for any other. A nil fence is the
// provider's default, which every driver holds.
func (c Capabilities) Fences(e *Egress) bool {
	switch {
	case e == nil:
		return true
	case len(e.Allow) == 0:
		return slices.Contains(c.Egress, EgressNone)
	default:
		return slices.Contains(c.Egress, EgressAllow)
	}
}

// Config is what a driver is made with.
type Config struct {
	// Project scopes every VM the driver lists and makes: it is in the name
	// of each, or on its label. Empty is DefaultProject.
	Project string
	// Base is what a fresh VM starts from (InstanceSpec.From nil), in the
	// driver's own terms: docker's image reference, vz's Linux guest image
	// (a reference or a directory), boxd's image, fake's name. Empty is the
	// provider's default, where it has one (boxd's default image, vz's
	// DefaultLinuxBase, fake's), or none (docker). It is the driver's
	// configuration, never a create's (ADR 26-11-03-606). A driver with no
	// base (hcs, docker unconfigured) refuses a fresh VM.
	Base string
	// Dir is where a driver whose provider is this host's disk (hcs, vz,
	// fake) keeps its VMs and the media it downloads. Empty is DefaultDir().
	// A driver whose provider is a service (boxd, docker) keeps nothing on
	// the host and ignores it.
	Dir string
	// ShimCommand starts the process that holds a running instance, for a
	// driver whose machine dies with the process that booted it (vz) or
	// belongs to the host (hcs): a program and its leading arguments, to
	// which the shim's own are appended. Empty is this executable's `shim`
	// subcommand. A program that embeds the engine points it at a
	// subcommand of its own that calls local.ShimMain, so it ships no second
	// binary.
	ShimCommand []string
	// Keys is the key the driver calls its provider with, asked on every
	// call. Nil is none: a driver whose provider needs a key (boxd) fails
	// its calls, and one that keeps its own (fake) acts as its owner. The
	// command line passes KeyFromEnv; nothing else reads the environment
	// for a key.
	Keys KeySource
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

// DefaultProject is the project of everything made without one named.
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

// ValidateName checks a VM's name: 1 to 63 lowercase letters, digits, and
// single hyphens, starting and ending with a letter or digit. It is unique in
// the project across every VM, committed or not, and in the name of what the
// driver makes for the VM (a boxd machine or snapshot, a container or a
// Docker image), which must be a DNS label in a proxy's domain, so a driver
// may refuse a name that is valid here but too long beside its project.
func ValidateName(name string) error {
	if !label(name, 63) || strings.Contains(name, "--") {
		return fmt.Errorf("machine: VM name %q is not 1 to 63 lowercase letters, digits, and single hyphens, starting and ending with a letter or digit", name)
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

// Forward sends the connections a guest process opens to the host on a port
// (guest.DialHost: vsock to the host on vz, hvsocket on HCS) to a Unix socket
// on the host, which something there serves, or to a guest port of another
// instance of the project. It is how a guest reaches a host service, or a
// peer, without a TCP listener or a network device, as libkrun maps a vsock
// port to a Unix socket. Exactly one of Socket and Instance is set.
type Forward struct {
	Port   uint32 `json:"port"`
	Socket string `json:"socket,omitempty"`
	// Instance names another instance of the project, whose guest port
	// InstancePort each connection is spliced into while both run. While
	// the target is not running each connection is closed at once, so the
	// guest connects and reads EOF. The target sees the connection come
	// from the host, not from this instance.
	Instance     string `json:"instance,omitempty"`
	InstancePort uint32 `json:"instancePort,omitempty"`
}

// Peer reports whether the forward goes to another instance, not to a host
// socket.
func (f Forward) Peer() bool { return f.Instance != "" }

// InstanceSpec is a new VM (ADR 26-11-03-606).
type InstanceSpec struct {
	// Name is unique in the project, across every VM, committed or not
	// (ValidateName).
	Name string
	// From is the committed VM the new one is a copy of, as the driver
	// found it; its guest OS is the new VM's. That it was committed is the
	// caller's to know: a driver whose provider cannot tell (Archil) copies
	// any stopped VM. Nil is a fresh VM, from the
	// driver's base (Config.Base), whose guest OS is the base's; a driver
	// with no base refuses it with ErrUnsupported.
	From *Instance
	// CPUs and Memory size every boot; zero is the driver's default.
	CPUs   int
	Memory uint64
	// Forwards are served whenever the instance runs, by a driver that lists
	// Capabilities.Ports.
	Forwards []Forward
	// Isolated strips the machine of what its provider gives one by default
	// (boxd: the in-VM CLI and its token, the git credential helper and
	// integrations, and the default network's peers), for good: a VM
	// committed from it, and a VM created from that, stay isolated. A driver that does not
	// list Capabilities.Isolation refuses it.
	Isolated bool
	// Networks are the provider's networks the machine peers on, in place
	// of its default one. A driver that does not list
	// Capabilities.Isolation refuses them.
	Networks []string
	// Egress fences what the machine reaches beyond its networks, in place
	// before Create returns and across stop and start; nil is the
	// provider's default. It is the spec's, never a committed VM's. A driver that
	// does not hold the fence asked for (Capabilities.Fences) refuses it.
	Egress *Egress
	// Data is the instance's data volume, and Cache its cache volume; nil is
	// none. A driver that does not list the volume in
	// Capabilities.Volumes refuses it. Neither is ever part of a committed
	// VM.
	Data  *DataVolume
	Cache *CacheVolume
}

// Egress is an instance's fence: what it may reach beyond its networks,
// which are never fenced by it.
type Egress struct {
	// Allow is the hostnames, wildcards (*.example.com), IPv4 addresses,
	// and IPv4 ranges the instance may reach. Empty is nothing.
	Allow []string `json:"allow,omitempty"`
}

// Instance is one VM, as its provider has it, committed or not: which it is
// is the caller's to record, since not every provider can tell (ADR
// 26-11-03-606).
type Instance struct {
	// ID is the provider's own: a container's or a Docker image's ID, a boxd
	// machine's or snapshot's, the directory of a local driver's VM.
	ID   string `json:"id"`
	Name string `json:"name"`
	// From is the name of the committed VM this one was created from, where
	// the provider says; empty for a fresh VM, an install, or where it does
	// not.
	From    string    `json:"from,omitempty"`
	GuestOS OS        `json:"guestOS"`
	CPUs    int       `json:"cpus,omitempty"`
	Memory  uint64    `json:"memory,omitempty"`
	Created time.Time `json:"created,omitzero"`
	// Forwards are served whenever it runs.
	Forwards []Forward `json:"forwards,omitempty"`
	// Isolated and Networks are as InstanceSpec's, as the provider reports
	// them.
	Isolated bool     `json:"isolated,omitempty"`
	Networks []string `json:"networks,omitempty"`
	// Egress is the fence the provider holds, as InstanceSpec's: nil is
	// none.
	Egress *Egress `json:"egress,omitempty"`
	// Data and Cache are its volumes, as the provider reports them; Cache
	// says which generation it was cloned from. A committed VM has neither.
	Data  *DataVolume  `json:"data,omitempty"`
	Cache *CacheVolume `json:"cache,omitempty"`
}

// InstallSpec makes a committed VM from installation media.
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
	// lists Capabilities.Agent. The driver bakes it into the disk and
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
// a committed VM, and it is the same for every boot of a VM.
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
	// PrivateURL is where the instance's peers, other instances of the
	// driver on its networks, reach the service: a direct path,
	// http://<address>:<port>, at an address where every port the guest
	// listens on is reachable too; URL, where the driver gives instances no
	// path to each other but URL reaches them; or empty, where neither does.
	// A caller tells a direct path by PrivateURL != URL.
	PrivateURL string
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
// knows: the VMs of the project it was made for (Config.Project), committed
// or not (ADR 26-11-03-606). Every listing and lookup reads the provider;
// nothing is cached between calls.
type Driver interface {
	Name() string
	Capabilities() Capabilities
	// Check reports whether this host can run the driver: the hypervisor
	// feature, privileges, an entitlement, a credential. It should name the
	// fix.
	Check(ctx context.Context) error

	// Install makes the VM name from installation media, committed (the
	// caller records it), and returns it as the provider keeps it. A name the project already has is
	// ErrExists. A driver that does not list Capabilities.Install returns
	// ErrUnsupported.
	Install(ctx context.Context, spec InstallSpec, name string) (Instance, error)

	// Create makes a stopped VM: a copy-on-write copy of the committed VM
	// spec.From, or a fresh one from the driver's base, that boots from
	// power-off with a machine identity of its own. A name the project
	// already has, committed or not, is ErrExists; a From the provider no
	// longer has is ErrNotFound, and a running one is refused; a fresh VM
	// on a driver with no base is ErrUnsupported. Its data volume is made empty, and its cache
	// volume a clone of the key's current generation.
	Create(ctx context.Context, spec InstanceSpec) (Instance, error)
	// Instance is the project's VM of a name, committed or not, or
	// ErrNotFound.
	Instance(ctx context.Context, name string) (Instance, error)
	// Instances lists the project's VMs, committed or not.
	Instances(ctx context.Context) ([]Instance, error)
	// Destroy removes a VM and everything made for it, its volumes
	// included, stopping it first if it runs. One already gone is not an
	// error. A driver whose copies chain below the seam (hcs) refuses, with
	// ErrInUse, a VM that a VM created from it still needs, and removes
	// nothing; elsewhere a VM created from one outlives it.
	Destroy(ctx context.Context, inst Instance) error

	// Boot starts a stopped VM in this process and returns once its guest
	// operations answer. A committed VM whose provider has no machine left
	// is ErrCommitted, and one whose disk a VM created from it differences
	// over is ErrInUse, as they are to Start. The machine is this process's: it lives
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
	// Commit freezes a stopped VM under its name for copying, and returns it
	// (ADR 26-11-03-606): its data volume and cache clone are removed, and
	// its disk is what VMs are created from. A running VM is refused. The
	// caller records that it is committed and never starts it again: a
	// driver whose provider keeps a committed VM apart (docker's image,
	// boxd's snapshot) refuses to, and one that cannot tell (Archil) does
	// not.
	Commit(ctx context.Context, inst Instance) (Instance, error)
	// Service is the instance's service. A URL that exists only while the
	// instance runs (a port the daemon publishes, a shim's) is asked for
	// while it is stopped: then the driver returns ErrNotRunning. A URL
	// that outlives a stop (boxd's) is the same across stop and start.
	Service(ctx context.Context, inst Instance) (Service, error)

	// Caches lists the project's cache keys, each with its current
	// generation. A driver that does not list VolumeCache lists none.
	Caches(ctx context.Context) ([]Cache, error)
	// SaveCache snapshots stopped inst's cache volume as a new generation
	// and points save.Key at it, keeping the instance and its volume. A
	// guarded save whose key does not hold the generation it expects is
	// ErrCacheMoved, and saves nothing. The key's old generation is removed
	// once nothing needs it. A driver that does not list VolumeCache returns
	// ErrUnsupported.
	SaveCache(ctx context.Context, inst Instance, save CacheSave) (Cache, error)
	// DeleteCache drops a cache key and the generations nothing else needs.
	// A key already gone is not an error. A driver that does not list
	// VolumeCache returns ErrUnsupported.
	DeleteCache(ctx context.Context, key string) error

	// IssueKey makes the narrowest key the driver can for another host to
	// run this project's machines with. An issued key expires, and cannot
	// itself issue keys: a key that could would let its holder make one
	// that outlives every revocation. A driver with no key to hand on
	// returns ErrNoKey; one whose provider cannot make such a key returns
	// ErrCannotIssue (Capabilities.Keys says which up front). It never
	// returns the caller's own key: handing that on is the caller's choice.
	IssueKey(ctx context.Context, req IssueRequest) (Key, error)
	// IssuedKeys lists the keys the driver issued for this project under a
	// name, or under every name when name is empty, without their secrets.
	IssuedKeys(ctx context.Context, name string) ([]Key, error)
	// RevokeKey revokes a key the driver issued for this project (by its
	// ID), so it works no more. One already gone is not an error; one the
	// driver did not issue for this project is refused. A driver with no
	// keys returns ErrNoKey.
	RevokeKey(ctx context.Context, key Key) error
}

// HostListener is a running machine whose guest processes can connect out to
// the host. A driver that lists Capabilities.Ports returns machines that
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
	// Dial opens a stream to a guest port: vsock on vz, hvsocket on HCS with
	// the service ID derived from the port by the vsock template, the host's
	// loopback on fake. An attached machine's goes through the process that
	// holds it. A driver that does not list Capabilities.Ports returns
	// ErrUnsupported.
	Dial(ctx context.Context, port uint32) (net.Conn, error)
}
