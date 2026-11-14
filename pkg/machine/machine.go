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
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
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

// ErrExists reports a VM name the provider already has in the pool,
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

// ErrCapacity reports a boot refused because the host already runs as many
// guests of the instance's OS as Capabilities.MaxRunning allows, counting
// every pool and every process (Driver.Boot and Start). It is the
// hypervisor's limit, not a policy: one stopping makes room.
var ErrCapacity = errors.New("machine: the host runs as many guests of this OS as the hypervisor allows")

// ErrNotRunning reports that an instance has no running machine to attach to.
var ErrNotRunning = errors.New("machine: the instance is not running")

// ErrStale reports a running instance held by a process of another build
// that this one cannot reach its guest through: a shim an older (or newer)
// disco-vm started, which outlived the engine that started it. The instance
// still runs, still stops, and boots again on its own disk; stopping it and
// starting it again replaces the process.
var ErrStale = errors.New("machine: the process that holds the instance is of another disco-vm; stop the instance and start it again to replace it")

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
	// cap. Virtualization.framework allows two macOS guests per host. The
	// driver enforces it, across every pool it keeps, and refuses a boot past
	// it with ErrCapacity.
	MaxRunning map[OS]int `json:"maxRunning,omitempty"`
	// SharedDirectories reports host directory sharing (virtiofs, Plan9).
	SharedDirectories bool `json:"sharedDirectories"`
	// Display reports a viewable guest framebuffer, which a boot shows in a
	// window when asked (BootOptions.GUI).
	Display bool `json:"display"`
	// View reports that a window opens on a running instance's desktop from
	// any process (Driver.View), and on an install in progress
	// (Driver.ViewInstall), not only from the process that booted it. It
	// implies Display.
	View bool `json:"view"`
	// Ports reports that an instance's guest ports reach past its one
	// service, in three directions, all through the process that holds the
	// machine: a caller dials the agent's port or the service's
	// (Machine.Dial; guest.DialablePort), guest processes connect out to the
	// host (Forwards to a Socket), and to another instance's agent or
	// service port (Forwards to an Instance), of the pool or, with
	// AcrossPools, of another. Every port is known at boot (ADR
	// 26-11-03-247).
	Ports bool `json:"ports"`
	// AcrossPools reports that the driver keeps every pool's VMs where each
	// pool's driver reaches them (one root), so a VM is created from a
	// committed VM of another pool (InstanceSpec.From with a Pool of its
	// own) and forwards to an instance of another pool (Forward.Pool). Only
	// the program that embeds the engine names another pool
	// (engine.Engine.From and Forwards), never a pool's own request, and a
	// driver called with a key it issued refuses it, since that key is one
	// pool's. A driver without it refuses both with ErrUnsupported.
	AcrossPools bool `json:"acrossPools"`
	// Agent reports that the driver puts the disco-vm agent in the guest and
	// reaches the guest's operations through it: Install bakes it in
	// (InstallSpec.Agent) on hcs, a boot hands it to a vz or libkrun guest
	// (a read-only drive on macOS, the initrd on Linux), and docker adds it
	// to its base at Create. boxd reaches them through its own API, and
	// needs none.
	Agent bool `json:"agent"`
	// Install is every guest OS the driver makes a committed VM of from
	// installation media (Install). An OS it boots but does not install
	// comes from its base (Config.Base), or from a VM committed from one.
	// A driver that installs any reports Commit.
	Install []OS `json:"install,omitempty"`
	// Commit reports that the driver commits a stopped VM (Driver.Commit)
	// and creates VMs from a committed one (InstanceSpec.From), which go
	// together (ADR 26-11-03-868). A driver without it refuses both, and
	// Upgrade with a from, with ErrUnsupported and changes nothing, so a
	// caller plans without trying: a pool on such a driver makes every VM
	// fresh. A driver that installs (Install) or has no base reports it,
	// since it makes VMs no other way.
	Commit bool `json:"commit"`
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
	// URLs is which of a service's URLs the driver gives an instance: never,
	// when asked (it can give one and withhold one), or always
	// (InstanceSpec.URLs).
	URLs URLCapabilities `json:"urls"`
}

// URLReach is whether a driver gives an instance one of its service's URLs
// (Capabilities.URLs).
type URLReach string

const (
	// URLNever is a URL the driver gives no instance.
	URLNever URLReach = ""
	// URLOptional is a URL the driver gives an instance or withholds, as
	// the instance's URLRequest says.
	URLOptional URLReach = "optional"
	// URLAlways is a URL every instance of the driver has.
	URLAlways URLReach = "always"
)

// URLCapabilities is a driver's URLReach for each of a service's URLs, by
// who reaches it (ADR 26-11-12-604; ADR 26-11-04-412 §4):
//
//   - Internal: the caller managing the pool, alone. No driver reports one
//     yet (machine.Service has no field for it).
//   - Private: the instance's peers, at Service.PrivateURL.
//   - Public: anyone on the internet, at Service.URL. A URL on the engine's
//     host alone, a loopback port, is not one: a local driver's or docker's
//     Service.URL is never public.
type URLCapabilities struct {
	Internal URLReach `json:"internal,omitempty"`
	Private  URLReach `json:"private,omitempty"`
	Public   URLReach `json:"public,omitempty"`
}

// URLMode is what a caller asks of one of a service's URLs.
type URLMode string

const (
	// URLDefault is the driver's default.
	URLDefault URLMode = ""
	// URLOn asks for the URL.
	URLOn URLMode = "on"
	// URLOff asks for none.
	URLOff URLMode = "off"
)

// URLRequest is what a caller asks of one of a service's URLs. The driver
// honors Mode where it can. Without Strict, where it cannot, the instance
// gets what the driver gives, either way; with it, the driver gives exactly
// what Mode asks or refuses the create with ErrUnsupported. Strict needs a
// Mode.
type URLRequest struct {
	Mode   URLMode `json:"mode,omitempty"`
	Strict bool    `json:"strict,omitempty"`
}

// URLRequests is a URLRequest for each of a service's URLs, as
// URLCapabilities names them. The zero value is the driver's default for
// every one.
type URLRequests struct {
	Internal URLRequest `json:"internal"`
	Private  URLRequest `json:"private"`
	Public   URLRequest `json:"public"`
}

// Check refuses, with ErrUnsupported, a strict request the capabilities
// cannot meet: a URL the driver never gives asked on, or one it always gives
// asked off. A mode it does not know, or Strict with no mode, is an error.
func (r URLRequests) Check(caps URLCapabilities) error {
	for _, c := range []struct {
		name  string
		req   URLRequest
		reach URLReach
	}{{"internal", r.Internal, caps.Internal}, {"private", r.Private, caps.Private}, {"public", r.Public, caps.Public}} {
		switch c.req.Mode {
		case URLDefault:
			if c.req.Strict {
				return fmt.Errorf("a strict %s URL request needs on or off", c.name)
			}
		case URLOn:
			if c.req.Strict && c.reach == URLNever {
				return fmt.Errorf("the driver gives no instance its %s URL: %w", c.name, ErrUnsupported)
			}
		case URLOff:
			if c.req.Strict && c.reach == URLAlways {
				return fmt.Errorf("the driver gives every instance its %s URL: %w", c.name, ErrUnsupported)
			}
		default:
			return fmt.Errorf("a %s URL request of %q, not on or off", c.name, c.req.Mode)
		}
	}
	return nil
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
	// Pool scopes every VM the driver lists and makes: it is in the name
	// of each, or on its label. Empty is DefaultPool.
	Pool string
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
	// binary. The command is given its stdin and environment as they are:
	// until the shim is up, its stdin is a pipe from the start that spawned
	// it, which its environment names, and a shim whose start gives up
	// gives its boot up too.
	ShimCommand []string
	// Keys is the key the driver calls its provider with, asked on every
	// call. Nil is none: a driver whose provider needs a key (boxd) fails
	// its calls, and one that keeps its own (fake) acts as its owner. The
	// command line passes KeyFromEnv; nothing else reads the environment
	// for a key.
	Keys KeySource
	// DriverConfig is the driver's own configuration: a JSON object whose
	// fields its Definition describes (DefinitionOf) and its Validator
	// checks, which no other driver shares. Empty is the driver's defaults.
	// New refuses one its driver does not accept. A driver never reads
	// the environment for it: the command line reads each field's
	// ConfigField.Env (ConfigFromEnv). It holds no secret; a key is Keys'.
	DriverConfig json.RawMessage
	// Dial opens every connection the driver makes to its provider's
	// service, and is given the service's own address: boxd's API and
	// token exchange, Archil's API and process API, a Docker daemon at its
	// host (unix or tcp), the registry a local driver pulls its base or
	// runtime from (vz's and libkrun's), and the host vz downloads macOS
	// media from (install's "latest"). Set, the driver reads no proxy
	// variable (HTTPS_PROXY, boxd's ALL_PROXY): an embedder that checks
	// where a driver connects, as it connects, proxies within its dialer if
	// it proxies at all. Nil is the process's own way, proxy variables and
	// all. A shim is never given it, so libkrun's pull of a runtime removed
	// from the cache behind the driver's back, which a boot makes, dials
	// the process's own way. Which macOS media is the latest is
	// Virtualization.framework's own lookup, which no dialer can reach.
	Dial DialFunc
}

// DialFunc opens a connection, as net.Dialer.DialContext does.
type DialFunc func(ctx context.Context, network, addr string) (net.Conn, error)

// Dialer is c.Dial, or a net.Dialer's where it is nil: what a driver
// dials a service with that has no proxy of its own (a Docker daemon).
func (c Config) Dialer() DialFunc {
	if c.Dial != nil {
		return c.Dial
	}
	var d net.Dialer
	return d.DialContext
}

// HTTPTransport is the transport a driver calls an HTTP service with: one
// that dials the service through c.Dial and reads no proxy variable, or,
// where Dial is nil, http.DefaultTransport's own, which takes HTTPS_PROXY.
func (c Config) HTTPTransport() *http.Transport {
	t := &http.Transport{Proxy: http.ProxyFromEnvironment, ForceAttemptHTTP2: true}
	if def, ok := http.DefaultTransport.(*http.Transport); ok {
		t = def.Clone()
	}
	if c.Dial != nil {
		t.Proxy = nil
		t.DialContext = c.Dial
	}
	return t
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

// DefaultPool is the pool of everything made without one named.
const DefaultPool = "default"

// ValidatePool checks a pool name: 1 to 32 lowercase letters, digits,
// and single hyphens, starting and ending with a letter or digit. That fits
// in a DNS label beside the rest of a resource's name, as a boxd machine's
// name must, and a driver can end it with "--" and split it back off.
func ValidatePool(pool string) error {
	if !label(pool, 32) || strings.Contains(pool, "--") {
		return fmt.Errorf("machine: pool %q is not 1 to 32 lowercase letters, digits, and single hyphens, starting and ending with a letter or digit", pool)
	}
	return nil
}

// ValidateName checks a VM's name: 1 to 63 lowercase letters, digits, and
// single hyphens, starting and ending with a letter or digit. It is unique in
// the pool across every VM, committed or not, and in the name of what the
// driver makes for the VM (a boxd machine or snapshot, a container or a
// Docker image), which must be a DNS label in a proxy's domain, so a driver
// may refuse a name that is valid here but too long beside its pool.
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
// a pool, after its own prefix (boxd's "dvm-"): the pool and "--". The
// default pool's names before pools had no pool in them, so
// ResourcePool reads such a name as the default pool's.
// An empty pool is the default.
func ResourcePrefix(prefix, pool string) string {
	if pool == "" {
		pool = DefaultPool
	}
	return prefix + pool + "--"
}

// ResourcePool is the pool a name made with ResourcePrefix belongs to:
// the default pool for a name under prefix with no "--" in it, and "" for
// a name that is not under prefix at all.
func ResourcePool(prefix, name string) string {
	rest, ok := strings.CutPrefix(name, prefix)
	if !ok {
		return ""
	}
	pool, _, ok := strings.Cut(rest, "--")
	if !ok {
		return DefaultPool
	}
	return pool
}

// Forward sends the connections a guest process opens to the host on a port
// (guest.DialHost: vsock to the host on vz, hvsocket on HCS) to a Unix socket
// on the host, which something there serves, or to a guest port of another
// instance, of the pool or, on a driver that lists Capabilities.AcrossPools,
// of another pool. It is how a guest reaches a host service, or a peer,
// without a TCP listener or a network device, as libkrun maps a vsock port
// to a Unix socket. Exactly one of Socket and Instance is set.
type Forward struct {
	Port   uint32 `json:"port"`
	Socket string `json:"socket,omitempty"`
	// Instance names another instance, whose guest port InstancePort each
	// connection is spliced into while both run. While the target is not
	// running each connection is closed at once, so the guest connects and
	// reads EOF. The target sees the connection come from the host, not
	// from this instance.
	Instance     string `json:"instance,omitempty"`
	InstancePort uint32 `json:"instancePort,omitempty"`
	// Pool is the pool Instance is in; empty is the forwarding instance's
	// own. Another pool needs Capabilities.AcrossPools, and only the
	// program that embeds the engine sets one (engine.Engine.Forwards).
	Pool string `json:"pool,omitempty"`
}

// Peer reports whether the forward goes to another instance, not to a host
// socket.
func (f Forward) Peer() bool { return f.Instance != "" }

// OtherPool reports whether pool, a Forward's or an Instance's, names a pool
// other than own: an empty pool is own.
func OtherPool(pool, own string) bool { return pool != "" && pool != own }

// InstanceSpec is a new VM (ADR 26-11-03-606).
type InstanceSpec struct {
	// Name is unique in the pool, across every VM, committed or not
	// (ValidateName).
	Name string
	// From is the committed VM the new one is a copy of, as the driver
	// found it; its guest OS is the new VM's. Its Pool is the driver's, or,
	// on a driver that lists Capabilities.AcrossPools, another pool's,
	// found by a driver made for that pool; an empty Pool is the driver's. That it was committed is the
	// caller's to know: a driver whose provider cannot tell (Archil) copies
	// any stopped VM. A driver that does not report Capabilities.Commit
	// refuses it with ErrUnsupported. Nil is a fresh VM, from the
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
	// URLs asks for each of the service's URLs, or for none; the zero value
	// is the driver's default for each. A public URL is an inbound path from
	// the internet to the guest that its egress fence never sees, answered by
	// whatever the guest's root binds to the service's port, so a caller
	// that must not have one asks for it off, strictly (ADR 26-11-12-604).
	URLs URLRequests
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
	// Pool is the pool it is in: the pool of the driver that found it.
	Pool string `json:"pool"`
	// From is the name of the committed VM this one was created from, where
	// the provider says; empty for a fresh VM, an install, where it does
	// not, and always on a driver that does not report Capabilities.Commit.
	From string `json:"from,omitempty"`
	// FromPool is the pool From is in, where it is not this VM's own
	// (Capabilities.AcrossPools).
	FromPool string    `json:"fromPool,omitempty"`
	GuestOS  OS        `json:"guestOS"`
	CPUs     int       `json:"cpus,omitempty"`
	Memory   uint64    `json:"memory,omitempty"`
	Created  time.Time `json:"created,omitzero"`
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
	// Release is the guest OS release an install resolved its media to,
	// carried by every VM created from it or upgraded to it, so an embedder
	// can key a base on it (discobox ADR 26-10-09-106 §5): what `latest`
	// was when it was installed. Nil where the driver resolved none, and
	// for a fresh VM.
	Release *Release `json:"release,omitempty"`
}

// Release is a guest OS release: macOS's product version and build, as an
// IPSW's restore image reports them ("27.0.0", "26A428"). Version is always
// set; Build, where the media names one, tells two releases of one version
// apart, so a key made from a Release uses both (String).
type Release struct {
	Version string `json:"version"`
	Build   string `json:"build,omitempty"`
}

// String is the version, then the build in parentheses where there is one:
// "27.0.0 (26A428)".
func (r Release) String() string {
	if r.Build == "" {
		return r.Version
	}
	return r.Version + " (" + r.Build + ")"
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
	// lists Capabilities.Agent. hcs bakes it into the disk and arranges for
	// `disco-vm guest` to start at boot as a SYSTEM service. vz does not
	// read it: a macOS guest runs the agent each boot attaches on a drive,
	// its configured macOS agent (ADR 26-11-13-318).
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
	// local driver's shim serves. It is empty for an instance whose public
	// URL was asked off, on a driver that can withhold it.
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
// knows: the VMs of the pool it was made for (Config.Pool), committed
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
	// caller records it), and returns it as the provider keeps it. A name the pool already has is
	// ErrExists. A driver that does not list Capabilities.Install returns
	// ErrUnsupported.
	Install(ctx context.Context, spec InstallSpec, name string) (Instance, error)

	// Create makes a stopped VM: a copy-on-write copy of the committed VM
	// spec.From, or a fresh one from the driver's base, that boots from
	// power-off with a machine identity of its own. A name the pool
	// already has, committed or not, is ErrExists; a From the provider no
	// longer has is ErrNotFound, and a running one is refused; a From on a
	// driver that does not report Capabilities.Commit, a From or a Forward
	// of another pool on a driver that does not list AcrossPools, and a
	// fresh VM on a driver with no base, are ErrUnsupported, and make
	// nothing. Its data
	// volume is made empty, and its cache volume a clone of the key's
	// current generation.
	Create(ctx context.Context, spec InstanceSpec) (Instance, error)
	// Instance is the pool's VM of a name, committed or not, or
	// ErrNotFound.
	Instance(ctx context.Context, name string) (Instance, error)
	// Instances lists the pool's VMs, committed or not.
	Instances(ctx context.Context) ([]Instance, error)
	// Destroy removes a VM and everything made for it, its volumes
	// included, stopping it first if it runs: nothing else removes a data
	// volume but Commit (ADR 26-11-03-782). One already gone is not an
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
	// that outlives the call. A machine held by a process this build cannot
	// use is attached all the same, so it reads as running and stops as any
	// other does (Shutdown without reboot, Kill, Done); its other guest
	// operations and Dial return ErrStale.
	Attach(ctx context.Context, inst Instance) (Machine, error)
	// Commit freezes a stopped VM under its name for copying, and returns it
	// (ADR 26-11-03-606): its data volume and cache clone are removed, and
	// its disk is what VMs are created from. A running VM is refused. The
	// caller records that it is committed and never starts it again: a
	// driver whose provider keeps a committed VM apart (docker's image,
	// boxd's snapshot) refuses to, and one that cannot tell (Archil) does
	// not. A driver that does not report Capabilities.Commit returns
	// ErrUnsupported and leaves the VM as it was, running or stopped, with
	// its volumes (ADR 26-11-03-868).
	Commit(ctx context.Context, inst Instance) (Instance, error)
	// Upgrade gives stopped VM inst a new root disk: a copy of the committed
	// VM from, or, where from is nil, a fresh one from the driver's base
	// (ADR 26-11-03-782). The VM keeps its name, its data volume and its
	// cache clone as they are, and what its spec set (CPUs, Memory,
	// Forwards, Isolated, Networks, Egress). Its old root is removed. It
	// returns the VM as the provider now has it, whose ID, From, and
	// Created may differ; an Instance from before still names it.
	//
	// It refuses, changing nothing, a running VM; a from whose guest OS is
	// not the VM's, that the provider no longer has (ErrNotFound), or that
	// is running; a fresh root on a driver with no base, a from on a driver
	// that does not report Capabilities.Commit, a from of another pool on a
	// driver that does not list AcrossPools, and any upgrade on a driver
	// that does not list VolumeData (ErrUnsupported); and a from of another
	// pool to a caller with a key the driver issued (ErrUnauthorized). An upgrade
	// interrupted at any step leaves the VM listed under its name with its
	// data volume: Upgrade again with the same from finishes it, Destroy
	// removes it, and Boot and Start refuse it until one of them has.
	Upgrade(ctx context.Context, inst Instance, from *Instance) (Instance, error)
	// View opens a window in this process on running inst's desktop,
	// titled title, and returns once someone closes it, ctx is done, or the
	// instance stops: from any process, not only the one that booted it
	// with BootOptions.GUI. A driver that does not report Capabilities.View
	// returns ErrUnsupported.
	View(ctx context.Context, inst Instance, title string) error
	// ViewInstall opens a window in this process on an install in progress
	// in another (Install), from its first logon on: the install of the VM
	// name, or, when name is empty, the only one. It returns once someone
	// closes it, ctx is done, or the install stops booting its guest. None
	// in progress is ErrNotFound; one not booted yet, or done booting, is
	// ErrNotRunning. Waiting for a desktop to show is reported to log. A
	// driver that does not report Capabilities.View returns ErrUnsupported.
	ViewInstall(ctx context.Context, name, title string, log io.Writer) error
	// Service is the instance's service. A URL that exists only while the
	// instance runs (a port the daemon publishes, a shim's) is asked for
	// while it is stopped: then the driver returns ErrNotRunning. A URL
	// that outlives a stop (boxd's) is the same across stop and start.
	Service(ctx context.Context, inst Instance) (Service, error)

	// Caches lists the pool's cache keys, each with its current
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
	// run this pool's machines with. An issued key expires, and cannot
	// itself issue keys: a key that could would let its holder make one
	// that outlives every revocation. A driver with no key to hand on
	// returns ErrNoKey; one whose provider cannot make such a key returns
	// ErrCannotIssue (Capabilities.Keys says which up front). It never
	// returns the caller's own key: handing that on is the caller's choice.
	IssueKey(ctx context.Context, req IssueRequest) (Key, error)
	// IssuedKeys lists the keys the driver issued for this pool under a
	// name, or under every name when name is empty, without their secrets.
	IssuedKeys(ctx context.Context, name string) ([]Key, error)
	// RevokeKey revokes a key the driver issued for this pool (by its
	// ID), so it works no more. One already gone is not an error; one the
	// driver did not issue for this pool is refused. A driver with no
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
	// Dial opens a stream to a guest port, the agent's or the service's
	// (guest.DialablePort): vsock on vz and libkrun, hvsocket on HCS with
	// the service ID derived from the port by the vsock template, the host's
	// loopback on fake. An attached machine's goes through the process that
	// holds it. A driver that does not list Capabilities.Ports returns
	// ErrUnsupported.
	Dial(ctx context.Context, port uint32) (net.Conn, error)
}
