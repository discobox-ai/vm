// Package engine is disco-vm as a library: images, instances, and their
// lifecycle over one machine.Driver. The CLI is a thin layer over it, and a
// discobox provider embeds it the same way.
//
// The engine keeps no state (ADR 26-10-23-417). Every image, tag, and
// instance is the driver's, which keeps it in its provider, so any process
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
	"path/filepath"
	"strings"
	"time"

	"github.com/discobox-ai/vm/internal/fsutil"
	"github.com/discobox-ai/vm/pkg/guest"
	"github.com/discobox-ai/vm/pkg/machine"
)

// Engine is one project, over a driver made for it.
type Engine struct {
	Driver machine.Driver
	// Project is the driver's project, which every image and instance the
	// engine reads and makes is in.
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

// Instance is one machine made from an image, as its driver keeps it.
type Instance = machine.Instance

// CreateOptions describes a new instance.
type CreateOptions struct {
	// Name is the instance's name, unique in the project; empty is a random
	// one.
	Name   string
	CPUs   int
	Memory uint64
	// Temporary makes a build's instance, which listings leave out.
	Temporary bool
	// Forwards are served whenever the instance runs; the driver must list
	// Capabilities.Forward.
	Forwards []machine.Forward
}

// Create makes an instance from an image reference or layer ID, or from one of
// the driver's bases by its name or ID, which it runs from with no layer.
func (e *Engine) Create(ctx context.Context, ref string, opts CreateOptions) (Instance, error) {
	if err := e.checkForwards(opts.Forwards); err != nil {
		return Instance{}, err
	}
	spec := machine.InstanceSpec{Name: opts.Name, Image: ref, CPUs: opts.CPUs, Memory: opts.Memory, Forwards: opts.Forwards}
	layer, err := e.Resolve(ctx, ref)
	if err != nil {
		b, err := e.ResolveBase(ctx, ref, err)
		if err != nil {
			return Instance{}, err
		}
		spec.Base, spec.GuestOS = &b, b.GuestOS
	} else {
		spec.Layer, spec.GuestOS = layer.ID, layer.GuestOS
	}
	switch {
	case opts.Temporary:
		spec.Name = machine.TemporaryPrefix + fsutil.RandomHex(6)
	case spec.Name == "":
		spec.Name = fsutil.RandomHex(6)
	case strings.HasPrefix(spec.Name, machine.TemporaryPrefix):
		return Instance{}, fmt.Errorf("instance name %q starts with %q, which names a build's instances", spec.Name, machine.TemporaryPrefix)
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

// List returns the instances, excluding a build's temporary ones.
func (e *Engine) List(ctx context.Context) ([]Instance, error) {
	all, err := e.Driver.Instances(ctx)
	if err != nil {
		return nil, err
	}
	var out []Instance
	for _, inst := range all {
		if !inst.Temporary() {
			out = append(out, inst)
		}
	}
	return out, nil
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

// checkForwards refuses forwards the driver cannot serve or that make no sense.
func (e *Engine) checkForwards(forwards []machine.Forward) error {
	if len(forwards) == 0 {
		return nil
	}
	if !e.Driver.Capabilities().Forward {
		return fmt.Errorf("driver %s cannot forward a guest's connections to the host: %w", e.Driver.Name(), machine.ErrUnsupported)
	}
	ports := map[uint32]bool{}
	for _, f := range forwards {
		switch {
		case f.Port == 0:
			return errors.New("a forward needs a port")
		case !filepath.IsAbs(f.Socket):
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

// Booted is a machine running in this process. The builder boots this way.
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
// hard stop is a dirty unmount of the guest's disk, and a layer cut from one
// is not trustworthy, so the builder always comes through here. timeout
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

// Commit captures a stopped instance as the layer layer, on the instance's
// own, and removes the instance.
func (e *Engine) Commit(ctx context.Context, inst Instance, layer machine.Layer) (machine.Layer, error) {
	layer.Parent, layer.GuestOS = inst.Layer, inst.GuestOS
	return e.Driver.Commit(ctx, inst, layer)
}

// Snapshot commits a stopped instance's disk as a new layer on its image's
// layer, removes the instance, and tags the layer ref when ref is not empty.
// The instance is consumed, as a build's is, so there is one way a layer is
// cut; to go on from a snapshot, create an instance of it. The layer's ID is
// random: nothing reproducible made it, so it is never a cache hit.
func (e *Engine) Snapshot(ctx context.Context, inst Instance, ref string) (machine.Layer, error) {
	if inst.Temporary() {
		return machine.Layer{}, fmt.Errorf("instance %s belongs to a build", inst.Name)
	}
	if ref != "" {
		if err := ValidateRef(ref); err != nil {
			return machine.Layer{}, err
		}
	}
	if state := e.State(ctx, inst); state != Stopped {
		return machine.Layer{}, fmt.Errorf("instance %s is %s; stop it first", inst.Name, state)
	}
	layer, err := e.Commit(ctx, inst, machine.Layer{ID: fsutil.RandomHex(32), Comment: "snapshot " + inst.Name})
	if err != nil {
		return machine.Layer{}, fmt.Errorf("snapshot %s: %w", inst.Name, err)
	}
	if ref != "" {
		if err := e.Tag(ctx, ref, layer.ID); err != nil {
			return layer, fmt.Errorf("snapshot %s is layer %s, but tag %s: %w", inst.Name, Short(layer.ID), ref, err)
		}
	}
	return layer, nil
}

// Discard removes an instance without asking its state; the builder uses it
// on temporary instances it knows are stopped.
func (e *Engine) Discard(ctx context.Context, inst Instance) {
	_ = e.Driver.Destroy(ctx, inst)
}
