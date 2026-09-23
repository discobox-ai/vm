// Package engine is disco-vm as a library: images, instances, and their
// lifecycle over one machine.Driver. The CLI is a thin layer over it, and a
// discobox provider embeds it the same way.
//
// There is no daemon. Each running instance is owned by its own shim process
// (`disco-vm shim <id>`), which boots the machine, holds it for its whole life,
// and serves a small control API on loopback: status, stop, and a dial that
// splices a stream to any guest port. The shim exists because the two
// hypervisors disagree about who owns a running VM — a Virtualization.framework
// VM dies with the process that made it, while an HCS VM belongs to vmcompute —
// and a process per VM makes both look the same: the VM lives exactly as long
// as its shim, crashes are contained to one VM, and any process (a CLI
// invocation, a discobox server) reaches any instance through the same socket.
package engine

import (
	"context"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"net"
	"os"
	"path/filepath"
	"runtime"
	"sort"
	"strings"
	"time"

	"github.com/discobox-ai/vm/internal/fsutil"
	"github.com/discobox-ai/vm/pkg/guest"
	"github.com/discobox-ai/vm/pkg/image"
	"github.com/discobox-ai/vm/pkg/machine"
)

// Engine is one disco-vm state root and driver.
type Engine struct {
	Root   string
	Driver machine.Driver
	Images *image.Store
	// Exe is the disco-vm binary, started as each instance's shim.
	Exe string
}

// RootEnv overrides DefaultRoot.
const RootEnv = "DISCO_VM_ROOT"

// DefaultRoot is where disco-vm keeps its state: $DISCO_VM_ROOT, or
// %LOCALAPPDATA%\disco-vm on Windows, ~/Library/Application Support/disco-vm
// on macOS, and $XDG_DATA_HOME/disco-vm elsewhere.
func DefaultRoot() string {
	if root := os.Getenv(RootEnv); root != "" {
		return root
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

// Open opens (creating) a state root for a driver.
func Open(root string, driver machine.Driver) (*Engine, error) {
	if root == "" {
		root = DefaultRoot()
	}
	root, err := filepath.Abs(root)
	if err != nil {
		return nil, err
	}
	store, err := image.Open(filepath.Join(root, "images"))
	if err != nil {
		return nil, err
	}
	if err := os.MkdirAll(filepath.Join(root, "instances"), 0o755); err != nil {
		return nil, err
	}
	exe, err := os.Executable()
	if err != nil {
		return nil, err
	}
	return &Engine{Root: root, Driver: driver, Images: store, Exe: exe}, nil
}

// ErrNotFound reports an unknown instance.
var ErrNotFound = errors.New("instance not found")

// State is an instance's power state.
type State string

const (
	Stopped State = "stopped"
	Running State = "running"
)

// Instance is one VM made from an image.
type Instance struct {
	ID      string            `json:"id"`
	Name    string            `json:"name"`
	Image   string            `json:"image"`
	Layer   string            `json:"layer"`
	Driver  string            `json:"driver"`
	GuestOS machine.OS        `json:"guestOS"`
	Mode    machine.CloneMode `json:"mode"`
	CPUs    int               `json:"cpus,omitempty"`
	Memory  uint64            `json:"memory,omitempty"`
	Created time.Time         `json:"created"`
	// Temporary instances belong to a build and are not listed.
	Temporary bool `json:"temporary,omitempty"`
}

func (e *Engine) instanceDir(id string) string { return filepath.Join(e.Root, "instances", id) }

// machineSpec is the driver's view of an instance.
func (e *Engine) machineSpec(inst *Instance) (machine.InstanceSpec, error) {
	chain, err := e.Images.MachineChain(inst.Layer)
	if err != nil {
		return machine.InstanceSpec{}, err
	}
	spec := machine.InstanceSpec{
		ID:      inst.ID,
		Dir:     filepath.Join(e.instanceDir(inst.ID), "machine"),
		GuestOS: inst.GuestOS,
		Chain:   chain,
		Mode:    inst.Mode,
	}
	if inst.Mode != machine.Cold {
		spec.WarmDir = e.warmMachineDir(inst.Layer)
	}
	return spec, nil
}

// CreateOptions describes a new instance.
type CreateOptions struct {
	Name string
	// Mode is how to clone the image. Empty means machine.Auto: the fastest
	// mode the image is warm for, else cold. A fast mode named explicitly
	// fails when the image is not warm for it.
	Mode      machine.CloneMode
	CPUs      int
	Memory    uint64
	Temporary bool
}

// Create makes an instance from an image reference or layer ID.
func (e *Engine) Create(ctx context.Context, ref string, opts CreateOptions) (*Instance, error) {
	layerID, err := e.Images.Resolve(ref)
	if err != nil {
		return nil, err
	}
	layer, err := e.Images.Layer(layerID)
	if err != nil {
		return nil, err
	}
	if layer.Driver != e.Driver.Name() {
		return nil, fmt.Errorf("image %s was built for driver %s, not %s", ref, layer.Driver, e.Driver.Name())
	}
	mode, err := e.cloneMode(ctx, ref, layerID, opts)
	if err != nil {
		return nil, err
	}
	id := fsutil.RandomHex(6)
	name := opts.Name
	if name == "" {
		name = id
	} else if _, err := e.Get(name); err == nil {
		return nil, fmt.Errorf("an instance named %q already exists", name)
	}
	inst := &Instance{
		ID: id, Name: name, Image: ref, Layer: layerID,
		Driver: e.Driver.Name(), GuestOS: layer.GuestOS, Mode: mode,
		CPUs: opts.CPUs, Memory: opts.Memory,
		Created: time.Now().UTC(), Temporary: opts.Temporary,
	}
	if err := os.MkdirAll(e.instanceDir(id), 0o755); err != nil {
		return nil, err
	}
	spec, err := e.machineSpec(inst)
	if err == nil {
		err = e.Driver.Prepare(ctx, spec)
	}
	if errors.Is(err, machine.ErrNotWarm) && (opts.Mode == "" || opts.Mode == machine.Auto) {
		// The stage went between asking and taking (another clone took the
		// last one, or it was stale), and auto means whatever is fastest now.
		_ = e.Driver.Destroy(ctx, spec)
		inst.Mode, spec.Mode, spec.WarmDir = machine.Cold, machine.Cold, ""
		err = e.Driver.Prepare(ctx, spec)
	}
	if err == nil {
		err = fsutil.WriteJSON(filepath.Join(e.instanceDir(id), "instance.json"), inst)
	}
	if err != nil {
		if spec.Dir != "" {
			_ = e.Driver.Destroy(ctx, spec)
		}
		_ = os.RemoveAll(e.instanceDir(id))
		return nil, err
	}
	return inst, nil
}

// Get finds an instance by ID, unique ID prefix, or name.
func (e *Engine) Get(ref string) (*Instance, error) {
	var inst Instance
	if err := fsutil.ReadJSON(filepath.Join(e.instanceDir(ref), "instance.json"), &inst); err == nil {
		return &inst, nil
	}
	all, err := e.list(true)
	if err != nil {
		return nil, err
	}
	// A name is exact, so it wins over an ID that merely starts the same way.
	for _, candidate := range all {
		if candidate.Name == ref {
			return candidate, nil
		}
	}
	var match []*Instance
	for _, candidate := range all {
		if strings.HasPrefix(candidate.ID, ref) {
			match = append(match, candidate)
		}
	}
	switch len(match) {
	case 1:
		return match[0], nil
	case 0:
		return nil, fmt.Errorf("%q: %w", ref, ErrNotFound)
	default:
		return nil, fmt.Errorf("%q matches %d instances", ref, len(match))
	}
}

// List returns the instances, excluding a build's temporary ones.
func (e *Engine) List() ([]*Instance, error) { return e.list(false) }

func (e *Engine) list(all bool) ([]*Instance, error) {
	entries, err := os.ReadDir(filepath.Join(e.Root, "instances"))
	if err != nil {
		return nil, err
	}
	var out []*Instance
	for _, entry := range entries {
		var inst Instance
		if err := fsutil.ReadJSON(filepath.Join(e.Root, "instances", entry.Name(), "instance.json"), &inst); err != nil {
			continue
		}
		if inst.Temporary && !all {
			continue
		}
		out = append(out, &inst)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Created.Before(out[j].Created) })
	return out, nil
}

// State reports whether an instance's shim is up.
func (e *Engine) State(ctx context.Context, inst *Instance) State {
	shim, err := e.shim(inst.ID)
	if err != nil {
		return Stopped
	}
	ctx, cancel := context.WithTimeout(ctx, 2*time.Second)
	defer cancel()
	if err := shim.status(ctx); err != nil {
		return Stopped
	}
	return Running
}

// LayerInUse reports whether any instance is derived from a layer, for rmi.
func (e *Engine) LayerInUse(id string) bool {
	all, err := e.list(true)
	if err != nil {
		return true
	}
	for _, inst := range all {
		chain, err := e.Images.Chain(inst.Layer)
		if err != nil {
			continue
		}
		for _, layer := range chain {
			if layer.ID == id {
				return true
			}
		}
	}
	return false
}

// StartOptions bounds a start.
type StartOptions struct {
	// Timeout covers boot and the agent answering.
	Timeout time.Duration
}

// Start boots an instance under a new shim and waits for its agent.
func (e *Engine) Start(ctx context.Context, inst *Instance, opts StartOptions) error {
	if e.State(ctx, inst) == Running {
		return fmt.Errorf("instance %s is already running", inst.Name)
	}
	if err := e.checkCapacity(ctx, inst); err != nil {
		return err
	}
	if opts.Timeout == 0 {
		opts.Timeout = 10 * time.Minute
	}
	dir := e.instanceDir(inst.ID)
	_ = os.Remove(filepath.Join(dir, shimStateName))
	logFile, err := os.OpenFile(filepath.Join(dir, "shim.log"), os.O_WRONLY|os.O_CREATE|os.O_APPEND, 0o600)
	if err != nil {
		return err
	}
	defer logFile.Close()
	exited, err := spawnShim(e.Exe, e.Root, e.Driver.Name(), []string{inst.ID}, logFile)
	if err != nil {
		return err
	}

	ctx, cancel := context.WithTimeout(ctx, opts.Timeout)
	defer cancel()
	for {
		if _, err := e.shim(inst.ID); err == nil {
			break
		}
		select {
		case err := <-exited:
			return fmt.Errorf("instance %s failed to start: %v\n%s", inst.Name, err, tail(filepath.Join(dir, "shim.log")))
		case <-ctx.Done():
			return fmt.Errorf("instance %s: shim did not come up: %w", inst.Name, ctx.Err())
		case <-time.After(50 * time.Millisecond):
		}
	}
	client := e.Guest(inst)
	defer client.Close()
	stopped := make(chan struct{})
	go func() {
		<-exited
		close(stopped)
	}()
	return client.WaitReady(ctx, stopped)
}

// checkCapacity enforces the driver's per-OS cap on running guests, which is
// a limit of the hypervisor (two macOS guests under Virtualization.framework),
// not a policy.
func (e *Engine) checkCapacity(ctx context.Context, inst *Instance) error {
	limit := e.Driver.Capabilities().MaxRunning[inst.GuestOS]
	if limit == 0 {
		return nil
	}
	all, err := e.list(true)
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
func (e *Engine) Stop(ctx context.Context, inst *Instance, timeout time.Duration) error {
	shim, err := e.shim(inst.ID)
	if err != nil {
		return nil // not running
	}
	return shim.stop(ctx, timeout)
}

// Remove deletes an instance. A running one is refused unless force, which
// stops it first.
func (e *Engine) Remove(ctx context.Context, inst *Instance, force bool) error {
	if e.State(ctx, inst) == Running {
		if !force {
			return fmt.Errorf("instance %s is running; stop it or remove it with --force", inst.Name)
		}
		// A guest forced off after the timeout is still off, and removing it
		// is what was asked for.
		if err := e.Stop(ctx, inst, 30*time.Second); err != nil && e.State(ctx, inst) == Running {
			return err
		}
	}
	if spec, err := e.machineSpec(inst); err == nil {
		if err := e.Driver.Destroy(ctx, spec); err != nil {
			return err
		}
	}
	return os.RemoveAll(e.instanceDir(inst.ID))
}

// Guest returns a client for a running instance's agent, reached through its
// shim.
func (e *Engine) Guest(inst *Instance) *guest.Client {
	return guest.NewClient(func(ctx context.Context) (net.Conn, error) {
		shim, err := e.shim(inst.ID)
		if err != nil {
			return nil, err
		}
		return shim.dial(ctx, guest.AgentPort)
	})
}

// Dial opens a stream to any port of a running instance.
func (e *Engine) Dial(ctx context.Context, inst *Instance, port uint32) (net.Conn, error) {
	shim, err := e.shim(inst.ID)
	if err != nil {
		return nil, fmt.Errorf("instance %s is not running", inst.Name)
	}
	return shim.dial(ctx, port)
}

// Booted is a machine running in this process, with a client for its agent.
// The builder boots this way; the shim is the same thing in its own process.
type Booted struct {
	Machine machine.Machine
	Guest   *guest.Client
}

// Boot starts an instance in this process and waits for its agent.
func (e *Engine) Boot(ctx context.Context, inst *Instance, console io.Writer) (*Booted, error) {
	spec, err := e.machineSpec(inst)
	if err != nil {
		return nil, err
	}
	m, err := e.Driver.Boot(ctx, spec, machine.BootOptions{CPUs: inst.CPUs, Memory: inst.Memory, Console: console})
	if err != nil {
		return nil, err
	}
	client := guest.NewClient(func(ctx context.Context) (net.Conn, error) { return m.Dial(ctx, guest.AgentPort) })
	if err := client.WaitReady(ctx, m.Done()); err != nil {
		_ = m.Kill(context.Background())
		return nil, err
	}
	return &Booted{Machine: m, Guest: client}, nil
}

// Shutdown powers a booted machine off in order: the agent is asked, and the
// machine is killed only if it will not answer or will not stop in time. A
// hard stop is a dirty unmount of the guest's disk, and a layer cut from one
// is not trustworthy, so the builder always comes through here.
func (b *Booted) Shutdown(ctx context.Context, timeout time.Duration) error {
	defer b.Guest.Close()
	select {
	case <-b.Machine.Done():
		return b.Machine.Err()
	default:
	}
	askCtx, cancel := context.WithTimeout(ctx, 10*time.Second)
	err := b.Guest.Shutdown(askCtx, false)
	cancel()
	if err == nil {
		select {
		case <-b.Machine.Done():
			return b.Machine.Err()
		case <-time.After(timeout):
			err = fmt.Errorf("guest did not power off within %s", timeout)
		case <-ctx.Done():
			err = ctx.Err()
		}
	}
	_ = b.Machine.Kill(context.Background())
	return fmt.Errorf("forced off: %w", err)
}

// Commit captures a stopped temporary instance as a new layer and removes the
// instance.
func (e *Engine) Commit(ctx context.Context, inst *Instance, pending *image.Pending) (image.Layer, error) {
	spec, err := e.machineSpec(inst)
	if err != nil {
		return image.Layer{}, err
	}
	if err := e.Driver.Commit(ctx, spec, pending.MachineLayer()); err != nil {
		return image.Layer{}, err
	}
	layer, err := pending.Commit()
	if err != nil {
		return image.Layer{}, err
	}
	_ = e.Driver.Destroy(ctx, spec)
	_ = os.RemoveAll(e.instanceDir(inst.ID))
	return layer, nil
}

// Discard removes an instance without asking its state; the builder uses it
// on temporary instances it knows are stopped.
func (e *Engine) Discard(ctx context.Context, inst *Instance) {
	if spec, err := e.machineSpec(inst); err == nil {
		_ = e.Driver.Destroy(ctx, spec)
	}
	_ = os.RemoveAll(e.instanceDir(inst.ID))
}

func tail(path string) string {
	data, err := os.ReadFile(path)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return ""
		}
		return err.Error()
	}
	lines := strings.Split(strings.TrimRight(string(data), "\n"), "\n")
	if len(lines) > 20 {
		lines = lines[len(lines)-20:]
	}
	return strings.Join(lines, "\n")
}
