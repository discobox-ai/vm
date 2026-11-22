// Package boxd is a driver for boxd (https://boxd.sh): Linux microVMs in the
// cloud, reached through boxd's public gRPC API. It runs on every host OS,
// because the hypervisor is not on the host.
//
// boxd has no disk images to hand out, only machines and snapshots of them,
// and keeps no labels on either, so the driver maps the seam's VMs (ADR
// 26-11-03-606) onto their names and keeps nothing on the host (ADR
// 26-10-23-417):
//
//   - A VM is a boxd machine named dvm-<pool>--<name>: fresh, made from
//     the driver's base (machine.Config.Base, or boxd's default image), or
//     restored from a committed VM's snapshot, and powered off in order once
//     made, so its first boot is a cold boot of its disk.
//   - A committed VM is a boxd snapshot (memory and disk) of the guest booted
//     from the committed disk, under the same name, and its machine deleted.
//     A snapshot is whole: no VM restored from one needs it again.
//
// There is no install: a fresh VM starts from boxd's image.
//
// There is no hypervisor socket to the guest, and no disco-vm agent in it.
// The guest's operations (machine.Guest) are boxd's authenticated Exec stream:
// a command through sudo, tar over its stdin and stdout for copies (guest.go).
// The instance's service is reached at its ingress URL, and no guest port is
// dialed. A machine's power is boxd's: a guest that powers itself off from
// inside is left running by boxd, and reads as running until it is stopped.
package boxd

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"slices"
	"sync"
	"sync/atomic"
	"time"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	"github.com/discobox-ai/vm/pkg/guest"
	"github.com/discobox-ai/vm/pkg/machine"
	"github.com/discobox-ai/vm/pkg/machine/boxd/internal/boxdapi"
)

func init() {
	machine.Register("boxd", machine.Registration{
		New:        func(cfg machine.Config) (machine.Driver, error) { return New(cfg) },
		Definition: Definition(),
		Validate:   Validate,
	})
}

const (
	// waitLimit bounds each wait for a machine or snapshot to change state.
	waitLimit = 10 * time.Minute

	// maxMachineName is the longest name a machine can have: it is a DNS
	// label in its proxy's domain.
	maxMachineName = 63
)

// appearLimit bounds the wait for boxd to find a machine it has just made,
// and appearEvery is how often a call boxd answered NotFound is made again
// meanwhile.
var (
	appearLimit = time.Minute
	appearEvery = 250 * time.Millisecond
)

// waitEvery is how often a wait for a machine's state asks for it: a boot
// is about a second (2026-11-16, nyc and fra), so a second's poll cost up
// to one again.
var waitEvery = 200 * time.Millisecond

// raceGrace is how long a wait goes on reading a machine whose boot boxd
// refuses, FailedPrecondition, in one state: boxd moved it on and has yet to
// say so. A suspended machine StartVm resumed read suspended, and refused
// ResumeVm "VM is starting", and ran a second later (fra, 2026-11-22).
var raceGrace = 10 * time.Second

// Driver is the boxd driver.
type Driver struct {
	api  *client
	pool string
	// base is the image a fresh VM is made from; empty is boxd's default
	// image (GetConfig).
	base string
	// maxMachines is the key's org's machine limit, as Check (or a create
	// refused at it) last read it; zero until then.
	maxMachines atomic.Int64
}

var _ machine.Driver = (*Driver)(nil)

// New returns a driver for cfg's pool (machine.DefaultPool when empty),
// on the cluster and in the region its configuration (cfg.DriverConfig)
// names, that
// authenticates with the key cfg.Keys answers on each call. It dials
// nothing until it is used.
func New(cfg machine.Config) (*Driver, error) {
	config, err := machine.Normalize("boxd", cfg.DriverConfig)
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
	return &Driver{api: newClient(conf, cfg), pool: pool, base: cfg.Base}, nil
}

func (*Driver) Name() string { return "boxd" }

func (d *Driver) Capabilities() machine.Capabilities {
	var maxMachines map[machine.OS]int
	if n := d.maxMachines.Load(); n > 0 {
		// The org's limit counts every machine billed to it, stopped ones
		// and other members' included, and boxd refuses a create past it.
		maxMachines = map[machine.OS]int{machine.Linux: int(n)}
	}
	return machine.Capabilities{
		MaxMachines: maxMachines,
		GuestOS:     []machine.OS{machine.Linux},
		Commit:      true,
		Pause:       true,
		Keys:        keyCapabilities,
		Bare:        machine.BareStrip,
		Networks:    true,
		Egress:      []machine.EgressFence{machine.EgressNone, machine.EgressAllow},
		// Ingress is the default proxy, closed for a VM whose ingress URL
		// is asked off; the pool URL is the direct one on networks, else
		// the ingress one.
		URLs: machine.URLCapabilities{Ingress: machine.URLOptional, Pool: machine.URLAlways},
	}
}

// Check asks boxd who the key is, at the home endpoint, and reads its org's
// machine limit (Capabilities.MaxMachines). It fails when a host-bound
// secret of the org or of the key's owner would reach the driver's
// machines, since boxd lets a machine reach such a secret's hosts whatever
// its egress allowlist says. It also finds the region's endpoint, which
// needs the region to be one the key's org has enabled, and lists the
// machines there, so an endpoint that does not answer, or refuses the key,
// fails here rather than at the first create.
func (d *Driver) Check(ctx context.Context) error {
	account, err := d.api.Account()
	if err != nil {
		return err
	}
	ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	if _, err := account.Whoami(ctx, &boxdapi.WhoamiRequest{}); err != nil {
		return fmt.Errorf("boxd: %s: %w", d.api.addr, err)
	}
	limit, err := d.machineLimit(ctx, account)
	if err != nil {
		return err
	}
	if err := hostBoundSecrets(ctx, account, limit.GetOrgName()); err != nil {
		return err
	}
	api, err := d.api.API(ctx)
	if err != nil {
		return err
	}
	if _, err := api.ListVms(ctx, &boxdapi.ListVmsRequest{}); err != nil {
		return fmt.Errorf("boxd: region %s: %w", d.api.region, err)
	}
	return nil
}

// vmRef is a machine the driver reaches.
type vmRef struct {
	ID   string `json:"id"`
	Name string `json:"name"`
}

// prefix starts the name of everything the driver makes for its pool.
func (d *Driver) prefix() string { return machine.ResourcePrefix(namePrefix, d.pool) }

// machineName is a VM's machine, and a committed VM's snapshot.
func (d *Driver) machineName(name string) string { return d.prefix() + name }

// Install is not offered: a fresh VM starts from boxd's image instead.
func (*Driver) Install(context.Context, machine.InstallSpec, string) (machine.Instance, error) {
	return machine.Instance{}, fmt.Errorf("boxd: installs nothing from media; create a fresh VM from its image: %w", machine.ErrUnsupported)
}

// baseImage is the image a fresh VM is made from: the driver's configured
// one, or boxd's default.
func (d *Driver) baseImage(ctx context.Context, api boxdapi.BoxdApiClient) (string, error) {
	if d.base != "" {
		return d.base, nil
	}
	config, err := api.GetConfig(ctx, &boxdapi.GetConfigRequest{})
	if err != nil {
		return "", fmt.Errorf("boxd: read the default image: %w", err)
	}
	if config.GetDefaultImage() == "" {
		return "", fmt.Errorf("boxd: no default image to make a fresh VM from: %w", machine.ErrUnsupported)
	}
	return config.GetDefaultImage(), nil
}

// Create makes the VM's machine: restored from the committed VM's snapshot,
// or made fresh from the driver's image, at the size it asks for, fenced as
// it asks the moment boxd answers, with its default proxy pinned to the
// service port, or closed if its ingress URL is asked off; then
// powered off in order, so its first boot is a cold boot.
func (d *Driver) Create(ctx context.Context, spec machine.InstanceSpec) (machine.Instance, error) {
	if err := machine.ValidateName(spec.Name); err != nil {
		return machine.Instance{}, err
	}
	name := d.machineName(spec.Name)
	if len(name) > maxMachineName {
		return machine.Instance{}, fmt.Errorf("boxd: VM name %q is too long: its machine %s must be at most %d characters", spec.Name, name, maxMachineName)
	}
	if len(spec.Forwards) > 0 {
		return machine.Instance{}, fmt.Errorf("boxd: forwards: %w", machine.ErrUnsupported)
	}
	if spec.From != nil && machine.OtherPool(spec.From.Pool, d.pool) {
		return machine.Instance{}, fmt.Errorf("boxd: a VM from another pool's (%s): %w", spec.From.Pool, machine.ErrUnsupported)
	}
	if err := spec.URLs.Check(d.Capabilities().URLs); err != nil {
		return machine.Instance{}, fmt.Errorf("boxd: %w", err)
	}
	if !d.Capabilities().Fences(spec.Egress) {
		// boxd's allowlist leaves DNS open: it cannot close it.
		return machine.Instance{}, fmt.Errorf("boxd: a fence of %+v: %w", *spec.Egress, machine.ErrUnsupported)
	}
	if spec.URLs.Pool == (machine.URLRequest{Mode: machine.URLOn, Strict: true}) && len(spec.Networks) == 0 && spec.URLs.Ingress.Mode == machine.URLOff {
		// Peers reach a machine on no networks at its ingress URL alone.
		return machine.Instance{}, fmt.Errorf("boxd: a pool URL on no networks, with the ingress URL off: %w", machine.ErrUnsupported)
	}
	if spec.Data != nil || spec.Cache != nil {
		return machine.Instance{}, fmt.Errorf("boxd: a data or cache volume: %w", machine.ErrUnsupported)
	}
	restore := spec.From != nil
	if restore && len(spec.Networks) > 0 && !spec.Bare {
		// A restore takes no networks: they are set once it is made, and a
		// machine that is not isolated is on the default network until then.
		return machine.Instance{}, fmt.Errorf("boxd: networks on a VM created from a committed one need it isolated, or it peers on the default network until they are set: %w", machine.ErrUnsupported)
	}
	api, err := d.api.API(ctx)
	if err != nil {
		return machine.Instance{}, err
	}
	// A committed VM holds its name with no machine, which boxd would let a
	// new machine take, so the snapshot of the name is asked for, at once
	// with a restore's own; a machine of the name is boxd's to refuse
	// (taken, below).
	var from *boxdapi.SnapshotInfo
	var fromErr error
	var reading sync.WaitGroup
	if restore {
		// boxd restores only a snapshot: a From that is a machine is no
		// committed VM boxd has.
		reading.Go(func() { from, fromErr = d.snapshotOf(ctx, api, *spec.From) })
	}
	held, err := snapshotNamed(ctx, api, name)
	reading.Wait()
	switch {
	case err != nil:
		return machine.Instance{}, err
	case held.GetStatus() == "ready" || held.GetStatus() == "failed":
		return machine.Instance{}, fmt.Errorf("boxd: a VM named %q: %w", spec.Name, machine.ErrExists)
	}
	config := &boxdapi.VmConfig{Vcpu: uint32(spec.CPUs), MemoryBytes: spec.Memory}
	var created *boxdapi.CreateVmResponse
	if restore {
		snap, err := from, fromErr
		if err != nil {
			return machine.Instance{}, err
		}
		if snap == nil {
			return machine.Instance{}, fmt.Errorf("boxd: no committed VM %s, a snapshot of that name and ID; commit it first: %w", spec.From.Name, machine.ErrNotFound)
		}
		// By name, as boxd's own restores are: a committed VM's name is
		// its snapshot's, and no second VM of the pool takes it.
		req := &boxdapi.CreateVmFromSnapshotRequest{Snapshot: d.machineName(spec.From.Name), Name: name, Isolated: spec.Bare}
		if spec.CPUs != 0 || spec.Memory != 0 {
			// With none asked for, the snapshot's own size.
			req.Config = config
		}
		created, err = api.CreateVmFromSnapshot(ctx, req)
		if isNotFound(err) {
			return machine.Instance{}, fmt.Errorf("boxd: committed VM %s: %w", spec.From.Name, machine.ErrNotFound)
		}
	} else {
		var image string
		image, err = d.baseImage(ctx, api)
		if err != nil {
			return machine.Instance{}, err
		}
		created, err = api.CreateVm(ctx, &boxdapi.CreateVmRequest{
			Name: name, ImageRef: image, RestartPolicy: "never", Config: config,
			Isolated: spec.Bare, Networks: spec.Networks,
		})
	}
	if err != nil {
		// boxd refuses a name a machine has, CreateVm with
		// FailedPrecondition and a restore with AlreadyExists (2026-11-16),
		// and FailedPrecondition is not that refusal alone, so the machine
		// of the name is looked for on those two only: a create whose answer
		// was lost is never reported as another's name.
		if code := status.Code(err); code == codes.FailedPrecondition || code == codes.AlreadyExists {
			if vm, lookErr := machineNamed(ctx, api, name); lookErr == nil && vm != nil {
				return machine.Instance{}, fmt.Errorf("boxd: a VM named %q: %w", spec.Name, machine.ErrExists)
			}
		}
		if limit, full := d.atLimit(ctx); full {
			return machine.Instance{}, fmt.Errorf("boxd: org %s has %d machines, its limit, stopped ones and every member's counted; destroy one, or have the org's owner ask boxd for more: %w (create %s: %w)",
				limit.GetOrgName(), limit.GetVmCount(), machine.ErrCapacity, name, err)
		}
		return machine.Instance{}, fmt.Errorf("boxd: create %s: %w", name, err)
	}
	vm := created.GetVmId()
	fresh := newlyMade{deadline: time.Now().Add(appearLimit)}
	if spec.Egress != nil {
		// Neither create takes an allowlist, and the machine runs until it
		// is set, so it is set before anything else.
		err = fresh.step(ctx, func() error { return fence(ctx, api, vm, *spec.Egress) })
	}
	if err == nil && restore && len(spec.Networks) > 0 {
		// A restore takes no networks of its own. It is isolated, so it has
		// no peers until they are set.
		err = fresh.step(ctx, func() error {
			if _, err := api.SetVmNetworks(ctx, &boxdapi.SetVmNetworksRequest{VmId: vm, Networks: spec.Networks}); err != nil {
				return fmt.Errorf("boxd: networks of %s: %w", name, err)
			}
			return nil
		})
	}
	if err == nil {
		err = fresh.step(ctx, func() error { return d.settle(ctx, api, vm) })
	}
	if err == nil {
		err = fresh.step(ctx, func() error {
			_, err := waitVM(ctx, api, vm, "running")
			return err
		})
	}
	if err == nil && spec.URLs.Ingress.Mode == machine.URLOff {
		err = fresh.step(ctx, func() error { return closeProxies(ctx, api, vm) })
	} else if err == nil {
		err = fresh.step(ctx, func() error {
			_, err := pinProxy(ctx, api, vm, guest.ServicePort)
			return err
		})
	}
	if err == nil {
		// A snapshot is of a running guest, and a new machine runs. Stop
		// it, so the first boot is a cold boot of its disk.
		err = fresh.step(ctx, func() error { return powerOff(ctx, api, vm) })
	}
	if err != nil {
		if undo := d.destroyVM(vm, fresh.seen); undo != nil {
			err = errors.Join(err, fmt.Errorf("boxd: undo the create of %s: %w", name, undo))
		}
		return machine.Instance{}, err
	}
	info, err := api.GetVm(ctx, &boxdapi.GetVmRequest{VmId: vm})
	if err != nil {
		return machine.Instance{}, fmt.Errorf("boxd: machine %s: %w", name, err)
	}
	return d.instance(info), nil
}

// newlyMade is a machine boxd has just made, which boxd may answer NotFound
// for a moment: right after CreateVm answered, SetAutoSuspendTimeout did
// (region fra, 2026-11-15), and boxd does not say that every call sees a new
// machine at once. Each step of its create is made again while it fails on a
// NotFound, until deadline.
type newlyMade struct {
	deadline time.Time
	// seen is whether a step has succeeded: boxd has shown the machine.
	seen bool
}

// step runs one step of the create, again after a NotFound until the
// deadline. Each step is one that can be made twice.
func (m *newlyMade) step(ctx context.Context, do func() error) error {
	for {
		err := do()
		if err == nil {
			m.seen = true
			return nil
		}
		if !isNotFound(err) || !time.Now().Before(m.deadline) {
			return err
		}
		select {
		case <-ctx.Done():
			return err
		case <-time.After(appearEvery):
		}
	}
}

// fence sets the machine's egress allowlist, and reads it back: a machine
// boxd does not report fenced as asked is not one to hand out. An empty
// Allow is boxd's ["none"], since boxd's empty allowlist is none at all.
func fence(ctx context.Context, api boxdapi.BoxdApiClient, vm string, egress machine.Egress) error {
	want := allowlist(egress)
	if _, err := api.SetVmEgressAllow(ctx, &boxdapi.SetVmEgressAllowRequest{Vm: vm, Entries: want}); err != nil {
		return fmt.Errorf("boxd: fence %s's egress to %q: %w", vm, want, err)
	}
	info, err := api.GetVm(ctx, &boxdapi.GetVmRequest{VmId: vm})
	if err != nil {
		return fmt.Errorf("boxd: read back %s's egress: %w", vm, err)
	}
	if got := info.GetEgressAllow(); !sameEntries(got, want) {
		return fmt.Errorf("boxd: %s's egress is %q, not the %q asked for", vm, got, want)
	}
	return nil
}

// allowlist is a fence as boxd's entries.
func allowlist(egress machine.Egress) []string {
	if len(egress.Allow) == 0 {
		return []string{"none"}
	}
	return egress.Allow
}

// egressOf is boxd's allowlist as a fence: nil for none, and an empty Allow
// for ["none"].
func egressOf(entries []string) *machine.Egress {
	switch {
	case len(entries) == 0:
		return nil
	case slices.Equal(entries, []string{"none"}):
		return &machine.Egress{}
	}
	return &machine.Egress{Allow: entries}
}

// sameEntries reports whether two allowlists hold the same entries, in any
// order.
func sameEntries(a, b []string) bool {
	a, b = slices.Clone(a), slices.Clone(b)
	slices.Sort(a)
	slices.Sort(b)
	return slices.Equal(slices.Compact(a), slices.Compact(b))
}

// settle turns off boxd's idle timers. A guest reached only through Exec looks
// idle to boxd, and a machine suspended or hibernated under a running instance
// would look to the engine like a hung guest.
func (d *Driver) settle(ctx context.Context, api boxdapi.BoxdApiClient, vm string) error {
	if _, err := api.SetAutoSuspendTimeout(ctx, &boxdapi.SetAutoSuspendTimeoutRequest{VmId: vm}); err != nil {
		return fmt.Errorf("boxd: disable auto-suspend on %s: %w", vm, err)
	}
	if _, err := api.SetAutoHibernateTimeout(ctx, &boxdapi.SetAutoHibernateTimeoutRequest{VmId: vm}); err != nil {
		return fmt.Errorf("boxd: disable auto-hibernate on %s: %w", vm, err)
	}
	return nil
}

// heldPaused reports a status of a machine boxd holds paused: suspended or
// in standby, its memory kept, or hibernated, its memory on disk. Start
// resumes or wakes it (untilRunning); nothing else does.
func heldPaused(status string) bool {
	return status == "suspended" || status == "standby" || status == "hibernated"
}

// Start is Boot, since boxd owns the machine, whatever process started it,
// but for a machine boxd holds paused, which it resumes rather than refuse.
func (d *Driver) Start(ctx context.Context, inst machine.Instance, opts machine.BootOptions) (machine.Machine, error) {
	return d.boot(ctx, inst, opts)
}

// Boot boots the stopped machine; one boxd holds paused is
// machine.ErrPaused.
func (d *Driver) Boot(ctx context.Context, inst machine.Instance, opts machine.BootOptions) (machine.Machine, error) {
	state, err := d.State(ctx, inst)
	if err != nil {
		return nil, err
	}
	if state == machine.StatePaused {
		return nil, fmt.Errorf("boxd: %s: %w", inst.Name, machine.ErrPaused)
	}
	return d.boot(ctx, inst, opts)
}

func (d *Driver) boot(ctx context.Context, inst machine.Instance, opts machine.BootOptions) (machine.Machine, error) {
	if opts.GUI {
		return nil, fmt.Errorf("boxd: a window on the guest's display: %w", machine.ErrUnsupported)
	}
	api, err := d.api.API(ctx)
	if err != nil {
		return nil, err
	}
	ref := vmRef{ID: inst.ID, Name: d.machineName(inst.Name)}
	// The expected machine is stopped, at the size it asks for, so StartVm
	// is asked first, with nothing read before it. boxd answers it OK for a
	// machine starting or running too, and resumes a suspended one; a
	// machine in any other state is seen, and booted, by the wait.
	if _, err := api.StartVm(ctx, &boxdapi.StartVmRequest{VmId: ref.ID}); err != nil {
		err = d.refuseCommitted(ctx, api, inst, fmt.Errorf("boxd: boot %s: %w", ref.Name, err))
		if errors.Is(err, machine.ErrCommitted) || isNotFound(err) {
			return nil, err
		}
	}
	// Running is boxd's word, and the boot's end: nothing in the guest is
	// waited for. boxd holds an Exec sent while the guest boots until it
	// answers, and one that does not reach it is machine.ErrUnreachable,
	// which the engine tries again.
	if err := untilRunning(ctx, api, ref, inst); err != nil {
		return nil, err
	}
	if opts.Console != nil {
		fmt.Fprintf(opts.Console, "boxd: %s (%s) is running\n", ref.Name, ref.ID)
	}
	return newVM(api, ref), nil
}

// untilRunning polls a machine Boot has asked StartVm of until boxd says it
// runs, and boots it from whatever else it finds: a stop still settling (a
// `disco-vm stop` a moment ago) is waited out and what it leaves started, a
// suspended machine resumed, a hibernated one woken. A machine that stops
// once it has been seen coming up ends the wait: a concurrent stop, or the
// guest's own power-off, is not undone.
//
// A machine at another size than inst asks (zero asks for none) is resized
// once, stopped first, since boxd resizes only a stopped machine: before its
// start where the wait finds it stopped, and otherwise, where this Boot's
// StartVm started it at its old size, by flushing its writes and stopping it
// mid-boot, then starting it again. One already running when the wait first
// sees it is left at its size, as a machine Boot did not start.
//
// boxd refuses a call that boots a machine (StartVm, ResumeVm, WakeVm) with
// FailedPrecondition when the machine has moved on since it was read, as a
// suspended machine StartVm is already resuming does ("VM is starting (must
// be suspended to resume)"). The wait reads it again rather than failing,
// and fails once boxd has refused it so in one state for raceGrace. A
// refused StopVm, the resize's, is no race: it fails the wait at once.
func untilRunning(ctx context.Context, api boxdapi.BoxdApiClient, ref vmRef, inst machine.Instance) error {
	ctx, cancel := context.WithTimeout(ctx, waitLimit)
	defer cancel()
	cpus, memory := uint32(inst.CPUs), inst.Memory
	last := ""
	// ranBefore: the wait first found the machine running, so Boot did not
	// start it. up: the wait has seen it come up since it was last started.
	// resized: its one resize has begun. stopped: the wait stopped it
	// itself, to resize it.
	first, ranBefore, up, resized, stopped := true, false, false, false, false
	// refusedIn is the state the machine read as when boxd refused,
	// FailedPrecondition, the call that boots it from there, and refusedAt
	// when it first did: the refusals are a race until raceGrace has passed
	// with the machine still reading so.
	refusedIn, refusedAt := "", time.Time{}
	for {
		info, err := api.GetVm(ctx, &boxdapi.GetVmRequest{VmId: ref.ID})
		if isNotFound(err) {
			return fmt.Errorf("boxd: machine %s is gone: %w", ref.Name, err)
		}
		if err == nil {
			last = info.GetStatus()
			if first {
				ranBefore, first = last == "running", false
			}
			if last != refusedIn {
				refusedIn, refusedAt = "", time.Time{}
			}
			booting := false
			resize := !resized && ((cpus != 0 && cpus != info.GetVcpu()) || (memory != 0 && memory != info.GetMemoryBytes()))
			switch last {
			case "running", "starting":
				if stopped {
					// The wait's own stop has not shown yet: boxd's stop
					// settles after StopVm answers.
					break
				}
				if resize && !ranBefore {
					// This Boot's StartVm started it at its old size.
					resized, stopped = true, true
					if err := syncDisks(ctx, api, ref.ID); err != nil {
						return fmt.Errorf("boxd: resize %s: %w", ref.Name, err)
					}
					_, err = api.StopVm(ctx, &boxdapi.StopVmRequest{VmId: ref.ID})
					break
				}
				if last == "running" {
					return nil
				}
				up = true
			case "stopping":
				if up && !stopped {
					return fmt.Errorf("boxd: machine %s stopped before it ran", ref.Name)
				}
			case "stopped":
				if up && !stopped {
					return fmt.Errorf("boxd: machine %s stopped before it ran", ref.Name)
				}
				if resize || stopped {
					resized = true
					if _, err := api.ResizeVm(ctx, &boxdapi.ResizeVmRequest{VmId: ref.ID, Vcpu: cpus, MemoryBytes: memory}); err != nil {
						return fmt.Errorf("boxd: resize %s: %w", ref.Name, err)
					}
				}
				up, stopped, booting = false, false, true
				_, err = api.StartVm(ctx, &boxdapi.StartVmRequest{VmId: ref.ID})
			case "standby", "suspended":
				booting = true
				_, err = api.ResumeVm(ctx, &boxdapi.ResumeVmRequest{VmId: ref.ID})
			case "hibernated":
				booting = true
				_, err = api.WakeVm(ctx, &boxdapi.WakeVmRequest{VmId: ref.ID})
			default:
				return fmt.Errorf("boxd: machine %s is %s", ref.Name, last)
			}
			switch {
			case err == nil:
			case booting && status.Code(err) == codes.FailedPrecondition && (refusedIn != last || time.Since(refusedAt) < raceGrace):
				// boxd moved the machine on between the read and the call
				// (its own resume of a machine StartVm resumed): a read to
				// come says where it went.
				if refusedIn != last {
					refusedIn, refusedAt = last, time.Now()
				}
			default:
				return fmt.Errorf("boxd: boot %s, which is %s: %w", ref.Name, last, err)
			}
		}
		select {
		case <-ctx.Done():
			return fmt.Errorf("boxd: machine %s is %q, waiting for it to run: %w", ref.Name, last, ctx.Err())
		case <-time.After(waitEvery):
		}
	}
}

// State is boxd's, as Attach reads it: a machine suspended, in standby, or
// hibernated is paused (the driver turns boxd's idle timers off, so only
// someone else's SuspendVm leaves one so); running or starting is running;
// anything else is stopped.
func (d *Driver) State(ctx context.Context, inst machine.Instance) (machine.State, error) {
	_, err := d.Attach(ctx, inst)
	switch {
	case err == nil:
		return machine.StateRunning, nil
	case errors.Is(err, machine.ErrPaused):
		return machine.StatePaused, nil
	case errors.Is(err, machine.ErrNotRunning):
		return machine.StateStopped, nil
	default:
		return "", err
	}
}

// Attach finds the instance's machine through the API, as any process can.
// Its state is boxd's: a guest that powered itself off from inside is left
// running by boxd, and reads as running until `disco-vm stop`.
func (d *Driver) Attach(ctx context.Context, inst machine.Instance) (machine.Machine, error) {
	api, err := d.api.API(ctx)
	if err != nil {
		return nil, err
	}
	ref := vmRef{ID: inst.ID, Name: d.machineName(inst.Name)}
	info, err := api.GetVm(ctx, &boxdapi.GetVmRequest{VmId: ref.ID})
	switch {
	case isNotFound(err):
		return nil, fmt.Errorf("boxd: machine %s is gone: %w", ref.Name, machine.ErrNotRunning)
	case err != nil:
		// A committed VM's ID is its snapshot's (a failed snapshot listed
		// in its place's too), which GetVm need not answer NotFound for: it
		// has no machine, so it is not running, whatever boxd says of the ID.
		if snap, snapErr := snapshotNamed(ctx, api, ref.Name); snapErr == nil && snap != nil && snap.GetSnapshotId() == inst.ID {
			return nil, fmt.Errorf("boxd: %s is a snapshot: %w", inst.Name, machine.ErrNotRunning)
		}
		return nil, fmt.Errorf("boxd: machine %s: %w", ref.Name, err)
	}
	switch state := info.GetStatus(); {
	case state == "running" || state == "starting":
		return newVM(api, ref), nil
	case heldPaused(state):
		return nil, fmt.Errorf("boxd: machine %s is %s: %w", ref.Name, state, machine.ErrPaused)
	default:
		return nil, fmt.Errorf("boxd: machine %s is %s: %w", ref.Name, state, machine.ErrNotRunning)
	}
}

// Upgrade is not offered: boxd attaches no disk but a machine's root, so
// there is no data volume for a new root to keep (ADR 26-11-03-782).
func (*Driver) Upgrade(context.Context, machine.Instance, *machine.Instance) (machine.Instance, error) {
	return machine.Instance{}, fmt.Errorf("boxd: an upgrade keeps a data volume, and boxd attaches none: %w", machine.ErrUnsupported)
}

// Commit boots the stopped VM from its disk, snapshots it under its name,
// and deletes the machine: a boxd snapshot can only be taken of a running
// machine. The snapshot's disk is the one the guest flushed at its last
// orderly shutdown.
func (d *Driver) Commit(ctx context.Context, inst machine.Instance) (machine.Instance, error) {
	api, err := d.api.API(ctx)
	if err != nil {
		return machine.Instance{}, err
	}
	ref := vmRef{ID: inst.ID, Name: d.machineName(inst.Name)}
	info, err := api.GetVm(ctx, &boxdapi.GetVmRequest{VmId: ref.ID})
	if err != nil {
		return machine.Instance{}, d.refuseCommitted(ctx, api, inst, fmt.Errorf("boxd: machine %s: %w", ref.Name, err))
	}
	if info.GetStatus() != "stopped" {
		return machine.Instance{}, fmt.Errorf("boxd: commit needs a stopped machine; %s is %s", ref.Name, info.GetStatus())
	}
	if _, err := api.StartVm(ctx, &boxdapi.StartVmRequest{VmId: ref.ID}); err != nil {
		return machine.Instance{}, fmt.Errorf("boxd: start %s: %w", ref.Name, err)
	}
	if _, err := waitVM(ctx, api, ref.ID, "running"); err != nil {
		return machine.Instance{}, err
	}
	if err := waitGuest(ctx, api, ref.ID); err != nil {
		return machine.Instance{}, err
	}
	created, err := api.CreateSnapshot(ctx, &boxdapi.CreateSnapshotRequest{Vm: ref.ID, Name: ref.Name})
	if err != nil {
		return machine.Instance{}, fmt.Errorf("boxd: snapshot %s: %w", ref.Name, err)
	}
	snap, err := waitSnapshot(ctx, api, ref.Name, created.GetVersion())
	if err != nil {
		return machine.Instance{}, err
	}
	// The snapshot is the VM now. A machine left by a delete that fails is
	// removed with it (Destroy).
	_ = d.destroyVM(ref.ID, true)
	return d.committed(snap), nil
}

// Destroy removes a VM's machine, or a committed VM's snapshot (or a failed
// one listed in its place) and any machine of its name a Commit left. A
// machine restored from the snapshot has a disk of its own, and outlives it.
//
// A machine's Destroy takes the snapshot of its name with it unless that was
// ready before the machine was destroyed: that one is a Commit that
// finished, the committed VM, which stays, even for a caller still holding
// the machine. Anything else is the machine's own Commit in flight, which
// nothing lists: a pending or failed snapshot is deleted first, since boxd
// makes a pending one ready once its machine is destroyed, and one that
// appears while the machine is destroyed is deleted after. A Commit racing
// its own VM's Destroy fails, or, in the moment between its snapshot's
// ready and the delete, reports a VM the Destroy has removed.
func (d *Driver) Destroy(ctx context.Context, inst machine.Instance) error {
	api, err := d.api.API(ctx)
	if err != nil {
		return err
	}
	name := d.machineName(inst.Name)
	before, err := snapshotNamed(ctx, api, name)
	if err != nil {
		return err
	}
	if before != nil && before.GetSnapshotId() == inst.ID {
		listed, err := api.ListVms(ctx, &boxdapi.ListVmsRequest{})
		if err != nil {
			return fmt.Errorf("boxd: list machines: %w", err)
		}
		for _, vm := range listed.GetVms() {
			if vm.GetName() != name {
				continue
			}
			if _, err := api.DestroyVm(ctx, &boxdapi.DestroyVmRequest{VmId: vm.GetVmId()}); err != nil && !isNotFound(err) {
				return fmt.Errorf("boxd: destroy %s: %w", name, err)
			}
		}
		return deleteSnapshot(ctx, api, name)
	}
	finished := before.GetStatus() == "ready"
	if before != nil && !finished {
		if err := deleteSnapshot(ctx, api, name); err != nil {
			return err
		}
	}
	if _, err := api.DestroyVm(ctx, &boxdapi.DestroyVmRequest{VmId: inst.ID}); err != nil && !isNotFound(err) {
		return fmt.Errorf("boxd: destroy %s: %w", name, err)
	}
	if finished {
		return nil
	}
	after, err := snapshotNamed(ctx, api, name)
	if err != nil || after == nil {
		return err
	}
	return deleteSnapshot(ctx, api, name)
}

// deleteSnapshot deletes the snapshot of a name; one already gone is not an
// error.
func deleteSnapshot(ctx context.Context, api boxdapi.BoxdApiClient, name string) error {
	if _, err := api.DeleteSnapshot(ctx, &boxdapi.DeleteSnapshotRequest{Name: name}); err != nil && !isNotFound(err) {
		return fmt.Errorf("boxd: delete snapshot %s: %w", name, err)
	}
	return nil
}

// waitSnapshot waits until a snapshot can be restored. One that is gone once
// boxd has answered for it was deleted (a Destroy of its machine), and the
// wait fails.
func waitSnapshot(ctx context.Context, api boxdapi.BoxdApiClient, name string, version uint64) (*boxdapi.SnapshotInfo, error) {
	ctx, cancel := context.WithTimeout(ctx, waitLimit)
	defer cancel()
	seen := false
	for {
		got, err := api.GetSnapshot(ctx, &boxdapi.GetSnapshotRequest{Name: name})
		switch {
		case isNotFound(err) && seen:
			return nil, fmt.Errorf("boxd: snapshot %s was deleted", name)
		case err == nil:
			seen = true
		case !isNotFound(err):
			return nil, fmt.Errorf("boxd: snapshot %s: %w", name, err)
		}
		info := got.GetSnapshot()
		switch {
		case info.GetStatus() == "failed":
			return nil, fmt.Errorf("boxd: snapshot %s failed", name)
		case info.GetStatus() == "ready" && info.GetVersion() >= version:
			return info, nil
		}
		select {
		case <-ctx.Done():
			return nil, fmt.Errorf("boxd: snapshot %s is still %q: %w", name, info.GetStatus(), ctx.Err())
		case <-time.After(time.Second):
		}
	}
}

// powerOff stops a new machine in its first boot: once systemd has finished
// starting it (waitGuest), its writes flushed (syncDisks), then StopVm, and
// the wait until boxd says it stopped, so the disk a VM's first start boots
// is one its first boot finished writing. A guest that cannot be asked to
// flush is still stopped, so it costs nothing, but powerOff fails: a disk cut
// off mid-write is not one to boot clones from, which is the rule a snapshot
// follows too.
func powerOff(ctx context.Context, api boxdapi.BoxdApiClient, vm string) error {
	ready, cancel := context.WithTimeout(ctx, 2*time.Minute)
	flushed := waitGuest(ready, api, vm)
	cancel()
	if flushed != nil {
		flushed = fmt.Errorf("boxd: flush %s: %w", vm, flushed)
	} else {
		flushed = syncDisks(ctx, api, vm)
	}
	if _, err := api.StopVm(ctx, &boxdapi.StopVmRequest{VmId: vm}); err != nil && !isNotFound(err) {
		return errors.Join(flushed, fmt.Errorf("boxd: stop %s: %w", vm, err))
	}
	if _, err := waitVM(ctx, api, vm, "stopped"); err != nil {
		return errors.Join(flushed, err)
	}
	return flushed
}

// syncDisks flushes the guest's writes to its disk, once boxd's Exec reaches
// it. boxd does not say whether StopVm shuts a guest down in order or cuts
// its power, so every stop that keeps the disk (Shutdown, a cold clone's)
// flushes it first. It waits for nothing else in the guest: a stop just after
// a start (which returns once the machine runs) flushes what the boot has
// written so far.
func syncDisks(ctx context.Context, api boxdapi.BoxdApiClient, vm string) error {
	ready, cancel := context.WithTimeout(ctx, 2*time.Minute)
	defer cancel()
	err := machine.UntilReachable(ready, func() error {
		return runAsRoot(ready, api, vm, machine.ExecRequest{Argv: []string{"sync"}})
	})
	if err != nil {
		return fmt.Errorf("boxd: flush %s: %w", vm, err)
	}
	return nil
}

// destroyVM destroys a machine the driver made and will not hand out, and
// fails, naming it, if the machine may be left. boxd's NotFound is not proof
// that a machine is gone, since boxd has answered it for a machine it had just
// made: a machine DestroyVm does not find is gone only once ListVms does not
// list it either (or lists it destroying), and a machine no call has found yet
// (seen false) is waited for until appearLimit.
func (d *Driver) destroyVM(vm string, seen bool) error {
	ctx, cancel := context.WithTimeout(context.Background(), appearLimit)
	defer cancel()
	api, err := d.api.API(ctx)
	if err != nil {
		return fmt.Errorf("boxd: machine %s is left: %w", vm, err)
	}
	for {
		_, err := api.DestroyVm(ctx, &boxdapi.DestroyVmRequest{VmId: vm})
		switch {
		case err == nil:
			return nil
		case !isNotFound(err):
			return fmt.Errorf("boxd: machine %s is left: destroy: %w", vm, err)
		}
		state, err := listedState(ctx, api, vm)
		switch {
		case err != nil:
			return fmt.Errorf("boxd: machine %s may be left: %w", vm, err)
		case state == "destroying", state == "" && seen:
			return nil
		}
		select {
		case <-ctx.Done():
			return fmt.Errorf("boxd: machine %s may be left: boxd answers NotFound to its destroy, and lists it %q: %w", vm, state, ctx.Err())
		case <-time.After(appearEvery):
		}
	}
}

// listedState is the state ListVms lists a machine in, or "" for none.
func listedState(ctx context.Context, api boxdapi.BoxdApiClient, vm string) (string, error) {
	listed, err := api.ListVms(ctx, &boxdapi.ListVmsRequest{})
	if err != nil {
		return "", fmt.Errorf("boxd: list machines: %w", err)
	}
	for _, v := range listed.GetVms() {
		if v.GetVmId() == vm {
			return v.GetStatus(), nil
		}
	}
	return "", nil
}

// waitVM polls until the machine reaches one of the given states, and fails
// on a state it cannot leave.
func waitVM(ctx context.Context, api boxdapi.BoxdApiClient, vm string, want ...string) (string, error) {
	ctx, cancel := context.WithTimeout(ctx, waitLimit)
	defer cancel()
	last := ""
	for {
		info, err := api.GetVm(ctx, &boxdapi.GetVmRequest{VmId: vm})
		switch {
		case isNotFound(err):
			return "", fmt.Errorf("boxd: machine %s is gone: %w", vm, err)
		case err == nil:
			last = info.GetStatus()
			if slices.Contains(want, last) {
				return last, nil
			}
			if last == "failed" || last == "destroying" {
				return last, fmt.Errorf("boxd: machine %s is %s", vm, last)
			}
		}
		select {
		case <-ctx.Done():
			return last, fmt.Errorf("boxd: machine %s is %q, waiting for %v: %w", vm, last, want, ctx.Err())
		case <-time.After(waitEvery):
		}
	}
}

// run runs a shell command in the machine, as boxd's own user, and returns its
// combined output.
func run(ctx context.Context, api boxdapi.BoxdApiClient, vm, command string) (string, int32, error) {
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	stream, err := api.Exec(ctx)
	if err != nil {
		return "", -1, err
	}
	if err := stream.Send(&boxdapi.ExecChunk{VmId: vm, Command: command}); err != nil {
		return "", -1, err
	}
	if err := stream.CloseSend(); err != nil {
		return "", -1, err
	}
	var out bytes.Buffer
	var code int32
	for {
		msg, err := stream.Recv()
		if errors.Is(err, io.EOF) {
			return out.String(), code, nil
		}
		if err != nil {
			return out.String(), -1, err
		}
		out.Write(msg.GetData())
		if msg.GetExitCode() != 0 {
			code = msg.GetExitCode()
		}
	}
}
