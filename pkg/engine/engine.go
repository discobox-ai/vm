// Package engine is disco-vm as a library: VMs and their lifecycle over one
// machine.Driver (ADR 26-11-03-606). The CLI is a thin layer over it, and a
// discobox provider embeds it the same way.
//
// The engine keeps no state (ADR 26-10-23-417). Every VM is the driver's,
// which keeps it in its provider, so any process
// with the provider's credentials sees the same ones, and nothing the engine
// knows can disagree with what the provider has. There is no daemon either: a
// running instance is the provider's (boxd's API, the Docker daemon), or a
// shim's that the driver starts (vz, hcs), and every call attaches to it
// through the driver.
package engine

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"time"

	"github.com/discobox-ai/vm/internal/fsutil"
	"github.com/discobox-ai/vm/pkg/guest"
	"github.com/discobox-ai/vm/pkg/machine"
)

// Engine is one project, over a driver made for it.
type Engine struct {
	Driver machine.Driver
	// Project is the driver's project, which every VM the engine reads and
	// makes is in.
	Project string
}

// Open is an engine over driver, which was made for project (an empty
// project is machine.DefaultProject).
func Open(driver machine.Driver, project string) (*Engine, error) {
	if project == "" {
		project = machine.DefaultProject
	}
	if err := machine.ValidateProject(project); err != nil {
		return nil, err
	}
	return &Engine{Driver: driver, Project: project}, nil
}

// ErrNotFound reports an unknown instance.
var ErrNotFound = errors.New("instance not found")

// State is an instance's power state.
type State string

const (
	Stopped State = "stopped"
	Running State = "running"
	// Unknown is an instance whose provider could not be asked.
	Unknown State = "unknown"
)

// Instance is one VM, committed or not, as its driver keeps it.
type Instance = machine.Instance

// CreateOptions describes a new VM.
type CreateOptions struct {
	// Name is the VM's name, unique in the project; empty is a random one.
	Name string
	// From names the committed VM the new one is a copy of; empty is a
	// fresh VM from the driver's base. That it is committed is the caller's
	// to know. The driver must report Capabilities.Commit.
	From   string
	CPUs   int
	Memory uint64
	// Forwards are served whenever the instance runs, each to a host socket
	// or to another instance's guest port; the driver must list
	// Capabilities.Ports.
	Forwards []machine.Forward
	// Isolated and Networks strip the instance of what its provider gives a
	// machine by default, and place it on networks of the caller's
	// (machine.InstanceSpec); the driver must list Capabilities.Isolation.
	Isolated bool
	Networks []string
	// Egress fences what the instance reaches beyond its networks, from
	// before Create returns (machine.InstanceSpec); nil is the provider's
	// default. The driver must hold the fence (Capabilities.Fences).
	Egress *machine.Egress
	// Data and Cache are the instance's volumes, mounted at
	// machine.VolumePath; nil is none. The driver must list each in
	// Capabilities.Volumes. Cache.Generation is the driver's to report.
	Data  *machine.DataVolume
	Cache *machine.CacheVolume
}

// Create makes a VM: a copy of the committed VM opts.From, or a fresh one
// from the driver's base.
func (e *Engine) Create(ctx context.Context, opts CreateOptions) (Instance, error) {
	if err := e.checkForwards(opts.Forwards); err != nil {
		return Instance{}, err
	}
	if (opts.Isolated || len(opts.Networks) > 0) && !e.Driver.Capabilities().Isolation {
		return Instance{}, fmt.Errorf("driver %s cannot isolate an instance or place it on networks: %w", e.Driver.Name(), machine.ErrUnsupported)
	}
	if !e.Driver.Capabilities().Fences(opts.Egress) {
		if len(opts.Egress.Allow) == 0 {
			return Instance{}, fmt.Errorf("driver %s cannot fence an instance to nothing beyond its networks: %w", e.Driver.Name(), machine.ErrUnsupported)
		}
		return Instance{}, fmt.Errorf("driver %s cannot fence an instance's egress to a list: %w", e.Driver.Name(), machine.ErrUnsupported)
	}
	if err := e.checkVolumes(opts.Data, opts.Cache); err != nil {
		return Instance{}, err
	}
	spec := machine.InstanceSpec{
		Name: opts.Name, CPUs: opts.CPUs, Memory: opts.Memory, Forwards: opts.Forwards,
		Isolated: opts.Isolated, Networks: opts.Networks, Egress: opts.Egress,
	}
	if opts.Data != nil {
		spec.Data = &machine.DataVolume{Bytes: opts.Data.Bytes}
	}
	if opts.Cache != nil {
		spec.Cache = &machine.CacheVolume{Key: opts.Cache.Key, Bytes: opts.Cache.Bytes}
	}
	if opts.From != "" {
		if !e.Driver.Capabilities().Commit {
			return Instance{}, fmt.Errorf("driver %s cannot create a VM from a committed one: %w", e.Driver.Name(), machine.ErrUnsupported)
		}
		from, err := e.Get(ctx, opts.From)
		if err != nil {
			return Instance{}, err
		}
		if state := e.State(ctx, from); state != Stopped {
			return Instance{}, fmt.Errorf("instance %s is %s; stop and commit it to create from it", from.Name, state)
		}
		spec.From = &from
	}
	if spec.Name == "" {
		spec.Name = fsutil.RandomHex(6)
	}
	for _, f := range opts.Forwards {
		if f.Peer() && f.Instance == spec.Name {
			return Instance{}, fmt.Errorf("port %d forwards to instance %s itself", f.Port, f.Instance)
		}
	}
	return e.Driver.Create(ctx, spec)
}

// Get finds an instance by name, or by its provider's ID or a unique prefix
// of one.
func (e *Engine) Get(ctx context.Context, ref string) (Instance, error) {
	inst, err := e.Driver.Instance(ctx, ref)
	if err == nil {
		return inst, nil
	}
	if !errors.Is(err, machine.ErrNotFound) {
		return Instance{}, err
	}
	all, err := e.Driver.Instances(ctx)
	if err != nil {
		return Instance{}, err
	}
	var match []Instance
	for _, candidate := range all {
		if candidate.ID == ref {
			return candidate, nil
		}
		if strings.HasPrefix(candidate.ID, ref) {
			match = append(match, candidate)
		}
	}
	switch len(match) {
	case 1:
		return match[0], nil
	case 0:
		return Instance{}, fmt.Errorf("%q: %w", ref, ErrNotFound)
	default:
		return Instance{}, fmt.Errorf("%q matches %d instances", ref, len(match))
	}
}

// List returns the project's instances.
func (e *Engine) List(ctx context.Context) ([]Instance, error) {
	return e.Driver.Instances(ctx)
}

// State reports whether an instance is running, as its driver finds it.
func (e *Engine) State(ctx context.Context, inst Instance) State {
	ctx, cancel := context.WithTimeout(ctx, time.Minute)
	defer cancel()
	_, err := e.Driver.Attach(ctx, inst)
	switch {
	case err == nil:
		return Running
	case errors.Is(err, machine.ErrNotRunning):
		return Stopped
	default:
		return Unknown
	}
}

// StartOptions bounds a start.
type StartOptions struct {
	// Timeout covers boot and the guest answering.
	Timeout time.Duration
	// GUI opens a window on the guest's display for this boot, in the
	// process that holds the machine.
	GUI bool
}

// checkForwards refuses forwards the driver cannot serve or that make no
// sense: each goes to exactly one of an absolute host socket, or an instance
// of the project and a port.
func (e *Engine) checkForwards(forwards []machine.Forward) error {
	if len(forwards) == 0 {
		return nil
	}
	if !e.Driver.Capabilities().Ports {
		return fmt.Errorf("driver %s cannot forward a guest's connections: %w", e.Driver.Name(), machine.ErrUnsupported)
	}
	ports := map[uint32]bool{}
	for _, f := range forwards {
		switch {
		case f.Port == 0:
			return errors.New("a forward needs a port")
		case f.Socket != "" && f.Peer():
			return fmt.Errorf("port %d forwards to both a socket and instance %s", f.Port, f.Instance)
		case f.Peer() && machine.ValidateName(f.Instance) != nil:
			return fmt.Errorf("port %d forwards to %q, which is not an instance name", f.Port, f.Instance)
		case f.Peer() && f.InstancePort == 0:
			return fmt.Errorf("port %d forwards to instance %s with no port", f.Port, f.Instance)
		case !f.Peer() && f.InstancePort != 0:
			return fmt.Errorf("port %d names a port of no instance", f.Port)
		case !f.Peer() && !filepath.IsAbs(f.Socket):
			return fmt.Errorf("port %d forwards to %q, which is not an absolute path", f.Port, f.Socket)
		case ports[f.Port]:
			return fmt.Errorf("port %d is forwarded twice", f.Port)
		}
		ports[f.Port] = true
	}
	return nil
}

// CheckGUI refuses a window from a driver that has no display to show.
func (e *Engine) CheckGUI(gui bool) error {
	if gui && !e.Driver.Capabilities().Display {
		return fmt.Errorf("driver %s has no display to show: %w", e.Driver.Name(), machine.ErrUnsupported)
	}
	return nil
}

// Start boots an instance so that it outlives this process, waits for its
// guest, and tells the guest where its service listens.
func (e *Engine) Start(ctx context.Context, inst Instance, opts StartOptions) error {
	if e.State(ctx, inst) == Running {
		return fmt.Errorf("instance %s is already running", inst.Name)
	}
	if err := e.checkCapacity(ctx, inst); err != nil {
		return err
	}
	if err := e.CheckGUI(opts.GUI); err != nil {
		return err
	}
	if opts.Timeout == 0 {
		opts.Timeout = 10 * time.Minute
	}
	ctx, cancel := context.WithTimeout(ctx, opts.Timeout)
	defer cancel()
	m, err := e.Driver.Start(ctx, inst, machine.BootOptions{GUI: opts.GUI, Title: "disco-vm: " + inst.Name})
	if err != nil {
		return fmt.Errorf("instance %s failed to start: %w", inst.Name, err)
	}
	if err := e.announceService(ctx, inst, m); err != nil {
		_ = m.Kill(context.Background())
		return err
	}
	return nil
}

// checkCapacity enforces the driver's per-OS cap on running guests, which is
// a limit of the hypervisor (two macOS guests under Virtualization.framework),
// not a policy.
func (e *Engine) checkCapacity(ctx context.Context, inst Instance) error {
	limit := e.Driver.Capabilities().MaxRunning[inst.GuestOS]
	if limit == 0 {
		return nil
	}
	all, err := e.Driver.Instances(ctx)
	if err != nil {
		return err
	}
	running := 0
	for _, other := range all {
		if other.GuestOS == inst.GuestOS && other.ID != inst.ID && e.State(ctx, other) == Running {
			running++
		}
	}
	if running >= limit {
		return fmt.Errorf("driver %s runs at most %d %s guests at once, and %d are running", e.Driver.Name(), limit, inst.GuestOS, running)
	}
	return nil
}

// Stop shuts an instance down in order, forcing it off after timeout.
func (e *Engine) Stop(ctx context.Context, inst Instance, timeout time.Duration) error {
	m, err := e.Driver.Attach(ctx, inst)
	if errors.Is(err, machine.ErrNotRunning) {
		return nil
	}
	if err != nil {
		return err
	}
	return (&Booted{Machine: m}).Shutdown(ctx, timeout)
}

// Remove deletes an instance. A running one is refused unless force, which
// stops it first.
func (e *Engine) Remove(ctx context.Context, inst Instance, force bool) error {
	if state := e.State(ctx, inst); state != Stopped {
		if !force {
			return fmt.Errorf("instance %s is %s; stop it or remove it with --force", inst.Name, state)
		}
		// A guest forced off after the timeout is still off, and removing it
		// is what was asked for.
		if err := e.Stop(ctx, inst, 30*time.Second); err != nil && e.State(ctx, inst) == Running {
			return err
		}
	}
	return e.Driver.Destroy(ctx, inst)
}

// Guest is a running instance's guest operations, attached with this
// process's credentials.
func (e *Engine) Guest(ctx context.Context, inst Instance) (machine.Guest, error) {
	m, err := e.Driver.Attach(ctx, inst)
	if err != nil {
		if errors.Is(err, machine.ErrNotRunning) {
			return nil, fmt.Errorf("instance %s is not running", inst.Name)
		}
		return nil, fmt.Errorf("instance %s: %w", inst.Name, err)
	}
	return m, nil
}

// Dial opens a stream to a guest port of a running instance, through the
// process that holds its machine, on a driver that lists
// Capabilities.Ports. The stream is the guest's: whatever listens on the port
// authenticates its callers.
func (e *Engine) Dial(ctx context.Context, inst Instance, port uint32) (net.Conn, error) {
	if !e.Driver.Capabilities().Ports {
		return nil, fmt.Errorf("driver %s reaches no guest port: %w", e.Driver.Name(), machine.ErrUnsupported)
	}
	if port == 0 {
		return nil, errors.New("dial: a guest port is 1 or more")
	}
	m, err := e.Driver.Attach(ctx, inst)
	if err != nil {
		if errors.Is(err, machine.ErrNotRunning) {
			return nil, fmt.Errorf("instance %s is not running", inst.Name)
		}
		return nil, fmt.Errorf("instance %s: %w", inst.Name, err)
	}
	conn, err := m.Dial(ctx, port)
	if err != nil {
		return nil, fmt.Errorf("instance %s port %d: %w", inst.Name, port, err)
	}
	return conn, nil
}

// URL is where an instance's one service is reached over HTTP: boxd's public
// HTTPS URL, the same across stop and start, or a loopback port the Docker
// daemon publishes or the instance's shim serves while it runs, a new one at
// every start. None needs a credential, so the service authenticates its
// callers.
func (e *Engine) URL(ctx context.Context, inst Instance) (string, error) {
	service, err := e.Driver.Service(ctx, inst)
	if errors.Is(err, machine.ErrNotRunning) {
		return "", fmt.Errorf("instance %s is not running", inst.Name)
	}
	if err != nil {
		return "", err
	}
	if service.URL == "" {
		return "", fmt.Errorf("instance %s: the %s driver gave its service no URL", inst.Name, e.Driver.Name())
	}
	return service.URL, nil
}

// PrivateURL is where the instance's peers, other instances of the driver on
// its networks, reach its service (machine.Service.PrivateURL): a direct
// path to the instance's address, or URL where that is how peers reach it.
// It is refused while the instance is not running, as URL is, and where the
// driver gives peers no way to it.
func (e *Engine) PrivateURL(ctx context.Context, inst Instance) (string, error) {
	service, err := e.Driver.Service(ctx, inst)
	if errors.Is(err, machine.ErrNotRunning) {
		return "", fmt.Errorf("instance %s is not running", inst.Name)
	}
	if err != nil {
		return "", err
	}
	if service.PrivateURL == "" {
		return "", fmt.Errorf("instance %s: the %s driver gives its instances no way to each other: %w", inst.Name, e.Driver.Name(), machine.ErrUnsupported)
	}
	return service.PrivateURL, nil
}

// announceService tells a started guest where its service listens, in its
// guest.ServiceFile, before the start returns.
func (e *Engine) announceService(ctx context.Context, inst Instance, g machine.Guest) error {
	service, err := e.Driver.Service(ctx, inst)
	if err != nil && !errors.Is(err, machine.ErrNotRunning) {
		return err
	}
	if err := guest.SetService(ctx, g, string(inst.GuestOS), service.Listen); err != nil {
		return fmt.Errorf("instance %s: %w", inst.Name, err)
	}
	return nil
}

// Booted is a machine running in this process, or one attached to for as long
// as one call holds it.
type Booted struct {
	Machine machine.Machine
}

// BootOptions is one boot in this process.
type BootOptions struct {
	// Console receives the driver's log of the boot.
	Console io.Writer
	// GUI opens a window on the guest's display, in this process.
	GUI bool
	// Title names the window; it defaults to the instance's name.
	Title string
}

// Boot starts an instance in this process, and returns once its guest
// answers.
func (e *Engine) Boot(ctx context.Context, inst Instance, opts BootOptions) (*Booted, error) {
	if err := e.CheckGUI(opts.GUI); err != nil {
		return nil, err
	}
	title := opts.Title
	if title == "" {
		title = "disco-vm: " + inst.Name
	}
	m, err := e.Driver.Boot(ctx, inst, machine.BootOptions{Console: opts.Console, GUI: opts.GUI, Title: title})
	if err != nil {
		return nil, err
	}
	return &Booted{Machine: m}, nil
}

// Shutdown powers a booted machine off in order: the guest is asked, and the
// machine is killed only if it will not answer or will not stop in time. A
// hard stop is a dirty unmount of the guest's disk, and a VM committed from one
// is not trustworthy, so every stop comes through here. timeout
// bounds the request and the stop together: a driver may do real work before
// its guest accepts (boxd flushes the guest's disks, then asks StopVm).
func (b *Booted) Shutdown(ctx context.Context, timeout time.Duration) error {
	select {
	case <-b.Machine.Done():
		return b.Machine.Err()
	default:
	}
	deadline := time.Now().Add(timeout)
	askCtx, cancel := context.WithDeadline(ctx, deadline)
	err := b.Machine.Shutdown(askCtx, false)
	cancel()
	if err == nil {
		select {
		case <-b.Machine.Done():
			return b.Machine.Err()
		case <-time.After(time.Until(deadline)):
			err = fmt.Errorf("guest did not power off within %s", timeout)
		case <-ctx.Done():
			err = ctx.Err()
		}
	}
	if killErr := b.Machine.Kill(context.Background()); killErr != nil {
		return fmt.Errorf("not stopped in order (%w), nor forced off: %w", err, killErr)
	}
	return fmt.Errorf("forced off: %w", err)
}

// InstallOptions is a VM installed from installation media.
type InstallOptions struct {
	// GuestOS is the OS to install; empty is the driver's only one.
	GuestOS machine.OS
	// Media is a Windows ISO or a macOS IPSW, or "latest" where the driver
	// can fetch one itself. A relative path is the working directory's.
	Media string
	// Edition selects an image inside multi-image media (install.wim).
	Edition   string
	DiskBytes int64
	CPUs      int
	Memory    uint64
	// Options are the driver's own, passed through untouched.
	Options map[string]string
	// GUI shows every boot of the install in a native window.
	GUI bool
	// Log receives the driver's progress.
	Log io.Writer
}

// Install makes the VM name from installation media, committed, on a driver
// that lists the guest OS in Capabilities.Install: VMs are created from it,
// and the caller never starts it. Where the driver puts the agent in the
// guest, it is this binary, so the guest's OS must be this host's.
func (e *Engine) Install(ctx context.Context, name string, opts InstallOptions) (Instance, error) {
	caps := e.Driver.Capabilities()
	if len(caps.Install) == 0 {
		return Instance{}, fmt.Errorf("driver %s installs nothing from media; create a fresh VM of its base instead: %w", e.Driver.Name(), machine.ErrUnsupported)
	}
	if opts.GuestOS == "" && len(caps.Install) == 1 {
		opts.GuestOS = caps.Install[0]
	}
	if !caps.Installs(opts.GuestOS) {
		return Instance{}, fmt.Errorf("driver %s cannot install %q guests from media (it installs %v): %w", e.Driver.Name(), opts.GuestOS, caps.Install, machine.ErrUnsupported)
	}
	if err := machine.ValidateName(name); err != nil {
		return Instance{}, err
	}
	if err := e.CheckGUI(opts.GUI); err != nil {
		return Instance{}, err
	}
	agent := ""
	if caps.Agent {
		if opts.GuestOS != machine.OS(runtime.GOOS) {
			return Instance{}, fmt.Errorf("a %s guest's agent is disco-vm built for %s, and this one is built for %s", opts.GuestOS, opts.GuestOS, runtime.GOOS)
		}
		exe, err := os.Executable()
		if err != nil {
			return Instance{}, err
		}
		agent = exe
	}
	media := opts.Media
	if media == "" {
		media = "latest"
	}
	if media != "latest" {
		abs, err := filepath.Abs(media)
		if err != nil {
			return Instance{}, err
		}
		media = abs
	}
	inst, err := e.Driver.Install(ctx, machine.InstallSpec{
		GuestOS: opts.GuestOS, Media: media, Edition: opts.Edition,
		DiskBytes: opts.DiskBytes, CPUs: opts.CPUs, Memory: opts.Memory,
		Agent: agent, Options: opts.Options, GUI: opts.GUI, Log: opts.Log,
	}, name)
	if err != nil {
		return Instance{}, fmt.Errorf("install %s: %w", opts.GuestOS, err)
	}
	return inst, nil
}

// Commit freezes a stopped VM under its name, so VMs are created from it
// (ADR 26-11-03-606): its data volume and cache clone are removed. The
// caller records that it is committed, and never starts it again; to
// change it, create a VM from it, change that, and commit it under a new
// name. A driver that does not report Capabilities.Commit is refused with
// ErrUnsupported before it is asked, and the VM is left as it was (ADR
// 26-11-03-868).
func (e *Engine) Commit(ctx context.Context, inst Instance) (Instance, error) {
	if !e.Driver.Capabilities().Commit {
		return Instance{}, fmt.Errorf("driver %s cannot commit a VM: %w", e.Driver.Name(), machine.ErrUnsupported)
	}
	if state := e.State(ctx, inst); state != Stopped {
		return Instance{}, fmt.Errorf("instance %s is %s; stop it first", inst.Name, state)
	}
	committed, err := e.Driver.Commit(ctx, inst)
	if err != nil {
		return Instance{}, fmt.Errorf("commit %s: %w", inst.Name, err)
	}
	return committed, nil
}

// UpgradeOptions names the root an upgrade gives a VM.
type UpgradeOptions struct {
	// From names the committed VM the new root is a copy of; empty is a
	// fresh root from the driver's base. That it is committed is the
	// caller's to know.
	From string
}

// Upgrade gives the stopped VM name a new root, and keeps its name, its
// settings, its data volume, and its cache clone (ADR 26-11-03-782): its old
// root is removed for good. A caller that wants to go back commits the VM
// first and upgrades to that. An upgrade that was interrupted is finished by
// upgrading again with the same From. The driver must list
// machine.VolumeData.
func (e *Engine) Upgrade(ctx context.Context, name string, opts UpgradeOptions) (Instance, error) {
	if !e.Driver.Capabilities().Attaches(machine.VolumeData) {
		return Instance{}, fmt.Errorf("driver %s attaches no data volume, which an upgrade keeps; rebuild the VM in place instead: %w", e.Driver.Name(), machine.ErrUnsupported)
	}
	inst, err := e.Get(ctx, name)
	if err != nil {
		return Instance{}, err
	}
	if state := e.State(ctx, inst); state != Stopped {
		return Instance{}, fmt.Errorf("instance %s is %s; stop it to upgrade it", inst.Name, state)
	}
	var from *Instance
	if opts.From != "" {
		src, err := e.Get(ctx, opts.From)
		switch {
		case errors.Is(err, ErrNotFound):
			// An upgrade caught between its roots once its new root was
			// made finishes without the committed VM it is from, which may
			// be gone: the driver says whether it still needs it.
			src = Instance{Name: opts.From, GuestOS: inst.GuestOS}
		case err != nil:
			return Instance{}, err
		}
		if src.Name == inst.Name {
			return Instance{}, fmt.Errorf("instance %s cannot be upgraded from itself", inst.Name)
		}
		if src.GuestOS != inst.GuestOS {
			return Instance{}, fmt.Errorf("instance %s is a %s guest, and %s a %s one: a data volume is read by its guest's OS", inst.Name, inst.GuestOS, src.Name, src.GuestOS)
		}
		// One not found has no state, and is the driver's to refuse.
		if src.ID != "" {
			if state := e.State(ctx, src); state != Stopped {
				return Instance{}, fmt.Errorf("instance %s is %s; stop and commit it to upgrade from it", src.Name, state)
			}
		}
		from = &src
	}
	upgraded, err := e.Driver.Upgrade(ctx, inst, from)
	if err != nil {
		return Instance{}, fmt.Errorf("upgrade %s: %w", inst.Name, err)
	}
	return upgraded, nil
}
