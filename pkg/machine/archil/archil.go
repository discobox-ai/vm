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
// disco-vm puts nothing in the guest: what the image holds, and what runs in
// it, is the caller's. Archil's init is the VM's PID 1, and the guest's
// operations are Archil's process API (guest.go).
package archil

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"slices"
	"strings"
	"time"

	"github.com/discobox-ai/vm/pkg/guest"
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
	return &Driver{api: newAPI(conf, cfg.Keys, cfg.HTTPTransport()), pool: pool, base: cfg.Base}, nil
}

func (*Driver) Name() string { return "archil" }

func (*Driver) Capabilities() machine.Capabilities {
	return machine.Capabilities{
		GuestOS: []machine.OS{machine.Linux},
		Keys:    machine.KeyCapabilities{Mode: machine.KeysNone},
		Egress:  []machine.EgressFence{machine.EgressNone, machine.EgressAllow},
		Volumes: []machine.Volume{machine.VolumeData},
		// A committed VM is its stopped sandbox, and a copy a fork of it.
		Commit: true,
	}
}

// Check lists the account's sandboxes, which needs a region and a key.
func (d *Driver) Check(ctx context.Context) error {
	_, err := d.api.sandboxes(ctx)
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

// stopAndWait stops a sandbox that runs and waits until it has.
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
		if err := d.api.stop(ctx, id); err != nil && !isConflict(err) {
			return fmt.Errorf("archil: stop %s: %w", s.Name, err)
		}
	}
	_, err = d.api.waitFor(ctx, id, "stopped", "exited")
	return err
}

func stopped(s sandbox) bool { return s.Status == "stopped" || s.Status == "exited" }

// policy is the network policy of an egress fence; nil is none.
func policy(e *machine.Egress) *egressPolicy {
	if e == nil {
		return nil
	}
	p := &egressPolicy{}
	p.Egress = &struct {
		Default string `json:"default"`
		Allow   []any  `json:"allow,omitempty"`
	}{Default: "deny"}
	for _, a := range e.Allow {
		p.Egress.Allow = append(p.Egress.Allow, a)
	}
	return p
}

// fenceOf is the fence a policy holds: nil when egress is not denied by
// default.
func fenceOf(p egressPolicy) *machine.Egress {
	if p.Egress == nil || p.Egress.Default != "deny" {
		return nil
	}
	e := &machine.Egress{}
	for _, a := range p.Egress.Allow {
		switch v := a.(type) {
		case string:
			e.Allow = append(e.Allow, v)
		case map[string]any:
			if t, ok := v["target"].(string); ok {
				e.Allow = append(e.Allow, t)
			}
		}
	}
	return e
}

func sameFence(a, b *machine.Egress) bool {
	if a == nil || b == nil {
		return a == nil && b == nil
	}
	return slices.Equal(a.Allow, b.Allow)
}

// Create makes the VM's sandbox, of the base or forked from spec.From, with
// its port 7302 exposed and its fence in place, and its data volume, and
// stops it. Archil refuses a sandbox name it has, so the name is taken
// atomically, before anything else is made.
func (d *Driver) Create(ctx context.Context, spec machine.InstanceSpec) (machine.Instance, error) {
	if err := machine.ValidateName(spec.Name); err != nil {
		return machine.Instance{}, err
	}
	if err := d.checkName(spec.Name); err != nil {
		return machine.Instance{}, err
	}
	switch {
	case len(spec.Forwards) > 0:
		return machine.Instance{}, fmt.Errorf("archil: forwards: %w", machine.ErrUnsupported)
	case spec.From != nil && machine.OtherPool(spec.From.Pool, d.pool):
		return machine.Instance{}, fmt.Errorf("archil: a VM from another pool's (%s): %w", spec.From.Pool, machine.ErrUnsupported)
	case spec.Isolated || len(spec.Networks) > 0:
		return machine.Instance{}, fmt.Errorf("archil: isolation: %w", machine.ErrUnsupported)
	case spec.Cache != nil:
		return machine.Instance{}, fmt.Errorf("archil: a cache volume: %w", machine.ErrUnsupported)
	}
	// A data disk of the name with no sandbox is a VM caught mid-upgrade,
	// or one a Destroy stopped before its last step: listed, so the name is
	// taken.
	if id, err := d.dataDisk(ctx, spec.Name); err != nil {
		return machine.Instance{}, err
	} else if id != "" {
		return machine.Instance{}, fmt.Errorf("archil: a VM named %q, mid-upgrade or half removed: %w", spec.Name, machine.ErrExists)
	}
	made := &made{d: d}
	inst, err := d.create(ctx, spec, made)
	if err != nil {
		made.undo()
		return machine.Instance{}, err
	}
	return inst, nil
}

// made is what a Create has made so far, removed if it fails.
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
	sb, fence, err := d.makeSandbox(ctx, spec, src, made)
	if err != nil {
		return machine.Instance{}, err
	}
	if spec.Data != nil {
		id, err := d.api.createDisk(ctx, d.dataDiskName(spec.Name))
		if err != nil {
			return machine.Instance{}, fmt.Errorf("archil: create the data volume: %w", err)
		}
		made.disk = id
	}
	inst := d.instance(spec.Name, sb, spec.Data != nil)
	inst.Egress = fence
	if spec.From != nil {
		inst.From = spec.From.Name
	}
	return inst, nil
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

// makeSandbox makes the VM's sandbox, fresh from the base or a fork of src,
// with the spec's size and fence, which it reads back; exposes 7302; and
// leaves it stopped. It returns the sandbox and the fence Archil holds.
func (d *Driver) makeSandbox(ctx context.Context, spec machine.InstanceSpec, src *sandbox, made *made) (sandbox, *machine.Egress, error) {
	var sb sandbox
	var err error
	if src == nil {
		sb, err = d.api.createSandbox(ctx, createSandbox{
			Name: d.sandboxName(spec.Name), BaseImage: d.base, VCPUs: spec.CPUs, MemoryMiB: spec.Memory >> 20,
			Network: policy(spec.Egress),
		})
		if err != nil {
			return sb, nil, taken(spec.Name, fmt.Errorf("archil: create a sandbox of %s: %w", d.baseName(), err))
		}
	} else {
		sb, err = d.api.fork(ctx, src.ID, d.sandboxName(spec.Name))
		if err != nil {
			return sb, nil, taken(spec.Name, fmt.Errorf("archil: fork %s: %w", spec.From.Name, err))
		}
	}
	made.sandbox = sb.ID
	if sb, err = d.api.waitFor(ctx, sb.ID, "running"); err != nil {
		return sb, nil, err
	}
	if src != nil {
		// The fence is the spec's, never its source's, which a fork keeps.
		if p := policy(spec.Egress); p != nil {
			err = d.api.setNetwork(ctx, sb.ID, *p)
		} else if had, gerr := d.api.network(ctx, sb.ID); gerr != nil {
			err = gerr
		} else if fenceOf(had) != nil {
			err = d.api.setNetwork(ctx, sb.ID, egressPolicy{})
		}
		if err != nil {
			return sb, nil, fmt.Errorf("archil: fence %s: %w", spec.Name, err)
		}
	}
	got, err := d.api.network(ctx, sb.ID)
	if err != nil {
		return sb, nil, err
	}
	if !sameFence(fenceOf(got), spec.Egress) {
		return sb, nil, fmt.Errorf("archil: %s was fenced to %v, and Archil holds %v", spec.Name, spec.Egress, fenceOf(got))
	}
	if _, err := d.api.exposePort(ctx, sb.ID, guest.ServicePort); err != nil {
		return sb, nil, fmt.Errorf("archil: expose port %d: %w", guest.ServicePort, err)
	}
	if err := d.stopAndWait(ctx, sb.ID); err != nil {
		return sb, nil, err
	}
	if sb, err = d.api.sandbox(ctx, sb.ID); err != nil {
		return sb, nil, err
	}
	return sb, fenceOf(got), nil
}

// Upgrade gives the stopped VM a new root (ADR 26-11-03-782): its sandbox
// deleted and made again under its name, fresh from the base or a fork of
// from, with its size and fence, mounting its data disk at each start as
// before. Everything that could refuse it is checked before the sandbox is
// deleted. One stopped between the two is listed by its data disk, and
// Upgrade again finishes it with the size and fence inst carries: the
// listing's are none and the strictest fence. A VM with no data disk is
// gone if the upgrade fails or stops after its sandbox is deleted, since
// nothing of it is left.
func (d *Driver) Upgrade(ctx context.Context, inst machine.Instance, from *machine.Instance) (machine.Instance, error) {
	spec := machine.InstanceSpec{Name: inst.Name, From: from, CPUs: inst.CPUs, Memory: inst.Memory, Egress: inst.Egress}
	old, err := d.sandboxOf(ctx, inst)
	switch {
	case errors.Is(err, errUpgrading):
		old = sandbox{}
	case isNotFound(err):
		return machine.Instance{}, fmt.Errorf("archil: VM %s: %w", inst.Name, machine.ErrNotFound)
	case err != nil:
		return machine.Instance{}, err
	case !stopped(old) && old.Status != "failed":
		// failed is a guest that powered itself off, stopped: the engine
		// reports it stopped, and Archil deletes it.
		return machine.Instance{}, fmt.Errorf("archil: an upgrade needs a stopped VM; %s is %s", inst.Name, old.Status)
	default:
		got, err := d.api.network(ctx, old.ID)
		if err != nil {
			return machine.Instance{}, err
		}
		spec.CPUs, spec.Memory, spec.Egress = old.VCPUs, old.MemoryMiB<<20, fenceOf(got)
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
	data, err := d.dataDisk(ctx, inst.Name)
	if err != nil {
		return machine.Instance{}, err
	}
	if old.ID != "" {
		if err := d.api.deleteSandbox(ctx, old.ID); err != nil {
			if _, code := statusOf(err); code == "sandbox_has_forks" {
				return machine.Instance{}, fmt.Errorf("archil: %s has forks: %w", inst.Name, machine.ErrInUse)
			}
			return machine.Instance{}, fmt.Errorf("archil: delete %s: %w", old.Name, err)
		}
		if err := d.waitGone(ctx, old.ID); err != nil {
			return machine.Instance{}, err
		}
	}
	// A failure from here removes the new sandbox, which may be a fork
	// still holding its source's fence. A VM with a data disk is left
	// mid-upgrade, listed by it; one without is gone.
	made := &made{d: d}
	sb, fence, err := d.makeSandbox(ctx, spec, src, made)
	if err != nil {
		made.undo()
		if data == "" {
			return machine.Instance{}, fmt.Errorf("archil: upgrade %s failed after its sandbox was deleted, and with no data volume nothing of it is left: %w", inst.Name, err)
		}
		return machine.Instance{}, fmt.Errorf("archil: upgrade %s, which is left mid-upgrade (upgrade it again, or remove it): %w", inst.Name, err)
	}
	upgraded := d.instance(inst.Name, sb, data != "")
	upgraded.Egress = fence
	if from != nil {
		upgraded.From = from.Name
	}
	return upgraded, nil
}

// waitGone waits until Archil has deleted a sandbox, so its name is free.
func (d *Driver) waitGone(ctx context.Context, id string) error {
	ctx, cancel := context.WithTimeout(ctx, waitLimit)
	defer cancel()
	for {
		s, err := d.api.sandbox(ctx, id)
		if isNotFound(err) || err == nil && s.Status == "deleted" {
			return nil
		}
		if err != nil {
			return err
		}
		select {
		case <-ctx.Done():
			return fmt.Errorf("archil: sandbox %s is still %s: %w", s.Name, s.Status, ctx.Err())
		case <-time.After(time.Second):
		}
	}
}

func (d *Driver) baseName() string {
	if d.base == "" {
		return "Archil's default image"
	}
	return d.base
}

// instance is a VM as its sandbox reports it. Archil does not say which
// sandbox a fork came from, so From is empty.
func (d *Driver) instance(name string, sb sandbox, data bool) machine.Instance {
	inst := machine.Instance{
		ID: sb.ID, Name: name, Pool: d.pool, GuestOS: machine.Linux,
		CPUs: sb.VCPUs, Memory: sb.MemoryMiB << 20, Created: sb.Created,
	}
	if data {
		inst.Data = &machine.DataVolume{}
	}
	return inst
}

// instances reads the pool's VMs, a name's alone when name is not empty,
// with their fences and data volumes. A data disk with no sandbox is a VM
// caught mid-upgrade (or one a Destroy stopped before its last step): listed
// by the disk's ID, with no size, and the strictest fence, since Archil keeps
// nothing else of it while it has no sandbox.
func (d *Driver) instances(ctx context.Context, name string) ([]machine.Instance, error) {
	all, err := d.api.sandboxes(ctx)
	if err != nil {
		return nil, fmt.Errorf("archil: list sandboxes: %w", err)
	}
	var mine []sandbox
	has := map[string]bool{}
	for _, sb := range all {
		if n := d.nameOf(sb.Name); n != "" && (name == "" || n == name) && live(sb) {
			mine = append(mine, sb)
			has[n] = true
		}
	}
	query := ""
	if name != "" {
		query = d.dataDiskName(name)
	}
	disks, err := d.api.disks(ctx, query)
	if err != nil {
		return nil, fmt.Errorf("archil: list disks: %w", err)
	}
	data := map[string]disk{}
	for _, dk := range disks {
		if n := d.nameOfDisk(dk.Name); n != "" && (name == "" || n == name) {
			data[n] = dk
		}
	}
	var out []machine.Instance
	for _, sb := range mine {
		n := d.nameOf(sb.Name)
		_, hasData := data[n]
		inst := d.instance(n, sb, hasData)
		if p, err := d.api.network(ctx, sb.ID); err == nil {
			inst.Egress = fenceOf(p)
		} else if !isNotFound(err) {
			return nil, err
		}
		out = append(out, inst)
	}
	for n, dk := range data {
		if !has[n] {
			out = append(out, machine.Instance{
				ID: dk.ID, Name: n, Pool: d.pool, GuestOS: machine.Linux, Created: dk.Created,
				Egress: &machine.Egress{}, Data: &machine.DataVolume{},
			})
		}
	}
	slices.SortFunc(out, func(a, b machine.Instance) int { return a.Created.Compare(b.Created) })
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

// errUpgrading is a VM whose sandbox is gone and whose data disk is not: an
// upgrade stopped between deleting its sandbox and making the new one.
var errUpgrading = errors.New("mid-upgrade: upgrade it again, or remove it")

// sandboxOf is the VM's sandbox: the one inst names by ID, or, once an
// upgrade has made it again, the live one named for the VM, so an Instance
// from before an upgrade still names it. A VM with neither is not found, or
// errUpgrading when its data disk is left.
func (d *Driver) sandboxOf(ctx context.Context, inst machine.Instance) (sandbox, error) {
	if !strings.HasPrefix(inst.ID, "dsk-") {
		sb, err := d.api.sandbox(ctx, inst.ID)
		switch status, _ := statusOf(err); {
		case err == nil && d.nameOf(sb.Name) == inst.Name && live(sb):
			return sb, nil
		case err != nil && status != http.StatusNotFound && status != http.StatusBadRequest:
			return sb, err
		}
	}
	all, err := d.api.sandboxes(ctx)
	if err != nil {
		return sandbox{}, err
	}
	for _, sb := range all {
		if sb.Name == d.sandboxName(inst.Name) && live(sb) {
			return sb, nil
		}
	}
	if id, err := d.dataDisk(ctx, inst.Name); err != nil {
		return sandbox{}, err
	} else if id != "" {
		return sandbox{}, fmt.Errorf("archil: %s is %w", inst.Name, errUpgrading)
	}
	return sandbox{}, &apiError{Status: http.StatusNotFound, Message: fmt.Sprintf("VM %s is gone", inst.Name)}
}

// Destroy stops and deletes the VM's sandbox, then its data volume. Archil
// refuses a sandbox with forks, which is machine.ErrInUse: a VM created from
// it still needs it. A sandbox already gone still has its data volume
// removed.
func (d *Driver) Destroy(ctx context.Context, inst machine.Instance) error {
	sb, err := d.sandboxOf(ctx, inst)
	if isNotFound(err) || errors.Is(err, errUpgrading) {
		return d.removeDataDisk(ctx, inst.Name)
	}
	if err != nil {
		return err
	}
	if err := d.stopAndWait(ctx, sb.ID); err != nil {
		return err
	}
	if err := d.api.deleteSandbox(ctx, sb.ID); err != nil {
		if _, code := statusOf(err); code == "sandbox_has_forks" {
			return fmt.Errorf("archil: %s has forks: %w", inst.Name, machine.ErrInUse)
		}
		return fmt.Errorf("archil: delete %s: %w", sb.Name, err)
	}
	return d.removeDataDisk(ctx, inst.Name)
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
// Archil paused, and waits until a process runs in it. Archil runs the
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
	return m, nil
}

func (d *Driver) Boot(ctx context.Context, inst machine.Instance, opts machine.BootOptions) (machine.Machine, error) {
	return d.Start(ctx, inst, opts)
}

// Attach is the running sandbox's machine. One Archil paused at its session's
// limit is running as far as the VM goes, and is left paused: the machine's
// first exec or copy resumes it, so a listing (Engine.State) wakes nothing.
func (d *Driver) Attach(ctx context.Context, inst machine.Instance) (machine.Machine, error) {
	sb, err := d.sandboxOf(ctx, inst)
	switch {
	case errors.Is(err, errUpgrading):
		return nil, fmt.Errorf("archil: %s is %w: %w", inst.Name, errUpgrading, machine.ErrNotRunning)
	case isNotFound(err):
		return nil, fmt.Errorf("archil: %s is gone: %w", inst.Name, machine.ErrNotRunning)
	case err != nil:
		return nil, err
	}
	m := newMachine(d.api, sb.ID, inst.Name)
	switch sb.Status {
	case "running":
	case "paused", "pausing":
		// Resumed by the first operation that needs the guest, so a
		// listing (Engine.State) does not wake every paused sandbox.
		m.paused.Store(true)
	default:
		return nil, fmt.Errorf("archil: %s is %s: %w", inst.Name, sb.Status, machine.ErrNotRunning)
	}
	return m, nil
}

// Commit leaves the stopped VM's sandbox as it stands, with no mounts and no
// data volume: what is in it is the caller's. A sandbox with mounts is
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
	if err := d.removeDataDisk(ctx, inst.Name); err != nil {
		return machine.Instance{}, err
	}
	return d.Instance(ctx, inst.Name)
}

// Service is guest.ServicePort, exposed at Create, at the hostname Archil
// gives it, which lasts across stop and start. Archil gives sandboxes no path
// to each other, and the URL reaches them, so it is the private URL too.
func (d *Driver) Service(ctx context.Context, inst machine.Instance) (machine.Service, error) {
	service := machine.Service{Listen: fmt.Sprintf("tcp:0.0.0.0:%d", guest.ServicePort)}
	sb, err := d.sandboxOf(ctx, inst)
	if err != nil {
		return service, fmt.Errorf("archil: %s's service: %w", inst.Name, err)
	}
	host, err := d.api.exposePort(ctx, sb.ID, guest.ServicePort)
	if err != nil {
		return service, fmt.Errorf("archil: %s's service: %w", inst.Name, err)
	}
	service.URL = "https://" + host
	service.PrivateURL = service.URL
	return service, nil
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
