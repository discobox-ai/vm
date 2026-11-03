// Package archil is a driver for Archil (https://archil.com): Firecracker
// microVM sandboxes in the cloud, behind Archil's REST API. It runs on every
// host OS, because the hypervisor is not on the host (ADR 26-11-03-676).
//
// A VM is the sandbox named for it, dvm-<project>--<name> (names.go), which
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
	"fmt"
	"slices"
	"strings"
	"time"

	"github.com/discobox-ai/vm/pkg/guest"
	"github.com/discobox-ai/vm/pkg/machine"
)

func init() {
	machine.Register("archil", func(cfg machine.Config) (machine.Driver, error) { return New(cfg), nil }, APIKeyEnv)
}

// waitLimit bounds each wait for a sandbox to change state.
const waitLimit = 10 * time.Minute

// Driver is the archil driver.
type Driver struct {
	api     *api
	project string
	// base is the OCI image a fresh VM runs; empty is Archil's default.
	base string
}

var _ machine.Driver = (*Driver)(nil)

// New returns a driver for cfg's project (machine.DefaultProject when empty)
// whose fresh VMs run cfg.Base, and that calls Archil with the key cfg.Keys
// answers on each call. It calls nothing until it is used.
func New(cfg machine.Config) *Driver {
	project := cfg.Project
	if project == "" {
		project = machine.DefaultProject
	}
	return &Driver{api: newAPI(cfg.Keys), project: project, base: cfg.Base}
}

func (*Driver) Name() string { return "archil" }

func (*Driver) Capabilities() machine.Capabilities {
	return machine.Capabilities{
		GuestOS: []machine.OS{machine.Linux},
		Keys:    machine.KeyCapabilities{Mode: machine.KeysNone},
		Egress:  []machine.EgressFence{machine.EgressNone, machine.EgressAllow},
		Volumes: []machine.Volume{machine.VolumeData},
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
	case spec.Isolated || len(spec.Networks) > 0:
		return machine.Instance{}, fmt.Errorf("archil: isolation: %w", machine.ErrUnsupported)
	case spec.Cache != nil:
		return machine.Instance{}, fmt.Errorf("archil: a cache volume: %w", machine.ErrUnsupported)
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
	var sb sandbox
	var err error
	if spec.From == nil {
		sb, err = d.api.createSandbox(ctx, createSandbox{
			Name: d.sandboxName(spec.Name), BaseImage: d.base, VCPUs: spec.CPUs, MemoryMiB: spec.Memory >> 20,
			Network: policy(spec.Egress),
		})
		if err != nil {
			return machine.Instance{}, taken(spec.Name, fmt.Errorf("archil: create a sandbox of %s: %w", d.baseName(), err))
		}
	} else {
		from, err := d.api.sandbox(ctx, spec.From.ID)
		switch {
		case isNotFound(err) || err == nil && d.nameOf(from.Name) != spec.From.Name:
			return machine.Instance{}, fmt.Errorf("archil: VM %s: %w", spec.From.Name, machine.ErrNotFound)
		case err != nil:
			return machine.Instance{}, err
		case !stopped(from):
			return machine.Instance{}, fmt.Errorf("archil: a VM is created from a stopped one; %s is %s", spec.From.Name, from.Status)
		case len(from.Mounts) > 0:
			// A fork boots with its source's mounts: it would share the
			// source's data volume. A commit drops them.
			return machine.Instance{}, fmt.Errorf("archil: %s has a data volume mounted: commit it first", spec.From.Name)
		case (spec.CPUs != 0 && spec.CPUs != from.VCPUs) || (spec.Memory != 0 && spec.Memory>>20 != from.MemoryMiB):
			return machine.Instance{}, fmt.Errorf("archil: a VM created from %s has its size, %d vCPUs and %d MiB: %w", spec.From.Name, from.VCPUs, from.MemoryMiB, machine.ErrUnsupported)
		}
		sb, err = d.api.fork(ctx, from.ID, d.sandboxName(spec.Name))
		if err != nil {
			return machine.Instance{}, taken(spec.Name, fmt.Errorf("archil: fork %s: %w", spec.From.Name, err))
		}
	}
	made.sandbox = sb.ID
	if sb, err = d.api.waitFor(ctx, sb.ID, "running"); err != nil {
		return machine.Instance{}, err
	}
	if spec.From != nil {
		// The fence is the spec's, never its source's, which a fork keeps.
		if p := policy(spec.Egress); p != nil {
			err = d.api.setNetwork(ctx, sb.ID, *p)
		} else if had, gerr := d.api.network(ctx, sb.ID); gerr != nil {
			err = gerr
		} else if fenceOf(had) != nil {
			err = d.api.setNetwork(ctx, sb.ID, egressPolicy{})
		}
		if err != nil {
			return machine.Instance{}, fmt.Errorf("archil: fence %s: %w", spec.Name, err)
		}
	}
	got, err := d.api.network(ctx, sb.ID)
	if err != nil {
		return machine.Instance{}, err
	}
	if !sameFence(fenceOf(got), spec.Egress) {
		return machine.Instance{}, fmt.Errorf("archil: %s was fenced to %v, and Archil holds %v", spec.Name, spec.Egress, fenceOf(got))
	}
	if _, err := d.api.exposePort(ctx, sb.ID, guest.ServicePort); err != nil {
		return machine.Instance{}, fmt.Errorf("archil: expose port %d: %w", guest.ServicePort, err)
	}
	if err := d.stopAndWait(ctx, sb.ID); err != nil {
		return machine.Instance{}, err
	}
	// The name is this VM's now, so a disk of its data volume's name is one
	// an earlier VM of the name left (a Destroy stopped before its last
	// step): Start would mount it, and Archil would answer it rather than
	// make an empty one.
	if err := d.removeDataDisk(ctx, spec.Name); err != nil {
		return machine.Instance{}, err
	}
	if spec.Data != nil {
		id, err := d.api.createDisk(ctx, d.dataDiskName(spec.Name))
		if err != nil {
			return machine.Instance{}, fmt.Errorf("archil: create the data volume: %w", err)
		}
		made.disk = id
	}
	if sb, err = d.api.sandbox(ctx, sb.ID); err != nil {
		return machine.Instance{}, err
	}
	inst := d.instance(spec.Name, sb, spec.Data != nil)
	inst.Egress = fenceOf(got)
	if spec.From != nil {
		inst.From = spec.From.Name
	}
	return inst, nil
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
		ID: sb.ID, Name: name, GuestOS: machine.Linux,
		CPUs: sb.VCPUs, Memory: sb.MemoryMiB << 20, Created: sb.Created,
	}
	if data {
		inst.Data = &machine.DataVolume{}
	}
	return inst
}

// instances reads the project's VMs, a name's alone when name is not empty,
// with their fences and data volumes.
func (d *Driver) instances(ctx context.Context, name string) ([]machine.Instance, error) {
	all, err := d.api.sandboxes(ctx)
	if err != nil {
		return nil, fmt.Errorf("archil: list sandboxes: %w", err)
	}
	var mine []sandbox
	for _, sb := range all {
		if n := d.nameOf(sb.Name); n != "" && (name == "" || n == name) && sb.Status != "deleted" && sb.Status != "deleting" {
			mine = append(mine, sb)
		}
	}
	if len(mine) == 0 {
		return nil, nil
	}
	disks, err := d.api.disks(ctx, "")
	if err != nil {
		return nil, fmt.Errorf("archil: list disks: %w", err)
	}
	names := map[string]bool{}
	for _, dk := range disks {
		names[dk.Name] = true
	}
	var out []machine.Instance
	for _, sb := range mine {
		n := d.nameOf(sb.Name)
		inst := d.instance(n, sb, names[d.dataDiskName(n)])
		if p, err := d.api.network(ctx, sb.ID); err == nil {
			inst.Egress = fenceOf(p)
		} else if !isNotFound(err) {
			return nil, err
		}
		out = append(out, inst)
	}
	slices.SortFunc(out, func(a, b machine.Instance) int { return a.Created.Compare(b.Created) })
	return out, nil
}

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

// sandboxOf is the VM's sandbox, which must still be the one named for it.
func (d *Driver) sandboxOf(ctx context.Context, inst machine.Instance) (sandbox, error) {
	sb, err := d.api.sandbox(ctx, inst.ID)
	if err == nil && d.nameOf(sb.Name) != inst.Name {
		return sb, fmt.Errorf("archil: sandbox %s is not VM %s", sb.Name, inst.Name)
	}
	return sb, err
}

// Destroy stops and deletes the VM's sandbox, then its data volume. Archil
// refuses a sandbox with forks, which is machine.ErrInUse: a VM created from
// it still needs it. A sandbox already gone still has its data volume
// removed.
func (d *Driver) Destroy(ctx context.Context, inst machine.Instance) error {
	sb, err := d.sandboxOf(ctx, inst)
	if isNotFound(err) {
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
	host, err := d.api.exposePort(ctx, inst.ID, guest.ServicePort)
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
