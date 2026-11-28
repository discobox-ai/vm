// Package archil is a driver for Archil (https://archil.com): Firecracker
// microVM sandboxes in the cloud, behind Archil's REST API. It runs on every
// host OS, because the hypervisor is not on the host (ADR 26-11-03-676).
//
// A VM is the sandbox named for it, dvm-<pool>--<name> (names.go), which
// Archil keeps unique (ADR 26-11-03-606). A fresh VM is a sandbox of the
// driver's base, an OCI image (machine.Config.Base), or of Archil's default
// image; a VM created from another is a fork of that one's sandbox. A fork
// pins the sandbox it came from, so copies chain below the seam: Destroy
// refuses a VM with forks, with machine.ErrInUse. A committed VM is a stopped
// sandbox with no mounts, which Archil does not tell from any other: that it
// is committed is the caller's to record.
//
// Archil's init is the sandbox's PID 1 and runs nothing of the image's, so
// the driver has Archil's supervisor in the guest run systemd, as PID 1 of
// namespaces of its own, at every boot: installed with apt at Create where
// the base has none (systemd.go, ADR 26-11-20-447). The guest's operations
// are Archil's process API, entering systemd's namespaces (guest.go).
package archil

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"slices"
	"strings"
	"time"

	"github.com/discobox-ai/vm/pkg/machine"
)

func init() {
	machine.Register("archil", machine.Registration{
		New:        func(cfg machine.Config) (machine.Driver, error) { return New(cfg) },
		Definition: Definition(),
		Validate:   Validate,
	})
}

// waitLimit bounds each wait for a sandbox to change state.
const waitLimit = 10 * time.Minute

// Driver is the archil driver.
type Driver struct {
	api  *api
	pool string
	// base is the OCI image a fresh VM runs; empty is Archil's default.
	base string
	// dataDisks gives each new VM a data disk (Config.DataDisk).
	dataDisks bool
}

var _ machine.Driver = (*Driver)(nil)

// New returns a driver for cfg's pool (machine.DefaultPool when empty)
// whose fresh VMs run cfg.Base, in the region its configuration
// (cfg.DriverConfig) names, and that calls Archil with the key cfg.Keys
// answers on each call. It calls nothing until it is used.
func New(cfg machine.Config) (*Driver, error) {
	config, err := machine.Normalize("archil", cfg.DriverConfig)
	if err != nil {
		return nil, err
	}
	conf, err := decode(config)
	if err != nil {
		return nil, err
	}
	pool := cfg.Pool
	if pool == "" {
		pool = machine.DefaultPool
	}
	return &Driver{api: newAPI(conf, cfg.Keys, cfg.HTTPTransport()), pool: pool, base: cfg.Base, dataDisks: conf.DataDisk}, nil
}

func (*Driver) Name() string { return "archil" }

func (d *Driver) Capabilities() machine.Capabilities {
	// A new VM has a data disk only where the driver is configured for
	// one; else its data is on its root (ADR 26-11-28-860 §1). A VM that
	// has one keeps it either way.
	var volumes []machine.Volume
	if d.dataDisks {
		volumes = []machine.Volume{machine.VolumeData}
	}
	return machine.Capabilities{
		GuestOS: []machine.OS{machine.Linux},
		Keys:    machine.KeyCapabilities{Mode: machine.KeysNone},
		Egress:  []machine.EgressFence{machine.EgressNone, machine.EgressAllow, machine.EgressCloseDNS},
		Volumes: volumes,
		// A committed VM is its stopped sandbox, and a copy a fork of it.
		Commit: true,
		// A pause is Archil's caller pause (POST .../pause), which keeps
		// the sandbox's memory and processes, and Start's resume restores
		// them (ADR 26-11-21-438 §7).
		Pause: true,
		// A sandbox holds nothing of the account: the driver puts nothing
		// in it, and Archil gives it no credential. No data disk's mount
		// token is visible in the namespaces the guest runs in: Archil's
		// client holds it outside them (seen on client version 1006,
		// 2026-11; see DESIGN.md).
		Bare: machine.BareAlways,
		// Archil reaches a sandbox only at its port's hostname, with a
		// port token Service mints: its ingress URL, unless asked off, and
		// its pool URL either way (ADR 26-11-17-311 §3).
		URLs: machine.URLCapabilities{Ingress: machine.URLOptional, Pool: machine.URLAlways, Credential: true},
	}
}

// Check lists the pool's sandboxes, which needs a region and a key.
func (d *Driver) Check(ctx context.Context) error {
	_, err := d.api.sandboxes(ctx, d.filter(""))
	return err
}

// Install is not offered: a fresh VM runs the driver's base instead.
func (*Driver) Install(context.Context, machine.InstallSpec, string) (machine.Instance, error) {
	return machine.Instance{}, fmt.Errorf("archil: a fresh VM runs an OCI image instead: %w", machine.ErrUnsupported)
}

// waitFor polls a sandbox until it is in one of states. A failed sandbox, or
// one gone, is an error.
func (a *api) waitFor(ctx context.Context, id string, states ...string) (sandbox, error) {
	ctx, cancel := context.WithTimeout(ctx, waitLimit)
	defer cancel()
	for {
		s, err := a.sandbox(ctx, id)
		if err != nil {
			return s, err
		}
		if slices.Contains(states, s.Status) {
			return s, nil
		}
		if s.Status == "failed" || s.Status == "deleted" {
			return s, fmt.Errorf("archil: sandbox %s is %s: %s", s.Name, s.Status, s.ExitReason)
		}
		select {
		case <-ctx.Done():
			return s, fmt.Errorf("archil: sandbox %s is still %s, not %s: %w", s.Name, s.Status, strings.Join(states, " or "), ctx.Err())
		case <-time.After(time.Second):
		}
	}
}

// stopAndWait stops a sandbox that runs and waits until it has. A running
// one has systemd killed and its writes flushed first (haltScript), the
// data volume's where it is mounted, so Archil's stop waits for nothing in
// the guest; a guest that does not answer, or whose flush fails, is
// stopped all the same.
func (d *Driver) stopAndWait(ctx context.Context, id string) error {
	s, err := d.api.sandbox(ctx, id)
	if err != nil {
		return err
	}
	switch s.Status {
	case "stopped", "exited", "failed":
		return nil
	case "pausing":
		// Archil stops a paused sandbox, not one still pausing.
		if _, err := d.api.waitFor(ctx, id, "paused"); err != nil {
			return err
		}
		fallthrough
	case "paused", "running", "pending":
		if s.Status == "running" {
			d.api.haltQuietly(ctx, id, s.Name, machine.VolumePath(machine.Linux, machine.VolumeData))
		}
		if err := d.api.stop(ctx, id); err != nil && !isConflict(err) {
			return fmt.Errorf("archil: stop %s: %w", s.Name, err)
		}
	}
	_, err = d.api.waitFor(ctx, id, "stopped", "exited")
	return err
}

func stopped(s sandbox) bool { return s.Status == "stopped" || s.Status == "exited" }

// Create makes the VM's sandbox, of the base or forked from spec.From,
// labeled as the VM, with its fence in place and no port exposed, and its
// data volume, where the driver is configured for one (ADR 26-11-28-860
// §1), and stops it, or, with spec.Start, leaves it running, systemd
// launched, as Start does (ADR 26-11-23-458 §2). Archil runs a sandbox as
// soon as it is made, so a fresh one asked to start is started once: its
// data disk is made first and mounted by its create. A fork takes no
// mounts, so one asked to start with a data volume is stopped and started
// once with it mounted (ADR 26-11-24-905 §6); one with none is the fork
// alone, and boots once (ADR 26-11-28-860 §4).
func (d *Driver) Create(ctx context.Context, spec machine.InstanceSpec) (machine.Instance, error) {
	if err := machine.ValidateName(spec.Name); err != nil {
		return machine.Instance{}, err
	}
	if spec.Start != nil && spec.Start.GUI {
		return machine.Instance{}, fmt.Errorf("archil: a window on the guest's display: %w", machine.ErrUnsupported)
	}
	if err := d.checkName(spec.Name); err != nil {
		return machine.Instance{}, err
	}
	switch {
	case len(spec.Forwards) > 0:
		return machine.Instance{}, fmt.Errorf("archil: forwards: %w", machine.ErrUnsupported)
	case spec.From != nil && machine.OtherPool(spec.From.Pool, d.pool):
		return machine.Instance{}, fmt.Errorf("archil: a VM from another pool's (%s): %w", spec.From.Pool, machine.ErrUnsupported)
	case len(spec.Networks) > 0:
		return machine.Instance{}, fmt.Errorf("archil: networks: %w", machine.ErrUnsupported)
	case !d.Capabilities().Fences(spec.Egress):
		return machine.Instance{}, fmt.Errorf("archil: a fence of %+v: %w", *spec.Egress, machine.ErrUnsupported)
	case spec.Cache != nil:
		return machine.Instance{}, fmt.Errorf("archil: a cache volume: %w", machine.ErrUnsupported)
	case spec.Data != nil && !d.Capabilities().Attaches(machine.VolumeData):
		return machine.Instance{}, fmt.Errorf("archil: a data volume, where the driver keeps a new VM's data on its root: %w", machine.ErrUnsupported)
	}
	if err := spec.URLs.Check(d.Capabilities().URLs); err != nil {
		return machine.Instance{}, fmt.Errorf("archil: %w", err)
	}
	if err := checkAllow(spec.Egress); err != nil {
		return machine.Instance{}, err
	}
	if _, err := d.sandboxOf(ctx, machine.Instance{Name: spec.Name}); err == nil {
		return machine.Instance{}, fmt.Errorf("archil: a VM named %q: %w", spec.Name, machine.ErrExists)
	} else if !isNotFound(err) {
		return machine.Instance{}, err
	}
	made := &made{d: d}
	inst, err := d.create(ctx, spec, made)
	if err != nil {
		made.undo()
		return machine.Instance{}, err
	}
	return inst, nil
}

// made is what a Create or an Upgrade has made so far, removed if it fails.
type made struct {
	d       *Driver
	sandbox string
	disk    string
}

func (m *made) undo() {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()
	if m.sandbox != "" {
		_ = m.d.stopAndWait(ctx, m.sandbox)
		// A fork may carry its source's tokens.
		_ = m.d.revokeTokens(ctx, m.sandbox)
		_ = m.d.api.deleteSandbox(ctx, m.sandbox)
	}
	if m.disk != "" {
		_ = m.d.api.deleteDisk(ctx, m.disk)
	}
}

// taken reads Archil's refusal of a sandbox name it has as machine.ErrExists.
func taken(name string, err error) error {
	if _, code := statusOf(err); code == "sandbox_name_conflict" || isConflict(err) {
		return fmt.Errorf("archil: a VM named %q: %w", name, machine.ErrExists)
	}
	return err
}

func (d *Driver) create(ctx context.Context, spec machine.InstanceSpec, made *made) (machine.Instance, error) {
	var src *sandbox
	if spec.From != nil {
		from, err := d.source(ctx, *spec.From, spec.CPUs, spec.Memory)
		if err != nil {
			return machine.Instance{}, err
		}
		src = &from
	}
	// A fresh VM asked to start mounts its data disk in its sandbox's
	// create, so the disk is made before the sandbox takes the name: only
	// one Archil made for it (201) is, since a disk of the name already
	// there may be another create's, racing this one, as well as a removed
	// VM's, and only a create that holds the name may delete it. Otherwise,
	// and for a fork, which takes no mounts, the sandbox is made first and
	// started once more with the disk mounted.
	diskFirst := false
	var mounts []mount
	if spec.Start != nil && spec.Data != nil && src == nil {
		id, fresh, err := d.api.createDisk(ctx, d.dataDiskName(spec.Name))
		if err != nil {
			return machine.Instance{}, fmt.Errorf("archil: create the data volume: %w", err)
		}
		if fresh {
			diskFirst = true
			mounts = []mount{{DiskID: id, Path: machine.VolumePath(machine.Linux, machine.VolumeData)}}
			made.disk = id
		}
	}
	restart := spec.Start != nil && spec.Data != nil && !diskFirst
	sb, fence, err := d.makeSandbox(ctx, spec, d.sandboxName(spec.Name), d.labels(spec), src, mounts, spec.Start != nil && !restart, made)
	if err != nil {
		// A disk this create made is never another's: a create mounts only
		// one Archil made for it, and replaces any other once it holds the
		// name. So it goes with this create's sandbox, by its ID.
		return machine.Instance{}, err
	}
	switch {
	case spec.Data != nil && !diskFirst:
		id, err := d.makeDataDisk(ctx, spec.Name)
		if err != nil {
			return machine.Instance{}, err
		}
		made.disk = id
	case spec.Data == nil:
		// A VM has a data disk if one of its name is there, so a disk left
		// from a removed VM of the name would be this one's, mounted at
		// its next start, now that the sandbox holds the name and no other
		// create can be making one for it (ADR 26-11-28-860 §2).
		if err := d.removeDataDisk(ctx, spec.Name); err != nil {
			return machine.Instance{}, err
		}
	}
	inst := d.instance(spec.Name, sb, spec.Data != nil)
	inst.Egress = fence
	if spec.From != nil {
		inst.From = spec.From.Name
	}
	if restart {
		if _, err := d.Start(ctx, inst, *spec.Start); err != nil {
			return machine.Instance{}, err
		}
	}
	return inst, nil
}

// makeDataDisk makes a VM's data disk, once the create's sandbox is the VM
// and no other is: one Archil answers is there already (200, not 201) is
// left from a VM removed midway, so it is deleted and made again, and no VM
// sees another's data (ADR 26-11-24-905 §5).
func (d *Driver) makeDataDisk(ctx context.Context, name string) (string, error) {
	id, made, err := d.api.createDisk(ctx, d.dataDiskName(name))
	if err == nil && !made {
		if err = d.api.deleteDisk(ctx, id); err == nil {
			id, made, err = d.api.createDisk(ctx, d.dataDiskName(name))
		}
		if err == nil && !made {
			err = fmt.Errorf("archil: %s was there again once deleted", d.dataDiskName(name))
		}
	}
	if err != nil {
		return "", fmt.Errorf("archil: create the data volume: %w", err)
	}
	return id, nil
}

// source is the sandbox of the committed VM a VM is made from, refused
// unless a fork of it is one: stopped, with no mounts, and of the size asked
// for (zero is any), since a fork keeps its source's.
func (d *Driver) source(ctx context.Context, from machine.Instance, cpus int, memory uint64) (sandbox, error) {
	if from.GuestOS != "" && from.GuestOS != machine.Linux {
		return sandbox{}, fmt.Errorf("archil: VM %s is %s, and archil's are linux: %w", from.Name, from.GuestOS, machine.ErrUnsupported)
	}
	sb, err := d.sandboxOf(ctx, from)
	switch {
	case isNotFound(err):
		return sb, fmt.Errorf("archil: VM %s: %w", from.Name, machine.ErrNotFound)
	case err != nil:
		return sb, err
	case !stopped(sb):
		return sb, fmt.Errorf("archil: a VM is made from a stopped one; %s is %s", from.Name, sb.Status)
	case len(sb.Mounts) > 0:
		// A fork boots with its source's mounts: it would share the
		// source's data volume. A commit drops them.
		return sb, fmt.Errorf("archil: %s has a data volume mounted: commit it first", from.Name)
	case (cpus != 0 && cpus != sb.VCPUs) || (memory != 0 && memory>>20 != sb.MemoryMiB):
		return sb, fmt.Errorf("archil: a VM made from %s has its size, %d vCPUs and %d MiB: %w", from.Name, sb.VCPUs, sb.MemoryMiB, machine.ErrUnsupported)
	}
	return sb, nil
}

// makeSandbox makes the sandbox name, labeled labels, fresh from the base
// with mounts or a fork of src; and, once it is the VM its labels name and
// no older one is (ADR 26-11-24-905 §3; an upgrade's new sandbox is
// never the VM while the sandbox it replaces lives, so it is not asked),
// sets the spec's size, fence, and ingress (setNetwork), which it reads
// back, and service ingress off; exposes no port, and revokes any token a
// fork carried; has systemd run at its every boot (ensureSystemd), under its
// fence, and waits until it is launched, not until it settles; and leaves
// it stopped, or running where running asks. It returns
// the sandbox and the fence Archil holds.
func (d *Driver) makeSandbox(ctx context.Context, spec machine.InstanceSpec, name string, labels map[string]string, src *sandbox, mounts []mount, running bool, made *made) (sandbox, *machine.Egress, error) {
	var sb sandbox
	var err error
	if src == nil {
		sb, err = d.api.createSandbox(ctx, createSandbox{
			Name: name, BaseImage: d.base, VCPUs: spec.CPUs, MemoryMiB: spec.Memory >> 20,
			Network: policy(spec.Egress), Labels: labels, Mounts: mounts,
		})
		if err != nil {
			return sb, nil, taken(spec.Name, fmt.Errorf("archil: create a sandbox of %s: %w", d.baseName(), err))
		}
	} else {
		sb, err = d.api.fork(ctx, src.ID, name, labels)
		if err != nil {
			return sb, nil, taken(spec.Name, fmt.Errorf("archil: fork %s: %w", spec.From.Name, err))
		}
	}
	made.sandbox = sb.ID
	if labels[labelReplaces] == "" {
		// Two creates of a name both passed their checks: the older
		// sandbox is the VM, and the other's create gives way.
		all, err := d.api.sandboxes(ctx, d.filter(spec.Name))
		if err != nil {
			return sb, nil, err
		}
		if vm, ok := d.vms(all)[spec.Name]; ok && vm.ID != sb.ID {
			return sb, nil, fmt.Errorf("archil: a VM named %q: %w", spec.Name, machine.ErrExists)
		}
	}
	if sb, err = d.api.waitFor(ctx, sb.ID, "running"); err != nil {
		return sb, nil, err
	}
	got, err := d.setNetwork(ctx, sb.ID, spec.Egress, spec.URLs.Ingress.Mode == machine.URLOff)
	if err != nil {
		return sb, nil, fmt.Errorf("archil: fence %s: %w", spec.Name, err)
	}
	if src != nil {
		// Archil was seen to give a fork none of its source's tokens
		// (2026-11-18), and documents nothing of it: it answers neither.
		if err := d.revokeTokens(ctx, sb.ID); err != nil {
			return sb, nil, fmt.Errorf("archil: %s: %w", spec.Name, err)
		}
	}
	// Under the fence: a base without systemd that can't reach its
	// mirror fails here. A fork carries its source's root and service.
	m := newMachine(d.api, sb.ID, spec.Name)
	if err := m.waitReady(ctx); err != nil {
		return sb, nil, err
	}
	if err := d.api.ensureSystemd(ctx, sb.ID, spec.Name, spec.Egress != nil); err != nil {
		return sb, nil, err
	}
	if err := m.waitLaunched(ctx); err != nil {
		return sb, nil, err
	}
	if !running {
		if err := d.stopAndWait(ctx, sb.ID); err != nil {
			return sb, nil, err
		}
	}
	if sb, err = d.api.sandbox(ctx, sb.ID); err != nil {
		return sb, nil, err
	}
	return sb, fenceOf(got), nil
}

// Upgrade gives the stopped VM a new root (ADR 26-11-03-782, ADR
// 26-11-24-905 §4): a new sandbox, fresh from the base or a fork of from,
// labeled for the VM and as replacing its sandbox, at its size, fence, and
// ingress, with systemd launched and stopped; then the old sandbox's port
// tokens are revoked and it is deleted. Until then the old sandbox is the
// VM, so a failure removes the new one and leaves the VM as it was, and an
// upgrade stopped in between leaves a new sandbox nothing lists, which the
// next Upgrade or Destroy removes. The data disk is mounted at each start
// as before. A VM with no data disk is refused, ErrUnsupported, however the
// driver is configured: its data is on the root an upgrade deletes (ADR
// 26-11-28-860 §3).
func (d *Driver) Upgrade(ctx context.Context, inst machine.Instance, from *machine.Instance) (machine.Instance, error) {
	all, err := d.api.sandboxes(ctx, d.filter(inst.Name))
	if err != nil {
		return machine.Instance{}, err
	}
	old, ok := d.vms(all)[inst.Name]
	switch {
	case !ok:
		return machine.Instance{}, fmt.Errorf("archil: VM %s: %w", inst.Name, machine.ErrNotFound)
	case !stopped(old) && old.Status != "failed":
		// failed is a guest that powered itself off, stopped: the engine
		// reports it stopped, and Archil deletes it.
		return machine.Instance{}, fmt.Errorf("archil: an upgrade needs a stopped VM; %s is %s", inst.Name, old.Status)
	}
	data, err := d.dataDisk(ctx, inst.Name)
	if err != nil {
		return machine.Instance{}, err
	}
	if data == "" {
		return machine.Instance{}, fmt.Errorf("archil: %s keeps its data on its root, which a new root would delete; rebuild it in place instead: %w", inst.Name, machine.ErrUnsupported)
	}
	got, err := d.api.network(ctx, old.ID)
	if err != nil {
		return machine.Instance{}, err
	}
	spec := machine.InstanceSpec{
		Name: inst.Name, From: from, CPUs: old.VCPUs, Memory: old.MemoryMiB << 20, Egress: fenceOf(got),
	}
	if !ingressOf(got) {
		spec.URLs.Ingress = machine.URLRequest{Mode: machine.URLOff}
	}
	var src *sandbox
	if from != nil {
		if machine.OtherPool(from.Pool, d.pool) {
			return machine.Instance{}, fmt.Errorf("archil: a root from another pool's VM (%s): %w", from.Pool, machine.ErrUnsupported)
		}
		if from.Name == inst.Name {
			return machine.Instance{}, fmt.Errorf("archil: %s can't be upgraded from itself", inst.Name)
		}
		sb, err := d.source(ctx, *from, spec.CPUs, spec.Memory)
		if err != nil {
			return machine.Instance{}, err
		}
		src = &sb
	}
	// What an upgrade or a create of the name left behind goes first.
	for _, sb := range all {
		if live(sb) && sb.ID != old.ID && sb.Labels[labelPool] == d.pool {
			if err := d.removeSandbox(ctx, sb); err != nil {
				return machine.Instance{}, fmt.Errorf("archil: %s: %w", inst.Name, err)
			}
		}
	}
	labels := d.labels(spec)
	labels[labelReplaces] = old.ID
	root := &made{d: d}
	sb, fence, err := d.makeSandbox(ctx, spec, d.upgradeName(inst.Name), labels, src, nil, false, root)
	if errors.Is(err, machine.ErrExists) {
		// The random end was another's: once more, with another.
		root.undo()
		root = &made{d: d}
		sb, fence, err = d.makeSandbox(ctx, spec, d.upgradeName(inst.Name), labels, src, nil, false, root)
	}
	if err != nil {
		root.undo()
		return machine.Instance{}, fmt.Errorf("archil: upgrade %s, which is left as it was: %w", inst.Name, err)
	}
	if err := d.removeSandbox(ctx, old); err != nil {
		// A delete Archil took before the call failed (a dropped
		// connection, a cancel) leaves the new root the VM, so the new
		// one is removed only while the old one is still seen to live. One
		// that can't be read is left: while the old lives the new is no
		// VM, and the next upgrade or destroy removes it.
		readCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), time.Minute)
		now, readErr := d.api.sandbox(readCtx, old.ID)
		cancel()
		switch {
		case readErr == nil && live(now):
			root.undo()
			return machine.Instance{}, fmt.Errorf("archil: upgrade %s, which is left as it was: %w", inst.Name, err)
		case readErr != nil && !isNotFound(readErr):
			return machine.Instance{}, fmt.Errorf("archil: upgrade %s: its old root may or may not be deleted, and the new one is kept; upgrade it again: %w", inst.Name, errors.Join(err, readErr))
		}
	}
	upgraded := d.instance(inst.Name, sb, true)
	upgraded.Egress = fence
	if from != nil {
		upgraded.From = from.Name
	}
	return upgraded, nil
}

func (d *Driver) baseName() string {
	if d.base == "" {
		return "Archil's default image"
	}
	return d.base
}

// instance is a VM as its sandbox reports it. Archil does not say which
// sandbox a fork came from: From is its label's, empty on a sandbox made
// before the driver labeled them.
func (d *Driver) instance(name string, sb sandbox, data bool) machine.Instance {
	inst := machine.Instance{
		ID: sb.ID, Name: name, Pool: d.pool, From: sb.Labels[labelFrom], GuestOS: machine.Linux,
		CPUs: sb.VCPUs, Memory: sb.MemoryMiB << 20, Created: sb.Created, Bare: true,
	}
	if data {
		inst.Data = &machine.DataVolume{}
	}
	return inst
}

// instances reads the pool's VMs, a name's alone when name is not empty,
// with their fences and data volumes: the sandbox that is each VM (vms), and
// the data disk of its name.
func (d *Driver) instances(ctx context.Context, name string) ([]machine.Instance, error) {
	all, err := d.api.sandboxes(ctx, d.filter(name))
	if err != nil {
		return nil, fmt.Errorf("archil: list sandboxes: %w", err)
	}
	vms := d.vms(all)
	if len(vms) == 0 {
		return nil, nil
	}
	query := ""
	if name != "" {
		query = d.dataDiskName(name)
	}
	disks, err := d.api.disks(ctx, query)
	if err != nil {
		return nil, fmt.Errorf("archil: list disks: %w", err)
	}
	data := map[string]bool{}
	for _, dk := range disks {
		if n := d.nameOfDisk(dk.Name); n != "" {
			data[n] = true
		}
	}
	var out []machine.Instance
	for n, sb := range vms {
		inst := d.instance(n, sb, data[n])
		if p, err := d.api.network(ctx, sb.ID); err == nil {
			inst.Egress = fenceOf(p)
		} else if !isNotFound(err) {
			return nil, err
		}
		out = append(out, inst)
	}
	slices.SortFunc(out, func(a, b machine.Instance) int {
		if c := a.Created.Compare(b.Created); c != 0 {
			return c
		}
		return strings.Compare(a.Name, b.Name)
	})
	return out, nil
}

// live is a sandbox not being deleted.
func live(sb sandbox) bool { return sb.Status != "deleted" && sb.Status != "deleting" }

func (d *Driver) Instance(ctx context.Context, name string) (machine.Instance, error) {
	if machine.ValidateName(name) != nil {
		return machine.Instance{}, fmt.Errorf("VM %q: %w", name, machine.ErrNotFound)
	}
	all, err := d.instances(ctx, name)
	if err != nil {
		return machine.Instance{}, err
	}
	if len(all) == 0 {
		return machine.Instance{}, fmt.Errorf("VM %q: %w", name, machine.ErrNotFound)
	}
	return all[0], nil
}

func (d *Driver) Instances(ctx context.Context) ([]machine.Instance, error) {
	return d.instances(ctx, "")
}

// sandboxOf is the sandbox that is the VM now (vms), so an Instance from
// before an upgrade still names it, or not found.
func (d *Driver) sandboxOf(ctx context.Context, inst machine.Instance) (sandbox, error) {
	all, err := d.api.sandboxes(ctx, d.filter(inst.Name))
	if err != nil {
		return sandbox{}, err
	}
	if sb, ok := d.vms(all)[inst.Name]; ok {
		return sb, nil
	}
	return sandbox{}, &apiError{Status: http.StatusNotFound, Message: fmt.Sprintf("VM %s is gone", inst.Name)}
}

// Destroy stops each sandbox labeled for the VM, revokes its port tokens,
// and deletes it, then the data volume: the VM's own sandbox last, after
// any an upgrade or a create of the name left (ADR 26-11-24-905 §§3-4).
// Archil refuses a sandbox with forks, which is machine.ErrInUse: a VM
// created from it still needs it, and it keeps its data volume. A VM with no
// sandbox left still has its data volume removed.
func (d *Driver) Destroy(ctx context.Context, inst machine.Instance) error {
	all, err := d.api.sandboxes(ctx, d.filter(inst.Name))
	if err != nil {
		return err
	}
	vm, isVM := d.vms(all)[inst.Name]
	var order []sandbox
	for _, sb := range all {
		if live(sb) && sb.Labels[labelPool] == d.pool && (!isVM || sb.ID != vm.ID) {
			order = append(order, sb)
		}
	}
	if isVM {
		order = append(order, vm)
	}
	for _, sb := range order {
		if err := d.removeSandbox(ctx, sb); err != nil {
			return fmt.Errorf("archil: %s: %w", inst.Name, err)
		}
	}
	return d.removeDataDisk(ctx, inst.Name)
}

// removeSandbox stops a sandbox, revokes its port tokens, and deletes it.
// Archil was seen to refuse a deleted sandbox's tokens (2026-11-18), and
// documents nothing of it, so they are revoked first.
func (d *Driver) removeSandbox(ctx context.Context, sb sandbox) error {
	if err := d.stopAndWait(ctx, sb.ID); err != nil {
		return err
	}
	if err := d.revokeTokens(ctx, sb.ID); err != nil {
		return err
	}
	if err := d.api.deleteSandbox(ctx, sb.ID); err != nil {
		if _, code := statusOf(err); code == "sandbox_has_forks" {
			return fmt.Errorf("%s has forks: %w", sb.Name, machine.ErrInUse)
		}
		return fmt.Errorf("delete %s: %w", sb.Name, err)
	}
	return nil
}

// dataDisk is the ID of a VM's data volume, or "" for none.
func (d *Driver) dataDisk(ctx context.Context, name string) (string, error) {
	disks, err := d.api.disks(ctx, d.dataDiskName(name))
	if err != nil {
		return "", fmt.Errorf("archil: list disks: %w", err)
	}
	for _, dk := range disks {
		if dk.Name == d.dataDiskName(name) {
			return dk.ID, nil
		}
	}
	return "", nil
}

// removeDataDisk deletes a VM's data volume, if it has one.
func (d *Driver) removeDataDisk(ctx context.Context, name string) error {
	id, err := d.dataDisk(ctx, name)
	if err != nil || id == "" {
		return err
	}
	if err := d.api.deleteDisk(ctx, id); err != nil {
		return fmt.Errorf("archil: delete the data volume %s: %w", d.dataDiskName(name), err)
	}
	return nil
}

// Start starts the sandbox with its data volume mounted, or resumes one
// Archil paused, and waits until systemd, which
// Archil's supervisor starts, runs in it (waitLaunched), not for its boot
// to finish. Archil runs the
// sandbox, so Boot is Start. A committed VM is not refused: Archil can't
// tell it from any stopped one, and the caller never starts one.
func (d *Driver) Start(ctx context.Context, inst machine.Instance, opts machine.BootOptions) (machine.Machine, error) {
	if opts.GUI {
		return nil, fmt.Errorf("archil: a window on the guest's display: %w", machine.ErrUnsupported)
	}
	sb, err := d.sandboxOf(ctx, inst)
	if err != nil {
		return nil, err
	}
	if sb.Status == "stopping" || sb.Status == "pausing" {
		if sb, err = d.api.waitFor(ctx, sb.ID, "stopped", "exited", "paused"); err != nil {
			return nil, err
		}
	}
	switch sb.Status {
	case "paused":
		if err := d.api.resume(ctx, sb.ID); err != nil {
			return nil, fmt.Errorf("archil: resume %s: %w", inst.Name, err)
		}
	case "running", "pending":
	default:
		mounts := []mount{}
		id, err := d.dataDisk(ctx, inst.Name)
		if err != nil {
			return nil, err
		}
		if id != "" {
			mounts = append(mounts, mount{DiskID: id, Path: machine.VolumePath(machine.Linux, machine.VolumeData)})
		}
		if err := d.api.start(ctx, sb.ID, mounts); err != nil {
			return nil, fmt.Errorf("archil: start %s: %w", inst.Name, err)
		}
	}
	if _, err := d.api.waitFor(ctx, sb.ID, "running"); err != nil {
		return nil, err
	}
	if opts.Console != nil {
		fmt.Fprintf(opts.Console, "archil: %s (%s) is running\n", inst.Name, sb.Name)
	}
	m := newMachine(d.api, sb.ID, inst.Name)
	if err := m.waitReady(ctx); err != nil {
		return nil, err
	}
	// Archil's supervisor starts systemd as the guest boots; a start
	// installs and registers nothing. One whose systemd is not launched is
	// not left running.
	if err := m.waitLaunched(ctx); err != nil {
		stopCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Minute)
		defer cancel()
		_ = d.stopAndWait(stopCtx, sb.ID)
		return nil, err
	}
	return m, nil
}

// Boot is Start, since Archil runs the sandbox, but for one Archil paused (or
// is pausing), which is machine.ErrPaused: it is held, and Start resumes it.
func (d *Driver) Boot(ctx context.Context, inst machine.Instance, opts machine.BootOptions) (machine.Machine, error) {
	sb, err := d.sandboxOf(ctx, inst)
	if err != nil {
		return nil, err
	}
	if sb.Status == "paused" || sb.Status == "pausing" {
		return nil, fmt.Errorf("archil: %s is %s: %w", inst.Name, sb.Status, machine.ErrPaused)
	}
	return d.Start(ctx, inst, opts)
}

// State is the sandbox's as Archil has it, read without waking it: running,
// paused (or pausing) at its session's limit, which Start resumes, or else
// stopped, as Attach has it.
func (d *Driver) State(ctx context.Context, inst machine.Instance) (machine.State, error) {
	sb, err := d.sandboxOf(ctx, inst)
	switch {
	case isNotFound(err):
		return machine.StateStopped, nil
	case err != nil:
		return "", err
	}
	switch sb.Status {
	case "running":
		return machine.StateRunning, nil
	case "paused", "pausing":
		return machine.StatePaused, nil
	default:
		return machine.StateStopped, nil
	}
}

// Attach is the running sandbox's machine. One Archil paused (or is pausing),
// at its session's limit, is machine.ErrPaused, and is left paused: Start
// alone resumes it.
func (d *Driver) Attach(ctx context.Context, inst machine.Instance) (machine.Machine, error) {
	sb, err := d.sandboxOf(ctx, inst)
	switch {
	case isNotFound(err):
		return nil, fmt.Errorf("archil: %s is gone: %w", inst.Name, machine.ErrNotRunning)
	case err != nil:
		return nil, err
	}
	switch sb.Status {
	case "running":
	case "paused", "pausing":
		return nil, fmt.Errorf("archil: %s is %s: %w", inst.Name, sb.Status, machine.ErrPaused)
	default:
		return nil, fmt.Errorf("archil: %s is %s: %w", inst.Name, sb.Status, machine.ErrNotRunning)
	}
	return newMachine(d.api, sb.ID, inst.Name), nil
}

// Commit leaves the stopped VM's sandbox as it stands, with no mounts, no
// data volume, and no port token: what is in it is the caller's. A sandbox with mounts is
// started once with none and stopped, since only a start changes a sandbox's
// mounts and a fork boots with its source's, and a fork of a disk deleted
// fails. Nothing records the commit (ADR 26-11-03-606).
func (d *Driver) Commit(ctx context.Context, inst machine.Instance) (machine.Instance, error) {
	sb, err := d.sandboxOf(ctx, inst)
	if err != nil {
		return machine.Instance{}, err
	}
	if !stopped(sb) {
		return machine.Instance{}, fmt.Errorf("archil: commit needs a stopped VM; %s is %s", inst.Name, sb.Status)
	}
	data, err := d.dataDisk(ctx, inst.Name)
	if err != nil {
		return machine.Instance{}, err
	}
	if len(sb.Mounts) > 0 || data != "" {
		if err := d.api.start(ctx, sb.ID, []mount{}); err != nil {
			return machine.Instance{}, fmt.Errorf("archil: start %s to drop its mounts: %w", inst.Name, err)
		}
		if _, err := d.api.waitFor(ctx, sb.ID, "running"); err != nil {
			return machine.Instance{}, err
		}
		if err := d.stopAndWait(ctx, sb.ID); err != nil {
			return machine.Instance{}, err
		}
	}
	// A committed VM never runs, and a request to it with a token would
	// start it.
	if err := d.revokeTokens(ctx, sb.ID); err != nil {
		return machine.Instance{}, fmt.Errorf("archil: %s: %w", inst.Name, err)
	}
	if err := d.removeDataDisk(ctx, inst.Name); err != nil {
		return machine.Instance{}, err
	}
	return d.Instance(ctx, inst.Name)
}

// IssueKey can't: an Archil API token never expires, and reaches the whole
// account.
func (*Driver) IssueKey(context.Context, machine.IssueRequest) (machine.Key, error) {
	return machine.Key{}, fmt.Errorf("archil: an Archil token never expires: %w", machine.ErrCannotIssue)
}

// IssuedKeys is none.
func (*Driver) IssuedKeys(context.Context, string) ([]machine.Key, error) { return nil, nil }

// RevokeKey has issued nothing to revoke.
func (*Driver) RevokeKey(context.Context, machine.Key) error {
	return fmt.Errorf("archil: it issues no keys: %w", machine.ErrCannotIssue)
}

// Caches lists none: the cache volume is deferred (ADR 26-11-02-180).
func (*Driver) Caches(context.Context) ([]machine.Cache, error) { return nil, nil }

func (*Driver) SaveCache(context.Context, machine.Instance, machine.CacheSave) (machine.Cache, error) {
	return machine.Cache{}, fmt.Errorf("archil: a cache volume: %w", machine.ErrUnsupported)
}

func (*Driver) DeleteCache(context.Context, string) error {
	return fmt.Errorf("archil: a cache volume: %w", machine.ErrUnsupported)
}
