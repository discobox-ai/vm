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
// The instance's service is reached at its public URL, and no guest port is
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

// Driver is the boxd driver.
type Driver struct {
	api  *client
	pool string
	// base is the image a fresh VM is made from; empty is boxd's default
	// image (GetConfig).
	base string
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

func (*Driver) Capabilities() machine.Capabilities {
	return machine.Capabilities{
		GuestOS:   []machine.OS{machine.Linux},
		Commit:    true,
		Keys:      keyCapabilities,
		Isolation: true,
		Egress:    []machine.EgressFence{machine.EgressNone, machine.EgressAllow},
		// The default proxy, removed for a VM whose public URL is asked
		// off; the private URL is the direct one on networks, else the
		// public one.
		URLs: machine.URLCapabilities{Private: machine.URLAlways, Public: machine.URLOptional},
	}
}

// Check asks boxd who the key is, at the home endpoint. With a region it
// also finds the region's endpoint, which needs the region to be one the
// key's org has enabled, and lists the machines there, so an endpoint that
// does not answer, or refuses the key, fails here rather than at the first
// create.
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
	if d.api.region == "" {
		return nil
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
// service port, or every proxy removed if its public URL is asked off; then
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
	if spec.URLs.Private == (machine.URLRequest{Mode: machine.URLOn, Strict: true}) && len(spec.Networks) == 0 && spec.URLs.Public.Mode == machine.URLOff {
		// Peers reach a machine on no networks at its public URL alone.
		return machine.Instance{}, fmt.Errorf("boxd: a private URL on no networks, with the public URL off: %w", machine.ErrUnsupported)
	}
	if spec.Data != nil || spec.Cache != nil {
		return machine.Instance{}, fmt.Errorf("boxd: a data or cache volume: %w", machine.ErrUnsupported)
	}
	restore := spec.From != nil
	if restore && len(spec.Networks) > 0 && !spec.Isolated {
		// A restore takes no networks: they are set once it is made, and a
		// machine that is not isolated is on the default network until then.
		return machine.Instance{}, fmt.Errorf("boxd: networks on a VM created from a committed one need it isolated, or it peers on the default network until they are set: %w", machine.ErrUnsupported)
	}
	api, err := d.api.API(ctx)
	if err != nil {
		return machine.Instance{}, err
	}
	if _, err := d.Instance(ctx, spec.Name); err == nil {
		return machine.Instance{}, fmt.Errorf("boxd: a VM named %q: %w", spec.Name, machine.ErrExists)
	} else if !errors.Is(err, machine.ErrNotFound) {
		return machine.Instance{}, err
	}
	config := &boxdapi.VmConfig{Vcpu: uint32(spec.CPUs), MemoryBytes: spec.Memory}
	var created *boxdapi.CreateVmResponse
	if restore {
		// boxd restores only a snapshot: a From that is a machine is no
		// committed VM boxd has.
		snap, err := d.snapshotOf(ctx, api, *spec.From)
		if err != nil {
			return machine.Instance{}, err
		}
		if snap == nil {
			return machine.Instance{}, fmt.Errorf("boxd: no committed VM %s, a snapshot of that name and ID; commit it first: %w", spec.From.Name, machine.ErrNotFound)
		}
		// By name, as boxd's own restores are: a committed VM's name is
		// its snapshot's, and no second VM of the pool takes it.
		req := &boxdapi.CreateVmFromSnapshotRequest{Snapshot: d.machineName(spec.From.Name), Name: name, Isolated: spec.Isolated}
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
			Isolated: spec.Isolated, Networks: spec.Networks,
		})
	}
	if status.Code(err) == codes.AlreadyExists {
		return machine.Instance{}, fmt.Errorf("boxd: a VM named %q: %w", spec.Name, machine.ErrExists)
	}
	if err != nil {
		return machine.Instance{}, fmt.Errorf("boxd: create %s: %w", name, err)
	}
	vm := created.GetVmId()
	if spec.Egress != nil {
		// Neither create takes an allowlist, and the machine runs until it
		// is set, so it is set before anything else.
		err = fence(ctx, api, vm, *spec.Egress)
	}
	if err == nil && restore && len(spec.Networks) > 0 {
		// A restore takes no networks of its own. It is isolated, so it has
		// no peers until they are set.
		_, err = api.SetVmNetworks(ctx, &boxdapi.SetVmNetworksRequest{VmId: vm, Networks: spec.Networks})
		if err != nil {
			err = fmt.Errorf("boxd: networks of %s: %w", name, err)
		}
	}
	if err == nil {
		err = d.settle(ctx, api, vm)
	}
	if err == nil {
		_, err = waitVM(ctx, api, vm, "running")
	}
	if err == nil && spec.URLs.Public.Mode == machine.URLOff {
		err = dropProxies(ctx, api, vm)
	} else if err == nil {
		_, err = pinProxy(ctx, api, vm, guest.ServicePort)
	}
	if err == nil {
		// A snapshot is of a running guest, and a new machine runs. Stop
		// it, so the first boot is a cold boot of its disk.
		err = powerOff(ctx, api, vm)
	}
	if err != nil {
		d.destroyVM(vm)
		return machine.Instance{}, err
	}
	info, err := api.GetVm(ctx, &boxdapi.GetVmRequest{VmId: vm})
	if err != nil {
		return machine.Instance{}, fmt.Errorf("boxd: machine %s: %w", name, err)
	}
	return d.instance(info), nil
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

// Start is Boot: boxd owns the machine, whatever process started it.
func (d *Driver) Start(ctx context.Context, inst machine.Instance, opts machine.BootOptions) (machine.Machine, error) {
	return d.Boot(ctx, inst, opts)
}

func (d *Driver) Boot(ctx context.Context, inst machine.Instance, opts machine.BootOptions) (machine.Machine, error) {
	if opts.GUI {
		return nil, fmt.Errorf("boxd: a window on the guest's display: %w", machine.ErrUnsupported)
	}
	api, err := d.api.API(ctx)
	if err != nil {
		return nil, err
	}
	ref := vmRef{ID: inst.ID, Name: d.machineName(inst.Name)}
	info, err := api.GetVm(ctx, &boxdapi.GetVmRequest{VmId: ref.ID})
	if err != nil {
		return nil, d.refuseCommitted(ctx, api, inst, fmt.Errorf("boxd: machine %s: %w", ref.Name, err))
	}
	if info.GetStatus() == "stopping" {
		// A stop is still settling: a `disco-vm stop` a moment ago. Start
		// what it leaves.
		if _, err := waitVM(ctx, api, ref.ID, "stopped"); err != nil {
			return nil, err
		}
		if info, err = api.GetVm(ctx, &boxdapi.GetVmRequest{VmId: ref.ID}); err != nil {
			return nil, fmt.Errorf("boxd: machine %s: %w", ref.Name, err)
		}
	}
	switch state := info.GetStatus(); state {
	case "stopped":
		cpus, memory := uint32(inst.CPUs), inst.Memory
		if (cpus != 0 && cpus != info.GetVcpu()) || (memory != 0 && memory != info.GetMemoryBytes()) {
			// A stopped machine takes the new size at its next start.
			if _, err := api.ResizeVm(ctx, &boxdapi.ResizeVmRequest{VmId: ref.ID, Vcpu: cpus, MemoryBytes: memory}); err != nil {
				return nil, fmt.Errorf("boxd: resize %s: %w", ref.Name, err)
			}
		}
		_, err = api.StartVm(ctx, &boxdapi.StartVmRequest{VmId: ref.ID})
	case "standby", "suspended":
		_, err = api.ResumeVm(ctx, &boxdapi.ResumeVmRequest{VmId: ref.ID})
	case "hibernated":
		_, err = api.WakeVm(ctx, &boxdapi.WakeVmRequest{VmId: ref.ID})
	case "running", "starting":
	default:
		return nil, fmt.Errorf("boxd: machine %s is %s", ref.Name, state)
	}
	if err != nil {
		return nil, fmt.Errorf("boxd: boot %s: %w", ref.Name, err)
	}
	if _, err := waitVM(ctx, api, ref.ID, "running"); err != nil {
		return nil, err
	}
	m := newVM(api, ref)
	watch, stopWatching := context.WithCancel(ctx)
	defer stopWatching()
	if err := waitGuest(ctx, api, ref.ID, stopped(watch, api, ref.ID)); err != nil {
		_ = m.Kill(context.Background())
		return nil, err
	}
	if opts.Console != nil {
		fmt.Fprintf(opts.Console, "boxd: %s (%s) is running\n", ref.Name, ref.ID)
	}
	return m, nil
}

// stopped is closed if the machine stops, or is gone, before ctx ends. It is
// not the machine's Done, whose watcher would outlive Boot in a program that
// embeds the engine: Boot ends ctx when it returns.
func stopped(ctx context.Context, api boxdapi.BoxdApiClient, vm string) <-chan struct{} {
	ch := make(chan struct{})
	go func() {
		for {
			select {
			case <-ctx.Done():
				return
			case <-time.After(pollEvery):
			}
			info, err := api.GetVm(ctx, &boxdapi.GetVmRequest{VmId: vm})
			if isNotFound(err) || (err == nil && info.GetStatus() != "running" && info.GetStatus() != "starting") {
				close(ch)
				return
			}
		}
	}()
	return ch
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
		// A committed VM's ID is its snapshot's, which GetVm need not
		// answer NotFound for: it has no machine, so it is not running,
		// whatever boxd says of the ID.
		if snap, snapErr := d.snapshotOf(ctx, api, inst); snapErr == nil && snap != nil {
			return nil, fmt.Errorf("boxd: %s is a committed VM: %w", inst.Name, machine.ErrNotRunning)
		}
		return nil, fmt.Errorf("boxd: machine %s: %w", ref.Name, err)
	}
	switch state := info.GetStatus(); state {
	case "running", "starting":
		return newVM(api, ref), nil
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
	if err := waitGuest(ctx, api, ref.ID, nil); err != nil {
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
	d.destroyVM(ref.ID)
	return d.committed(snap), nil
}

// Destroy removes a VM's machine, or a committed VM's snapshot and any
// machine of its name a Commit left. A machine restored from the snapshot
// has a disk of its own, and outlives it.
func (d *Driver) Destroy(ctx context.Context, inst machine.Instance) error {
	api, err := d.api.API(ctx)
	if err != nil {
		return err
	}
	name := d.machineName(inst.Name)
	snap, err := d.snapshotOf(ctx, api, inst)
	if err != nil {
		return err
	}
	if snap != nil {
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
		if _, err := api.DeleteSnapshot(ctx, &boxdapi.DeleteSnapshotRequest{Name: name}); err != nil && !isNotFound(err) {
			return fmt.Errorf("boxd: delete snapshot %s: %w", name, err)
		}
		return nil
	}
	if _, err := api.DestroyVm(ctx, &boxdapi.DestroyVmRequest{VmId: inst.ID}); err != nil && !isNotFound(err) {
		return fmt.Errorf("boxd: destroy %s: %w", name, err)
	}
	return nil
}

// waitSnapshot waits until a snapshot can be restored.
func waitSnapshot(ctx context.Context, api boxdapi.BoxdApiClient, name string, version uint64) (*boxdapi.SnapshotInfo, error) {
	ctx, cancel := context.WithTimeout(ctx, waitLimit)
	defer cancel()
	for {
		got, err := api.GetSnapshot(ctx, &boxdapi.GetSnapshotRequest{Name: name})
		if err != nil && !isNotFound(err) {
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

// powerOff stops a machine: the guest's writes flushed (syncDisks), then
// StopVm, and the wait until boxd says it stopped. A guest that cannot be
// asked to flush is still stopped, so it costs nothing, but powerOff fails: a
// disk cut off mid-write is not one to boot clones from, which is the rule a
// snapshot follows too.
func powerOff(ctx context.Context, api boxdapi.BoxdApiClient, vm string) error {
	flushed := syncDisks(ctx, api, vm)
	if _, err := api.StopVm(ctx, &boxdapi.StopVmRequest{VmId: vm}); err != nil && !isNotFound(err) {
		return errors.Join(flushed, fmt.Errorf("boxd: stop %s: %w", vm, err))
	}
	if _, err := waitVM(ctx, api, vm, "stopped"); err != nil {
		return errors.Join(flushed, err)
	}
	return flushed
}

// syncDisks flushes the guest's writes to its disk, once it answers. boxd does
// not say whether StopVm shuts a guest down in order or cuts its power, so
// every stop that keeps the disk (Shutdown, a cold clone's)
// flushes it first.
func syncDisks(ctx context.Context, api boxdapi.BoxdApiClient, vm string) error {
	ready, cancel := context.WithTimeout(ctx, 2*time.Minute)
	defer cancel()
	if err := waitGuest(ready, api, vm, nil); err != nil {
		return fmt.Errorf("boxd: flush %s: %w", vm, err)
	}
	if err := runAsRoot(ready, api, vm, machine.ExecRequest{Argv: []string{"sync"}}); err != nil {
		return fmt.Errorf("boxd: flush %s: %w", vm, err)
	}
	return nil
}

// destroyVM is best-effort cleanup of a machine the driver made and will not
// hand out.
func (d *Driver) destroyVM(vm string) {
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	api, err := d.api.API(ctx)
	if err != nil {
		return
	}
	_, _ = api.DestroyVm(ctx, &boxdapi.DestroyVmRequest{VmId: vm})
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
			return "", fmt.Errorf("boxd: machine %s is gone", vm)
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
		case <-time.After(time.Second):
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
