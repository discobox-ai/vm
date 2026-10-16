// Package boxd is a driver for boxd (https://boxd.sh): Linux microVMs in the
// cloud, reached through boxd's public gRPC API. It runs on every host OS,
// because the hypervisor is not on the host.
//
// boxd has no disk images to hand out, only machines and snapshots of them, so
// the driver maps the seam onto those:
//
//   - A layer is a boxd snapshot (memory and disk) of the guest booted from the
//     committed disk, named in the layer's layer.json. Deleting the layer
//     deletes the snapshot.
//   - An instance is a boxd machine restored from its parent layer's snapshot,
//     named in the instance's vm.json. A cold clone is powered off in order
//     once restored, so its first boot is a cold boot of the layer's disk.
//
// There is no hypervisor socket to the guest, and no disco-vm agent in it.
// The guest's operations (machine.Guest) are boxd's authenticated Exec stream:
// a command through sudo, tar over its stdin and stdout for copies, and
// systemd's own power-off (guest.go). The engine reaches the instance's
// service at its public URL, and no guest port is dialed.
//
// A guest cannot power its boxd machine off. systemd shuts everything down and
// asks the kernel to power off, and the machine stays "running" with nothing in
// it. So Install adds a systemd shutdown hook, which records poweroff, halt, or
// reboot in a marker as the guest's last act, once its disks are synced. The
// driver watches for the marker and stops the machine through the API.
package boxd

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"time"

	"github.com/discobox-ai/vm/internal/fsutil"
	"github.com/discobox-ai/vm/pkg/machine"
	"github.com/discobox-ai/vm/pkg/machine/boxd/internal/boxdapi"
)

func init() {
	machine.Register("boxd", func() (machine.Driver, error) { return New(), nil })
}

const (
	// maxDisk is boxd's fixed disk: every machine has 100 GiB.
	maxDisk = 100 << 30

	layerFile = "layer.json"
	vmFile    = "vm.json"

	// waitLimit bounds each wait for a machine or snapshot to change state.
	waitLimit = 10 * time.Minute
)

// Driver is the boxd driver.
type Driver struct {
	api *client
}

var (
	_ machine.Driver   = (*Driver)(nil)
	_ machine.Attacher = (*Driver)(nil)
)

// New returns a driver that authenticates with $BOXD_API_KEY. It dials
// nothing until it is used.
func New() *Driver { return &Driver{api: newClient()} }

func (*Driver) Name() string { return "boxd" }

func (*Driver) Capabilities() machine.Capabilities {
	return machine.Capabilities{
		GuestOS: []machine.OS{machine.Linux},
		Remote:  true,
	}
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

// snapshotRef is a layer's layer.json.
type snapshotRef struct {
	Name    string `json:"name"`
	ID      string `json:"id"`
	Version uint64 `json:"version"`
	CPUs    uint32 `json:"cpus"`
	Memory  uint64 `json:"memory"`
}

// vmRef is an instance's vm.json.
type vmRef struct {
	ID   string `json:"id"`
	Name string `json:"name"`
	// Endpoint is the URL of the instance's service, set by Prepare.
	Endpoint string `json:"endpoint,omitempty"`
}

// Install creates a machine from one of boxd's own images and snapshots it as
// the base layer. It puts nothing of disco-vm's in the guest, so spec.Agent is
// not used.
func (d *Driver) Install(ctx context.Context, spec machine.InstallSpec, dst machine.Layer) error {
	if spec.GuestOS != machine.Linux {
		return fmt.Errorf("boxd: guests are linux, not %s", spec.GuestOS)
	}
	if spec.Media != "" && spec.Media != "latest" {
		return fmt.Errorf("boxd: install from media \"latest\"; pick a boxd image with options.image, not media %q", spec.Media)
	}
	for key := range spec.Options {
		if key != "image" {
			return fmt.Errorf("boxd: unknown install option %q (have: image)", key)
		}
	}
	if spec.DiskBytes > maxDisk {
		return fmt.Errorf("boxd: every machine has a 100 GiB disk; %d bytes were asked for", spec.DiskBytes)
	}
	api, err := d.api.API()
	if err != nil {
		return err
	}
	log := logger(spec.Log)

	created, err := api.CreateVm(ctx, &boxdapi.CreateVmRequest{
		Name:          vmName(dst.Project, "install"),
		ImageRef:      spec.Options["image"],
		RestartPolicy: "never",
		Config:        &boxdapi.VmConfig{Vcpu: uint32(spec.CPUs), MemoryBytes: spec.Memory},
	})
	if err != nil {
		return fmt.Errorf("boxd: create machine: %w", err)
	}
	vm := created.GetVmId()
	defer d.destroyVM(vm)
	log("boxd: created %s (%s)", created.GetName(), vm)
	if err := d.settle(ctx, api, vm); err != nil {
		return err
	}
	if _, err := waitVM(ctx, api, vm, "running"); err != nil {
		return err
	}
	if err := waitGuest(ctx, api, vm, nil); err != nil {
		return err
	}

	// The base layer is snapshotted running, and a cold clone boots its
	// disk: what the guest wrote must be on it.
	if err := syncDisks(ctx, api, vm); err != nil {
		return err
	}
	log("boxd: snapshotting the base layer")
	snap, err := d.snapshot(ctx, api, vm, dst)
	if err != nil {
		return err
	}
	return writeJSON(filepath.Join(dst.Dir, layerFile), snap)
}

func (d *Driver) Prepare(ctx context.Context, inst machine.InstanceSpec) error {
	if len(inst.Chain) == 0 && inst.Base == nil {
		return errors.New("boxd: instance has no image")
	}
	if err := os.MkdirAll(inst.Dir, 0o755); err != nil {
		return err
	}
	if inst.Base != nil {
		// Its machine is made at its first Boot, at the size that boot asks
		// for, so a first start is one boot.
		return nil
	}
	api, err := d.api.API()
	if err != nil {
		return err
	}
	var layer snapshotRef
	if err := readJSON(filepath.Join(inst.Parent().Dir, layerFile), &layer); err != nil {
		return fmt.Errorf("boxd: layer %s: %w", inst.Parent().ID, err)
	}
	vm, err := d.restore(ctx, api, inst, layer.Name)
	if err != nil {
		return err
	}
	if err := d.expose(ctx, api, inst); err != nil {
		return err
	}
	// The snapshot is of a running guest. Stop it, so the first boot is a
	// cold boot of the layer's disk.
	return powerOff(ctx, api, vm)
}

// create makes the machine of an instance of a base, from boxd's image or the
// snapshot, at the size opts asks for, and pins its proxy as Prepare does an
// instance of a layer's. vm.json is written as soon as the machine exists, so
// Destroy removes it even when a later step fails.
func (d *Driver) create(ctx context.Context, api boxdapi.BoxdApiClient, inst machine.InstanceSpec, opts machine.BootOptions) (vmRef, error) {
	config := &boxdapi.VmConfig{Vcpu: uint32(opts.CPUs), MemoryBytes: opts.Memory}
	name := vmName(inst.Project, inst.ID)
	var created *boxdapi.CreateVmResponse
	var err error
	if snapshot := inst.Base.Options["snapshot"]; snapshot != "" {
		req := &boxdapi.CreateVmFromSnapshotRequest{Snapshot: snapshot, Name: name}
		if opts.CPUs != 0 || opts.Memory != 0 {
			// With none asked for, the snapshot's own size.
			req.Config = config
		}
		created, err = api.CreateVmFromSnapshot(ctx, req)
	} else {
		created, err = api.CreateVm(ctx, &boxdapi.CreateVmRequest{
			Name: name, ImageRef: inst.Base.Options["image"], RestartPolicy: "never", Config: config,
		})
	}
	if err != nil {
		return vmRef{}, fmt.Errorf("boxd: create %s from %s: %w", name, inst.Base.Name, err)
	}
	ref := vmRef{ID: created.GetVmId(), Name: created.GetName()}
	path := filepath.Join(inst.Dir, vmFile)
	if err := writeJSON(path, ref); err != nil {
		d.destroyVM(ref.ID)
		return vmRef{}, err
	}
	err = d.settle(ctx, api, ref.ID)
	if err == nil {
		_, err = waitVM(ctx, api, ref.ID, "running")
	}
	if err == nil {
		err = d.expose(ctx, api, inst)
	}
	if err == nil {
		err = readJSON(path, &ref)
	}
	if err != nil {
		// Undone whole, so the next start makes the machine again rather
		// than boot one with no URL.
		d.destroyVM(ref.ID)
		_ = os.Remove(path)
		return vmRef{}, err
	}
	return ref, nil
}

// restore creates the instance's machine from a snapshot and waits for it to
// run. vm.json is written as soon as the machine exists, so Destroy removes it
// even when a later step fails.
func (d *Driver) restore(ctx context.Context, api boxdapi.BoxdApiClient, inst machine.InstanceSpec, snapshot string) (string, error) {
	created, err := api.CreateVmFromSnapshot(ctx, &boxdapi.CreateVmFromSnapshotRequest{
		Snapshot: snapshot,
		Name:     vmName(inst.Project, inst.ID),
	})
	if err != nil {
		return "", fmt.Errorf("boxd: restore %s: %w", snapshot, err)
	}
	vm := created.GetVmId()
	if err := writeJSON(filepath.Join(inst.Dir, vmFile), vmRef{ID: vm, Name: created.GetName()}); err != nil {
		d.destroyVM(vm)
		return "", err
	}
	if err := d.settle(ctx, api, vm); err != nil {
		return "", err
	}
	if _, err := waitVM(ctx, api, vm, "running"); err != nil {
		return "", err
	}
	return vm, nil
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

func (d *Driver) Boot(ctx context.Context, inst machine.InstanceSpec, opts machine.BootOptions) (machine.Machine, error) {
	api, err := d.api.API()
	if err != nil {
		return nil, err
	}
	var ref vmRef
	if err := readJSON(filepath.Join(inst.Dir, vmFile), &ref); err != nil {
		if inst.Base == nil || !errors.Is(err, fs.ErrNotExist) {
			return nil, fmt.Errorf("boxd: instance %s is not prepared: %w", inst.ID, err)
		}
		if ref, err = d.create(ctx, api, inst, opts); err != nil {
			return nil, err
		}
	}
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
		cpus, memory := uint32(opts.CPUs), opts.Memory
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
func (d *Driver) Attach(ctx context.Context, inst machine.InstanceSpec) (machine.Machine, error) {
	var ref vmRef
	if err := readJSON(filepath.Join(inst.Dir, vmFile), &ref); err != nil {
		return nil, fmt.Errorf("boxd: instance %s has no machine: %w", inst.ID, machine.ErrNotRunning)
	}
	api, err := d.api.API()
	if err != nil {
		return nil, err
	}
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

// Commit boots the stopped instance from its committed disk and snapshots it:
// a boxd snapshot can only be taken of a running machine.
func (d *Driver) Commit(ctx context.Context, inst machine.InstanceSpec, dst machine.Layer) error {
	var ref vmRef
	if err := readJSON(filepath.Join(inst.Dir, vmFile), &ref); errors.Is(err, fs.ErrNotExist) && inst.Base != nil {
		return fmt.Errorf("boxd: instance %s of %s has never started, so it has no machine to commit; start it once, then stop it", inst.ID, inst.Base.Name)
	} else if err != nil {
		return fmt.Errorf("boxd: instance %s: %w", inst.ID, err)
	}
	api, err := d.api.API()
	if err != nil {
		return err
	}
	info, err := api.GetVm(ctx, &boxdapi.GetVmRequest{VmId: ref.ID})
	if err != nil {
		return fmt.Errorf("boxd: machine %s: %w", ref.Name, err)
	}
	if info.GetStatus() != "stopped" {
		return fmt.Errorf("boxd: commit needs a stopped machine; %s is %s", ref.Name, info.GetStatus())
	}
	if _, err := api.StartVm(ctx, &boxdapi.StartVmRequest{VmId: ref.ID}); err != nil {
		return fmt.Errorf("boxd: start %s: %w", ref.Name, err)
	}
	if _, err := waitVM(ctx, api, ref.ID, "running"); err != nil {
		return err
	}
	if err := waitGuest(ctx, api, ref.ID, nil); err != nil {
		return err
	}
	snap, err := d.snapshot(ctx, api, ref.ID, dst)
	if err != nil {
		return err
	}
	return writeJSON(filepath.Join(dst.Dir, layerFile), snap)
}

func (d *Driver) Destroy(ctx context.Context, inst machine.InstanceSpec) error {
	var ref vmRef
	if err := readJSON(filepath.Join(inst.Dir, vmFile), &ref); err == nil {
		api, err := d.api.API()
		if err != nil {
			return err
		}
		if _, err := api.DestroyVm(ctx, &boxdapi.DestroyVmRequest{VmId: ref.ID}); err != nil && !isNotFound(err) {
			return fmt.Errorf("boxd: destroy %s: %w", ref.Name, err)
		}
	}
	return os.RemoveAll(inst.Dir)
}

func (d *Driver) DeleteLayer(ctx context.Context, layer machine.Layer) error {
	var snap snapshotRef
	if err := readJSON(filepath.Join(layer.Dir, layerFile), &snap); err != nil {
		return nil //nolint:nilerr // never committed, or already released: nothing to delete
	}
	return d.deleteSnapshot(ctx, snap.Name)
}

// LayerExists reports whether boxd still has the layer's snapshot, which
// `boxd snap rm` deletes without disco-vm knowing.
func (d *Driver) LayerExists(ctx context.Context, layer machine.Layer) (bool, error) {
	var snap snapshotRef
	if err := readJSON(filepath.Join(layer.Dir, layerFile), &snap); err != nil {
		return false, nil //nolint:nilerr // a layer with no snapshot recorded has none
	}
	api, err := d.api.API()
	if err != nil {
		return false, err
	}
	got, err := api.GetSnapshot(ctx, &boxdapi.GetSnapshotRequest{Name: snap.Name})
	if isNotFound(err) {
		return false, nil
	}
	if err != nil {
		return false, fmt.Errorf("boxd: snapshot %s: %w", snap.Name, err)
	}
	return got.GetSnapshot().GetStatus() != "failed", nil
}

// TagLayer saves a second snapshot of the layer, named for the layer and the
// tag, so that `boxd snap ls` says which snapshots are which images. boxd has
// no way to name or label a snapshot after the fact, and snapshots only a
// running machine, so the layer's snapshot is restored into a machine and that
// machine is snapshotted. Nothing restores from a tag's snapshot.
func (d *Driver) TagLayer(ctx context.Context, layer machine.Layer, ref string) error {
	var snap snapshotRef
	if err := readJSON(filepath.Join(layer.Dir, layerFile), &snap); err != nil {
		return fmt.Errorf("boxd: tag %s: %w", ref, err)
	}
	api, err := d.api.API()
	if err != nil {
		return err
	}
	created, err := api.CreateVmFromSnapshot(ctx, &boxdapi.CreateVmFromSnapshotRequest{Snapshot: snap.Name, Name: vmName(layer.Project, "tag")})
	if err != nil {
		return fmt.Errorf("boxd: tag %s: restore %s: %w", ref, snap.Name, err)
	}
	vm := created.GetVmId()
	defer d.destroyVM(vm)
	if _, err := waitVM(ctx, api, vm, "running"); err != nil {
		return err
	}
	name := tagSnapshot(layer, ref)
	saved, err := api.CreateSnapshot(ctx, &boxdapi.CreateSnapshotRequest{Vm: vm, Name: name})
	if err != nil {
		return fmt.Errorf("boxd: tag %s: snapshot %s: %w", ref, name, err)
	}
	if _, err := waitSnapshot(ctx, api, name, saved.GetVersion()); err != nil {
		_ = d.deleteSnapshot(context.Background(), name)
		return err
	}
	return nil
}

// UntagLayer deletes the tag's snapshot.
func (d *Driver) UntagLayer(ctx context.Context, layer machine.Layer, ref string) error {
	return d.deleteSnapshot(ctx, tagSnapshot(layer, ref))
}

// tagSnapshot names a tag's snapshot
// `dvm-<project>--<layer id>-tag-<ref>-<hash>`: beside its layer's
// `dvm-<project>--<layer id>-<random>` in a sorted listing, readable, and,
// since a boxd name has no room for a ref's punctuation, told apart from
// another ref that reads the same by a hash of the ref. The ref is cut short
// to keep the name within 66 characters.
func tagSnapshot(layer machine.Layer, ref string) string {
	sum := sha256.Sum256([]byte(ref))
	head := machine.ResourcePrefix(namePrefix, layer.Project) + label(layer.ID, 12) + "-tag-"
	tail := "-" + hex.EncodeToString(sum[:2])
	room := max(66-len(head)-len(tail), 0)
	var b strings.Builder
	for _, r := range strings.ToLower(ref) {
		if b.Len() >= room {
			break
		}
		switch {
		case r >= 'a' && r <= 'z' || r >= '0' && r <= '9':
			b.WriteRune(r)
		case b.Len() > 0 && !strings.HasSuffix(b.String(), "-"):
			b.WriteByte('-')
		}
	}
	return head + strings.Trim(b.String(), "-") + tail
}

// snapshot captures a running machine under a new name for layer,
// `dvm-<project>--<layer id>-<random>`, and waits until the snapshot can be
// restored.
func (d *Driver) snapshot(ctx context.Context, api boxdapi.BoxdApiClient, vm string, layer machine.Layer) (snapshotRef, error) {
	name := machine.ResourcePrefix(namePrefix, layer.Project) + label(layer.ID, 12) + "-" + fsutil.RandomHex(3)
	created, err := api.CreateSnapshot(ctx, &boxdapi.CreateSnapshotRequest{Vm: vm, Name: name})
	if err != nil {
		return snapshotRef{}, fmt.Errorf("boxd: snapshot %s: %w", vm, err)
	}
	snap, err := waitSnapshot(ctx, api, name, created.GetVersion())
	if err != nil {
		// Nothing records the name yet, so nothing else would delete it.
		cleanup, cancel := context.WithTimeout(context.Background(), time.Minute)
		defer cancel()
		_, _ = api.DeleteSnapshot(cleanup, &boxdapi.DeleteSnapshotRequest{Name: name})
	}
	return snap, err
}

// waitSnapshot waits until a snapshot can be restored.
func waitSnapshot(ctx context.Context, api boxdapi.BoxdApiClient, name string, version uint64) (snapshotRef, error) {
	ctx, cancel := context.WithTimeout(ctx, waitLimit)
	defer cancel()
	for {
		got, err := api.GetSnapshot(ctx, &boxdapi.GetSnapshotRequest{Name: name})
		if err != nil && !isNotFound(err) {
			return snapshotRef{}, fmt.Errorf("boxd: snapshot %s: %w", name, err)
		}
		info := got.GetSnapshot()
		switch {
		case info.GetStatus() == "failed":
			return snapshotRef{}, fmt.Errorf("boxd: snapshot %s failed", name)
		case info.GetStatus() == "ready" && info.GetVersion() >= version:
			return snapshotRef{
				Name: name, ID: info.GetSnapshotId(), Version: info.GetVersion(),
				CPUs: info.GetVcpu(), Memory: info.GetMemoryBytes(),
			}, nil
		}
		select {
		case <-ctx.Done():
			return snapshotRef{}, fmt.Errorf("boxd: snapshot %s is still %q: %w", name, info.GetStatus(), ctx.Err())
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
// layer commit follows too.
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
// every stop that keeps the disk (Shutdown, a cold clone's, a layer's install)
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

// vmName is a fresh machine name that says which project it is in and where
// it came from: `dvm-<project>--<id>-<random>`, at most 59 characters, so it
// is a DNS label in its proxy's domain.
func vmName(project, id string) string {
	return machine.ResourcePrefix(namePrefix, project) + label(id, 16) + "-" + fsutil.RandomHex(2)
}

// label keeps the characters of s that a boxd name allows, up to n of them.
func label(s string, n int) string {
	var b strings.Builder
	for _, r := range strings.ToLower(s) {
		if b.Len() == n {
			break
		}
		if r >= 'a' && r <= 'z' || r >= '0' && r <= '9' {
			b.WriteRune(r)
		}
	}
	if b.Len() == 0 {
		return "x"
	}
	return b.String()
}

func logger(w io.Writer) func(string, ...any) {
	return func(format string, args ...any) {
		if w != nil {
			fmt.Fprintf(w, format+"\n", args...)
		}
	}
}

func readJSON(path string, v any) error {
	data, err := os.ReadFile(path)
	if err != nil {
		return err
	}
	return json.Unmarshal(data, v)
}

// writeJSON writes under a temporary name and renames, so a reader never sees
// half a file.
func writeJSON(path string, v any) error {
	data, err := json.MarshalIndent(v, "", "  ")
	if err != nil {
		return err
	}
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, data, 0o644); err != nil { //nolint:gosec // G306: snapshot and stage references, no secrets; readable like the rest of the state root
		return err
	}
	return os.Rename(tmp, path)
}
