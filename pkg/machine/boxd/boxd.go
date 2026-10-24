// Package boxd is a driver for boxd (https://boxd.sh): Linux microVMs in the
// cloud, reached through boxd's public gRPC API. It runs on every host OS,
// because the hypervisor is not on the host.
//
// boxd has no disk images to hand out, only machines and snapshots of them,
// and keeps no labels on either, so the driver maps the seam onto their names
// and keeps nothing on the host (ADR 26-10-23-417):
//
//   - An image is a boxd snapshot (memory and disk) of the guest booted from
//     the committed disk, named dvm-<project>--<image id>. boxd keeps no
//     comment for it, and a snapshot is whole: no image needs another.
//   - A tag is a second snapshot of its image's, named
//     dvm-<project>--<image id>--t-<ref>, with the ref's punctuation escaped.
//   - An instance is a boxd machine named dvm-<project>--<name>, restored from
//     its image's snapshot or made from a base, and powered off in order once
//     made, so its first boot is a cold boot of its disk.
//
// There is no install: boxd's images and snapshots are bases.
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

	"github.com/discobox-ai/vm/internal/fsutil"
	"github.com/discobox-ai/vm/pkg/guest"
	"github.com/discobox-ai/vm/pkg/machine"
	"github.com/discobox-ai/vm/pkg/machine/boxd/internal/boxdapi"
)

func init() {
	machine.Register("boxd", func(cfg machine.Config) (machine.Driver, error) { return New(cfg.Project), nil })
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
	api     *client
	project string
}

var _ machine.Driver = (*Driver)(nil)

// New returns a driver that authenticates with $BOXD_API_KEY, for a project
// (machine.DefaultProject when empty). It dials nothing until it is used.
func New(project string) *Driver {
	if project == "" {
		project = machine.DefaultProject
	}
	return &Driver{api: newClient(), project: project}
}

func (*Driver) Name() string { return "boxd" }

func (*Driver) Capabilities() machine.Capabilities {
	return machine.Capabilities{GuestOS: []machine.OS{machine.Linux}}
}

func (d *Driver) Check(ctx context.Context) error {
	api, err := d.api.API()
	if err != nil {
		return err
	}
	ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	if _, err := api.Whoami(ctx, &boxdapi.WhoamiRequest{}); err != nil {
		return fmt.Errorf("boxd: %s: %w", d.api.addr, err)
	}
	return nil
}

// vmRef is a machine the driver reaches.
type vmRef struct {
	ID   string `json:"id"`
	Name string `json:"name"`
}

// prefix starts the name of everything the driver makes for its project.
func (d *Driver) prefix() string { return machine.ResourcePrefix(namePrefix, d.project) }

// imageName is an image's snapshot.
func (d *Driver) imageName(id string) string { return d.prefix() + id }

// transientName is a machine the driver makes for a moment of its own (to save
// a tag's snapshot), named as an instance of the project's, `<what>-<random>`,
// so that one a process killed meanwhile leaves, which boxd bills, is listed,
// and `rm` removes it.
func (d *Driver) transientName(what string) string {
	return d.prefix() + what + "-" + fsutil.RandomHex(3)
}

// machineName is an instance's machine.
func (d *Driver) machineName(name string) string { return d.prefix() + name }

// Install is not offered: boxd's images and snapshots are bases instead.
func (*Driver) Install(context.Context, machine.InstallSpec, machine.Image) (machine.Image, error) {
	return machine.Image{}, fmt.Errorf("boxd: run one of boxd's images or snapshots (disco-vm images): %w", machine.ErrUnsupported)
}

// Create makes the instance's machine: restored from its image's snapshot, or
// made from its base, at the size it asks for, with its proxy pinned to the
// service port; then powered off in order, so its first boot is a cold boot.
func (d *Driver) Create(ctx context.Context, spec machine.InstanceSpec) (machine.Instance, error) {
	if err := machine.ValidateName(spec.Name); err != nil {
		return machine.Instance{}, err
	}
	name := d.machineName(spec.Name)
	if len(name) > maxMachineName {
		return machine.Instance{}, fmt.Errorf("boxd: instance name %q is too long: its machine %s must be at most %d characters", spec.Name, name, maxMachineName)
	}
	if spec.Base == nil && spec.ImageID == "" {
		return machine.Instance{}, errors.New("boxd: instance has no image")
	}
	if len(spec.Forwards) > 0 {
		return machine.Instance{}, fmt.Errorf("boxd: forwards: %w", machine.ErrUnsupported)
	}
	api, err := d.api.API()
	if err != nil {
		return machine.Instance{}, err
	}
	if _, err := d.Instance(ctx, spec.Name); err == nil {
		return machine.Instance{}, fmt.Errorf("boxd: an instance named %q: %w", spec.Name, machine.ErrExists)
	} else if !errors.Is(err, machine.ErrNotFound) {
		return machine.Instance{}, err
	}
	config := &boxdapi.VmConfig{Vcpu: uint32(spec.CPUs), MemoryBytes: spec.Memory}
	var created *boxdapi.CreateVmResponse
	switch {
	case spec.Base == nil:
		req := &boxdapi.CreateVmFromSnapshotRequest{Snapshot: d.imageName(spec.ImageID), Name: name}
		if spec.CPUs != 0 || spec.Memory != 0 {
			req.Config = config
		}
		created, err = api.CreateVmFromSnapshot(ctx, req)
	case spec.Base.Options["snapshot"] != "":
		req := &boxdapi.CreateVmFromSnapshotRequest{Snapshot: spec.Base.Options["snapshot"], Name: name}
		if spec.CPUs != 0 || spec.Memory != 0 {
			// With none asked for, the snapshot's own size.
			req.Config = config
		}
		created, err = api.CreateVmFromSnapshot(ctx, req)
	default:
		created, err = api.CreateVm(ctx, &boxdapi.CreateVmRequest{
			Name: name, ImageRef: spec.Base.Options["image"], RestartPolicy: "never", Config: config,
		})
	}
	if status.Code(err) == codes.AlreadyExists {
		return machine.Instance{}, fmt.Errorf("boxd: an instance named %q: %w", spec.Name, machine.ErrExists)
	}
	if err != nil {
		return machine.Instance{}, fmt.Errorf("boxd: create %s: %w", name, err)
	}
	vm := created.GetVmId()
	err = d.settle(ctx, api, vm)
	if err == nil {
		_, err = waitVM(ctx, api, vm, "running")
	}
	if err == nil {
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
	inst := d.instance(info)
	inst.Image, inst.ImageID, inst.Base = spec.Image, spec.ImageID, spec.Base
	return inst, nil
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
	api, err := d.api.API()
	if err != nil {
		return nil, err
	}
	ref := vmRef{ID: inst.ID, Name: d.machineName(inst.Name)}
	info, err := api.GetVm(ctx, &boxdapi.GetVmRequest{VmId: ref.ID})
	if err != nil {
		return nil, fmt.Errorf("boxd: machine %s: %w", ref.Name, err)
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
	api, err := d.api.API()
	if err != nil {
		return nil, err
	}
	ref := vmRef{ID: inst.ID, Name: d.machineName(inst.Name)}
	info, err := api.GetVm(ctx, &boxdapi.GetVmRequest{VmId: ref.ID})
	switch {
	case isNotFound(err):
		return nil, fmt.Errorf("boxd: machine %s is gone: %w", ref.Name, machine.ErrNotRunning)
	case err != nil:
		return nil, fmt.Errorf("boxd: machine %s: %w", ref.Name, err)
	}
	switch state := info.GetStatus(); state {
	case "running", "starting":
		return newVM(api, ref), nil
	default:
		return nil, fmt.Errorf("boxd: machine %s is %s: %w", ref.Name, state, machine.ErrNotRunning)
	}
}

// Commit boots the stopped instance from its committed disk, snapshots it as
// the image, and destroys the machine: a boxd snapshot can only be taken of a
// running machine.
func (d *Driver) Commit(ctx context.Context, inst machine.Instance, dst machine.Image) (machine.Image, error) {
	api, err := d.api.API()
	if err != nil {
		return machine.Image{}, err
	}
	ref := vmRef{ID: inst.ID, Name: d.machineName(inst.Name)}
	info, err := api.GetVm(ctx, &boxdapi.GetVmRequest{VmId: ref.ID})
	if err != nil {
		return machine.Image{}, fmt.Errorf("boxd: machine %s: %w", ref.Name, err)
	}
	if info.GetStatus() != "stopped" {
		return machine.Image{}, fmt.Errorf("boxd: commit needs a stopped machine; %s is %s", ref.Name, info.GetStatus())
	}
	if _, err := api.StartVm(ctx, &boxdapi.StartVmRequest{VmId: ref.ID}); err != nil {
		return machine.Image{}, fmt.Errorf("boxd: start %s: %w", ref.Name, err)
	}
	if _, err := waitVM(ctx, api, ref.ID, "running"); err != nil {
		return machine.Image{}, err
	}
	if err := waitGuest(ctx, api, ref.ID, nil); err != nil {
		return machine.Image{}, err
	}
	img, err := d.snapshot(ctx, api, ref.ID, dst.ID)
	if err != nil {
		return machine.Image{}, err
	}
	d.destroyVM(ref.ID)
	return img, nil
}

func (d *Driver) Destroy(ctx context.Context, inst machine.Instance) error {
	api, err := d.api.API()
	if err != nil {
		return err
	}
	if _, err := api.DestroyVm(ctx, &boxdapi.DestroyVmRequest{VmId: inst.ID}); err != nil && !isNotFound(err) {
		return fmt.Errorf("boxd: destroy %s: %w", d.machineName(inst.Name), err)
	}
	return nil
}

// DeleteImage deletes the image's snapshot. No other image needs it: a
// machine restored from a snapshot has a disk of its own.
func (d *Driver) DeleteImage(ctx context.Context, id string) error {
	return d.deleteSnapshot(ctx, d.imageName(id))
}

// snapshot captures a running machine as the image id, and waits until the
// snapshot can be restored.
func (d *Driver) snapshot(ctx context.Context, api boxdapi.BoxdApiClient, vm, id string) (machine.Image, error) {
	name := d.imageName(id)
	created, err := api.CreateSnapshot(ctx, &boxdapi.CreateSnapshotRequest{Vm: vm, Name: name})
	if err != nil {
		return machine.Image{}, fmt.Errorf("boxd: snapshot %s: %w", vm, err)
	}
	snap, err := waitSnapshot(ctx, api, name, created.GetVersion())
	if err != nil {
		return machine.Image{}, err
	}
	return machine.Image{ID: id, GuestOS: machine.Linux, Created: unixTime(snap.GetCreatedAt(), time.Millisecond)}, nil
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

func (d *Driver) deleteSnapshot(ctx context.Context, name string) error {
	api, err := d.api.API()
	if err != nil {
		return err
	}
	if _, err := api.DeleteSnapshot(ctx, &boxdapi.DeleteSnapshotRequest{Name: name}); err != nil && !isNotFound(err) {
		return fmt.Errorf("boxd: delete snapshot %s: %w", name, err)
	}
	return nil
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

// destroyVM is best-effort cleanup of a machine the driver made for itself.
func (d *Driver) destroyVM(vm string) {
	api, err := d.api.API()
	if err != nil {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
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
