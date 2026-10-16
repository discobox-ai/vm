// Package engine is disco-vm as a library: images, instances, and their
// lifecycle over one machine.Driver. The CLI is a thin layer over it, and a
// discobox provider embeds it the same way.
//
// There is no daemon. Each running instance of a local hypervisor is owned by
// its own shim process (`disco-vm shim <id>`), which boots the machine, holds
// it for its whole life, and serves a small control API on loopback: status,
// stop, and a dial that splices a stream to any guest port. The shim exists
// because the two hypervisors disagree about who owns a running VM — a
// Virtualization.framework VM dies with the process that made it, while an HCS
// VM belongs to vmcompute — and a process per VM makes both look the same: the
// VM lives exactly as long as its shim, crashes are contained to one VM, and
// any process (a CLI invocation, a discobox server) reaches any instance
// through the same socket.
//
// A remote driver (Capabilities.Remote: boxd, docker) runs no shim. Its service owns
// the machine whatever process booted it, so each call attaches to the machine
// through the driver, with the calling process's own credentials, and nothing
// outlives the call.
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
	"strings"
	"time"

	"github.com/discobox-ai/x/gormdb"
	"gorm.io/gorm"
	"gorm.io/gorm/clause"

	"github.com/discobox-ai/vm/internal/fsutil"
	"github.com/discobox-ai/vm/internal/statedb"
	"github.com/discobox-ai/vm/pkg/guest"
	"github.com/discobox-ai/vm/pkg/image"
	"github.com/discobox-ai/vm/pkg/machine"
)

// Engine is one project of a disco-vm state root, and a driver.
type Engine struct {
	Root   string
	Driver machine.Driver
	// Project scopes every image and instance the engine reads and
	// makes, and the driver names what it makes for them after it.
	Project string
	Images  *image.Store
	// Exe is the disco-vm binary, started as each instance's shim.
	Exe string
	// ShimCommand is how a shim is started: a program and its leading
	// arguments, to which the shim's own ([--gui] INSTANCE) are appended.
	// Empty means Exe's `shim` subcommand. A program that embeds the engine
	// points it at a subcommand of its own that opens the same root, driver,
	// and project and calls ShimMain, so it ships no second binary.
	ShimCommand []string

	// db holds the metadata: the image store's tables and instances. ownsDB
	// is set when Open opened it, and Close then closes it.
	db     *gormdb.Pools
	ownsDB bool
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

// Open opens (creating) one project of a state root for a driver. db is the
// database its metadata is kept in, in tables named disco_vm_*: a program that
// embeds the engine passes its own, and keeps closing it. A nil db is a SQLite
// database in the root, which Close closes. The rows describe the files in one
// root, so a database serves one root, and any number of its projects. An
// empty project is machine.DefaultProject.
func Open(root string, driver machine.Driver, db *gormdb.Pools, project string) (*Engine, error) {
	if root == "" {
		root = DefaultRoot()
	}
	if project == "" {
		project = machine.DefaultProject
	}
	if err := machine.ValidateProject(project); err != nil {
		return nil, err
	}
	root, err := filepath.Abs(root)
	if err != nil {
		return nil, err
	}
	if _, ok := driver.(machine.Attacher); driver.Capabilities().Remote && !ok {
		return nil, fmt.Errorf("driver %s lists Capabilities.Remote but is not a machine.Attacher", driver.Name())
	}
	exe, err := os.Executable()
	if err != nil {
		return nil, err
	}
	if err := os.MkdirAll(filepath.Join(root, "instances"), 0o755); err != nil {
		return nil, err
	}
	e := &Engine{Root: root, Driver: driver, Project: project, Exe: exe, db: db}
	if db == nil {
		if e.db, err = statedb.Open(root); err != nil {
			return nil, err
		}
		e.ownsDB = true
	}
	if err := e.openState(); err != nil {
		_ = e.Close()
		return nil, err
	}
	return e, nil
}

// openState makes the image store and the engine's own tables, and takes in
// what an older disco-vm left in JSON files.
func (e *Engine) openState() error {
	store, err := image.Open(filepath.Join(e.Root, "images"), e.db, e.Project)
	if err != nil {
		return err
	}
	e.Images = store
	err = e.db.Write.Transaction(func(tx *gorm.DB) error {
		if err := tx.AutoMigrate(&Instance{}); err != nil {
			return err
		}
		// Warm stages are gone (ADR 26-10-15-851), and with them what was
		// asked of each. What a stage left in warm/ is the user's to delete.
		if err := tx.Migrator().DropTable(legacyWarmTable); err != nil {
			return err
		}
		// Before projects an instance's name was unique on its own.
		if tx.Migrator().HasIndex(&Instance{}, legacyNameIndex) {
			return tx.Migrator().DropIndex(&Instance{}, legacyNameIndex)
		}
		return nil
	})
	if err != nil {
		return fmt.Errorf("engine: migrate: %w", err)
	}
	return e.importFiles()
}

// Close releases the database Open opened. One the caller passed in stays
// open.
func (e *Engine) Close() error {
	if !e.ownsDB {
		return nil
	}
	return e.db.Close()
}

// importFiles moves the metadata a disco-vm from before the database kept in
// JSON files (instances/<id>/instance.json) into it.
// A file is removed once its row is in, so each is imported once.
func (e *Engine) importFiles() error {
	insert := func(row any) error {
		return e.db.Write.Clauses(clause.OnConflict{DoNothing: true}).Create(row).Error
	}
	entries, err := os.ReadDir(filepath.Join(e.Root, "instances"))
	if err != nil {
		return err
	}
	for _, entry := range entries {
		path := filepath.Join(e.instanceDir(entry.Name()), "instance.json")
		var inst Instance
		if err := fsutil.ReadJSON(path, &inst); err != nil || inst.ID != entry.Name() {
			continue
		}
		// Projects came after the files.
		inst.Project = machine.DefaultProject
		if err := insert(&inst); err != nil {
			return fmt.Errorf("engine: import %s: %w", path, err)
		}
		// Nothing was inserted for a row that is already there, and also for a
		// name another instance has: that file stays, so the instance is not
		// lost with it.
		if _, err := e.instance(inst.ID); err != nil {
			continue
		}
		if err := os.Remove(path); err != nil {
			return err
		}
	}
	return nil
}

// ErrNotFound reports an unknown instance.
var ErrNotFound = errors.New("instance not found")

// State is an instance's power state.
type State string

const (
	Stopped State = "stopped"
	Running State = "running"
	// Unknown is a remote instance whose service could not be asked.
	Unknown State = "unknown"
)

// Instance is one VM made from an image.
type Instance struct {
	ID string `json:"id" gorm:"primaryKey"`
	// Project is the instance's project; its name is unique within it.
	Project string `json:"project,omitempty" gorm:"not null;default:'default';uniqueIndex:idx_disco_vm_instances_project_name"`
	Name    string `json:"name" gorm:"uniqueIndex:idx_disco_vm_instances_project_name"`
	Image   string `json:"image"`
	// Layer is the image's layer, or empty for an instance of Base.
	Layer string `json:"layer" gorm:"index"`
	// Base is the driver's base the instance runs from when it has no layer
	// (machine.InstanceSpec.Base).
	Base    *machine.Base `json:"base,omitempty" gorm:"serializer:json;type:text"`
	Driver  string        `json:"driver"`
	GuestOS machine.OS    `json:"guestOS" gorm:"column:guest_os"`
	CPUs    int           `json:"cpus,omitempty" gorm:"column:cpus"`
	Memory  uint64        `json:"memory,omitempty"`
	Created time.Time     `json:"created"`
	// Temporary instances belong to a build and are not listed.
	Temporary bool `json:"temporary,omitempty"`
	// Forwards are served by the instance's shim whenever it runs.
	Forwards []Forward `json:"forwards,omitempty" gorm:"serializer:json;type:text"`
}

// TableName is the instances' table, named like the image store's.
func (Instance) TableName() string { return "disco_vm_instances" }

// legacyNameIndex made an instance's name unique across projects.
const legacyNameIndex = "idx_disco_vm_instances_name"

// legacyWarmTable held what was asked of each warm stage. An instance's mode
// column, from the same time, stays and is no longer read.
const legacyWarmTable = "disco_vm_warm_stages"

// Forward sends the connections a guest process opens to the host on a port
// (guest.DialHost: vsock to the host on vz, hvsocket on HCS) to a Unix socket
// on the host, which something there serves. It is how a guest reaches a host
// service without a TCP listener, as libkrun maps a vsock port to a Unix
// socket.
type Forward struct {
	Port   uint32 `json:"port"`
	Socket string `json:"socket"`
}

func (e *Engine) instanceDir(id string) string { return filepath.Join(e.Root, "instances", id) }

// machineSpec is the driver's view of an instance.
func (e *Engine) machineSpec(inst *Instance) (machine.InstanceSpec, error) {
	spec := machine.InstanceSpec{
		ID:      inst.ID,
		Project: inst.Project,
		Dir:     filepath.Join(e.instanceDir(inst.ID), "machine"),
		GuestOS: inst.GuestOS,
		Base:    inst.Base,
	}
	if inst.Base == nil {
		chain, err := e.Images.MachineChain(inst.Layer)
		if err != nil {
			return machine.InstanceSpec{}, err
		}
		spec.Chain = chain
	}
	return spec, nil
}

// CreateOptions describes a new instance.
type CreateOptions struct {
	Name      string
	CPUs      int
	Memory    uint64
	Temporary bool
	// Forwards are served whenever the instance runs; the driver must list
	// Capabilities.Forward.
	Forwards []Forward
}

// Create makes an instance from an image reference or layer ID, or from one of
// the driver's bases by its name or ID, which it runs from with no layer.
func (e *Engine) Create(ctx context.Context, ref string, opts CreateOptions) (*Instance, error) {
	if err := e.checkForwards(opts.Forwards); err != nil {
		return nil, err
	}
	var (
		layerID string
		base    *machine.Base
		guestOS machine.OS
	)
	layerID, err := e.Images.Resolve(ref)
	if err != nil {
		b, err := e.ResolveBase(ctx, ref, err)
		if err != nil {
			return nil, err
		}
		base, guestOS, layerID = &b, b.GuestOS, ""
	} else {
		layer, err := e.Images.Layer(layerID)
		if err != nil {
			return nil, err
		}
		if layer.Driver != e.Driver.Name() {
			return nil, fmt.Errorf("image %s was built for driver %s, not %s", ref, layer.Driver, e.Driver.Name())
		}
		guestOS = layer.GuestOS
	}
	id := fsutil.RandomHex(6)
	name := opts.Name
	if name == "" {
		name = id
	} else if _, err := e.Get(name); err == nil {
		return nil, fmt.Errorf("an instance named %q already exists", name)
	}
	inst := &Instance{
		ID: id, Project: e.Project, Name: name, Image: ref, Layer: layerID, Base: base,
		Driver: e.Driver.Name(), GuestOS: guestOS,
		CPUs: opts.CPUs, Memory: opts.Memory,
		Created: time.Now().UTC(), Temporary: opts.Temporary,
		Forwards: opts.Forwards,
	}
	if err := os.MkdirAll(e.instanceDir(id), 0o755); err != nil {
		return nil, err
	}
	spec, err := e.machineSpec(inst)
	if err == nil {
		err = e.Driver.Prepare(ctx, spec)
	}
	if err == nil {
		// The row is the instance: until it is in, nothing lists the machine.
		err = e.db.Write.WithContext(ctx).Create(inst).Error
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
	if inst, err := e.instance(ref); err == nil {
		return inst, nil
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
	query := e.db.Read.Where("project = ?", e.Project).Order("created, id")
	if !all {
		query = query.Where("temporary = ?", false)
	}
	var out []*Instance
	if err := query.Find(&out).Error; err != nil {
		return nil, err
	}
	return out, nil
}

// instance reads one instance by its full ID.
func (e *Engine) instance(id string) (*Instance, error) {
	var found []*Instance
	if err := e.db.Read.Where("project = ? AND id = ?", e.Project, id).Limit(1).Find(&found).Error; err != nil {
		return nil, err
	}
	if len(found) == 0 {
		return nil, fmt.Errorf("%q: %w", id, ErrNotFound)
	}
	return found[0], nil
}

// forget removes an instance's directory and then its row, so a removal that
// fails partway leaves an instance that can be removed again.
func (e *Engine) forget(ctx context.Context, id string) error {
	if err := os.RemoveAll(e.instanceDir(id)); err != nil {
		return err
	}
	return e.db.Write.WithContext(ctx).Delete(&Instance{ID: id}).Error
}

// remote is the driver as an Attacher when its machines are owned by a
// service rather than by a shim. Open refuses a driver that lists Remote and
// is not one.
func (e *Engine) remote() machine.Attacher {
	if !e.Driver.Capabilities().Remote {
		return nil
	}
	attacher, _ := e.Driver.(machine.Attacher)
	return attacher
}

// retireShim ends a shim that an older disco-vm started for a remote
// instance. The service owns the machine, so the shim only watches it, and
// its credentials may have expired since it started: left alone, it would
// poll forever. It is asked for its status first, with its token, so a PID
// that has since been reused is never signaled. The machine is untouched.
func (e *Engine) retireShim(ctx context.Context, inst *Instance) {
	statePath := filepath.Join(e.instanceDir(inst.ID), shimStateName)
	if shim, err := openShim(statePath); err == nil {
		ctx, cancel := context.WithTimeout(ctx, 2*time.Second)
		alive := shim.status(ctx) == nil
		cancel()
		if alive {
			killProcess(shim.state.PID)
			deadline := time.Now().Add(5 * time.Second)
			for processAlive(shim.state.PID) && time.Now().Before(deadline) {
				time.Sleep(50 * time.Millisecond)
			}
		}
	}
	_ = os.Remove(statePath)
}

// attach finds a remote instance's running machine.
func (e *Engine) attach(ctx context.Context, attacher machine.Attacher, inst *Instance) (machine.Machine, error) {
	spec, err := e.machineSpec(inst)
	if err != nil {
		return nil, err
	}
	return attacher.Attach(ctx, spec)
}

// State reports whether an instance is running: whether its shim is up, or for
// a remote driver, what its service says.
func (e *Engine) State(ctx context.Context, inst *Instance) State {
	if attacher := e.remote(); attacher != nil {
		ctx, cancel := context.WithTimeout(ctx, time.Minute)
		defer cancel()
		_, err := e.attach(ctx, attacher, inst)
		switch {
		case err == nil:
			return Running
		case errors.Is(err, machine.ErrNotRunning):
			return Stopped
		default:
			return Unknown
		}
	}
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
	// GUI has the shim open a window on the guest's display for this boot.
	GUI bool
}

// checkForwards refuses forwards the driver cannot serve or that make no sense.
func (e *Engine) checkForwards(forwards []Forward) error {
	if len(forwards) == 0 {
		return nil
	}
	if caps := e.Driver.Capabilities(); !caps.Forward || caps.Remote {
		// A remote instance has no shim to hold the forwards' listeners.
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

// Start boots an instance and waits for its agent: under a new shim, or for a
// remote driver, from this process, which lets the machine go once it answers.
func (e *Engine) Start(ctx context.Context, inst *Instance, opts StartOptions) error {
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
	if e.remote() != nil {
		e.retireShim(ctx, inst)
		return e.startRemote(ctx, inst, opts)
	}
	dir := e.instanceDir(inst.ID)
	_ = os.Remove(filepath.Join(dir, shimStateName))
	logFile, err := os.OpenFile(filepath.Join(dir, "shim.log"), os.O_WRONLY|os.O_CREATE|os.O_APPEND, 0o600)
	if err != nil {
		return err
	}
	defer logFile.Close()
	args := []string{inst.ID}
	if opts.GUI {
		args = []string{"--gui", inst.ID}
	}
	exited, err := e.spawnShim(args, logFile)
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
			return fmt.Errorf("instance %s failed to start: %w\n%s", inst.Name, err, tail(filepath.Join(dir, "shim.log")))
		case <-ctx.Done():
			return fmt.Errorf("instance %s: shim did not come up: %w", inst.Name, ctx.Err())
		case <-time.After(50 * time.Millisecond):
		}
	}
	return nil
}

// startRemote boots a remote instance, which the driver returns once its
// guest answers. Nothing waits on the machine's Done, so it is not watched,
// and nothing of it outlives this call but the machine itself.
func (e *Engine) startRemote(ctx context.Context, inst *Instance, opts StartOptions) error {
	spec, err := e.machineSpec(inst)
	if err != nil {
		return err
	}
	ctx, cancel := context.WithTimeout(ctx, opts.Timeout)
	defer cancel()
	m, err := e.Driver.Boot(ctx, spec, machine.BootOptions{CPUs: inst.CPUs, Memory: inst.Memory})
	if err != nil {
		return fmt.Errorf("instance %s failed to start: %w", inst.Name, err)
	}
	if _, err := e.announceService(ctx, inst, m); err != nil {
		_ = m.Kill(context.Background())
		return err
	}
	return nil
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
	if attacher := e.remote(); attacher != nil {
		e.retireShim(ctx, inst)
		m, err := e.attach(ctx, attacher, inst)
		if errors.Is(err, machine.ErrNotRunning) {
			return nil
		}
		if err != nil {
			return err
		}
		booted := &Booted{Machine: m}
		return booted.Shutdown(ctx, timeout)
	}
	shim, err := e.shim(inst.ID)
	if err != nil {
		return nil //nolint:nilerr // no shim answering means the instance is not running, which is what Stop wants
	}
	return shim.stop(ctx, timeout)
}

// Remove deletes an instance. A running one is refused unless force, which
// stops it first.
func (e *Engine) Remove(ctx context.Context, inst *Instance, force bool) error {
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
	if e.remote() != nil {
		e.retireShim(ctx, inst)
	}
	if spec, err := e.machineSpec(inst); err == nil {
		if err := e.Driver.Destroy(ctx, spec); err != nil {
			return err
		}
	}
	return e.forget(ctx, inst.ID)
}

// Guest is a running instance's guest operations: the machine itself, for a
// remote driver, attached with this process's credentials; otherwise its
// shim's, relayed to the machine the shim holds.
func (e *Engine) Guest(ctx context.Context, inst *Instance) (machine.Guest, error) {
	if attacher := e.remote(); attacher != nil {
		m, err := e.attach(ctx, attacher, inst)
		if err != nil {
			return nil, fmt.Errorf("instance %s: %w", inst.Name, err)
		}
		return m, nil
	}
	if _, err := e.shim(inst.ID); err != nil {
		return nil, fmt.Errorf("instance %s is not running", inst.Name)
	}
	return guest.NewClient(func(ctx context.Context) (net.Conn, error) {
		shim, err := e.shim(inst.ID)
		if err != nil {
			return nil, fmt.Errorf("instance %s is not running", inst.Name)
		}
		return shim.dialGuest(ctx)
	}), nil
}

// URL is where an instance's one service is reached over HTTP. It is the
// driver's where the driver has one (machine.Service.URL): boxd's public
// HTTPS URL, the same across stop and start, or a port the Docker daemon
// publishes while the container runs. Otherwise it is http://127.0.0.1:PORT,
// which the instance's shim serves for as long as it runs by splicing each
// connection into the service's guest port; the port changes at every start.
// Neither needs a credential, so the service authenticates its callers.
func (e *Engine) URL(ctx context.Context, inst *Instance) (string, error) {
	service, err := e.service(ctx, inst)
	if err != nil {
		return "", err
	}
	if service.URL != "" {
		return service.URL, nil
	}
	if e.remote() != nil {
		return "", fmt.Errorf("instance %s: the %s driver gave its service no URL", inst.Name, e.Driver.Name())
	}
	shim, err := e.shim(inst.ID)
	if err != nil || shim.state.Service == "" {
		return "", fmt.Errorf("instance %s is not running", inst.Name)
	}
	return "http://" + shim.state.Service, nil
}

// service is the driver's view of an instance's service.
func (e *Engine) service(ctx context.Context, inst *Instance) (machine.Service, error) {
	spec, err := e.machineSpec(inst)
	if err != nil {
		return machine.Service{}, err
	}
	service, err := e.Driver.Service(ctx, spec)
	if errors.Is(err, machine.ErrNotRunning) {
		return machine.Service{}, fmt.Errorf("instance %s is not running: %w", inst.Name, err)
	}
	return service, err
}

// announceService tells a started guest where its service listens, in its
// guest.ServiceFile, before the start returns.
func (e *Engine) announceService(ctx context.Context, inst *Instance, g machine.Guest) (machine.Service, error) {
	service, err := e.service(ctx, inst)
	if err != nil {
		return machine.Service{}, err
	}
	if err := guest.SetService(ctx, g, string(inst.GuestOS), service.Listen); err != nil {
		return machine.Service{}, fmt.Errorf("instance %s: %w", inst.Name, err)
	}
	return service, nil
}

// Booted is a machine running in this process. The builder boots this way;
// the shim is the same thing in its own process.
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
func (e *Engine) Boot(ctx context.Context, inst *Instance, opts BootOptions) (*Booted, error) {
	spec, err := e.machineSpec(inst)
	if err != nil {
		return nil, err
	}
	if err := e.CheckGUI(opts.GUI); err != nil {
		return nil, err
	}
	title := opts.Title
	if title == "" {
		title = "disco-vm: " + inst.Name
	}
	m, err := e.Driver.Boot(ctx, spec, machine.BootOptions{
		CPUs: inst.CPUs, Memory: inst.Memory, Console: opts.Console,
		GUI: opts.GUI, Title: title,
	})
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

// Commit captures a stopped instance as a new layer and removes the instance.
func (e *Engine) Commit(ctx context.Context, inst *Instance, pending *image.Pending) (image.Layer, error) {
	spec, err := e.machineSpec(inst)
	if err != nil {
		return image.Layer{}, err
	}
	if err := e.Driver.Commit(ctx, spec, pending.MachineLayer()); err != nil {
		return image.Layer{}, err
	}
	layer, err := pending.Commit(ctx, e.Driver)
	if err != nil {
		return image.Layer{}, err
	}
	_ = e.Driver.Destroy(ctx, spec)
	_ = e.forget(ctx, inst.ID)
	return layer, nil
}

// Snapshot commits a stopped instance's disk as a new layer on its image's
// layer, removes the instance, and tags the layer ref when ref is not empty.
// The instance is consumed, as a build's is, so there is one way a layer is
// cut; to go on from a snapshot, create an instance of it. The layer's ID is
// random: nothing reproducible made it, so it is never a cache hit.
func (e *Engine) Snapshot(ctx context.Context, inst *Instance, ref string) (image.Layer, error) {
	if inst.Temporary {
		return image.Layer{}, fmt.Errorf("instance %s belongs to a build", inst.Name)
	}
	if ref != "" {
		if err := image.ValidateRef(ref); err != nil {
			return image.Layer{}, err
		}
	}
	if state := e.State(ctx, inst); state != Stopped {
		return image.Layer{}, fmt.Errorf("instance %s is %s; stop it first", inst.Name, state)
	}
	if e.remote() != nil {
		e.retireShim(ctx, inst)
	}
	pending, err := e.Images.Begin(image.Layer{
		ID: fsutil.RandomHex(32), Parent: inst.Layer, Driver: e.Driver.Name(), GuestOS: inst.GuestOS,
		Comment: "snapshot " + inst.Name,
	})
	if err != nil {
		return image.Layer{}, err
	}
	layer, err := e.Commit(ctx, inst, pending)
	if err != nil {
		pending.Abort(context.Background(), e.Driver)
		return image.Layer{}, fmt.Errorf("snapshot %s: %w", inst.Name, err)
	}
	if ref != "" {
		if err := e.Images.Tag(ctx, ref, layer.ID, e.Driver); err != nil {
			return layer, fmt.Errorf("snapshot %s is layer %s, but tag %s: %w", inst.Name, image.Short(layer.ID), ref, err)
		}
	}
	return layer, nil
}

// Discard removes an instance without asking its state; the builder uses it
// on temporary instances it knows are stopped.
func (e *Engine) Discard(ctx context.Context, inst *Instance) {
	if spec, err := e.machineSpec(inst); err == nil {
		_ = e.Driver.Destroy(ctx, spec)
	}
	_ = e.forget(ctx, inst.ID)
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
