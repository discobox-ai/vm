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
//     named in the instance's vm.json. A cold clone is powered off through the
//     agent once restored, so its first boot is a cold boot of the layer's disk.
//     A resume clone is suspended once restored, and its first boot resumes it.
//   - A warm stage points at a snapshot to resume from: the layer's own, or one
//     taken at another size. A snapshot restores any number of times, so a warm
//     image never runs out (Warmth.Clones is -1).
//
// There is no hypervisor socket to the guest. The agent listens on a root-only
// unix socket, and Dial reaches a guest port by running `disco-vm pipe` in the
// guest over boxd's authenticated Exec stream.
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
	"debug/elf"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"time"

	"github.com/discobox-ai/vm/internal/fsutil"
	"github.com/discobox-ai/vm/pkg/guest"
	"github.com/discobox-ai/vm/pkg/machine"
	"github.com/discobox-ai/vm/pkg/machine/boxd/internal/boxdapi"
)

func init() {
	machine.Register("boxd", func() (machine.Driver, error) { return New(), nil })
}

const (
	// agentPath and agentSocket are where Install puts the agent in the guest.
	agentPath   = "/usr/local/libexec/disco-vm"
	agentSocket = "/run/disco-vm/agent.sock"
	agentUnit   = "disco-vm-guest.service"
	// haltedMarker is written by the shutdown hook (shutdownHook) and read by
	// the driver. It is on tmpfs, so a cold boot starts without one.
	haltedMarker = "/run/disco-vm/halted"
	shutdownHook = "/usr/lib/systemd/system-shutdown/disco-vm"

	// maxDisk is boxd's fixed disk: every machine has 100 GiB.
	maxDisk = 100 << 30

	layerFile = "layer.json"
	vmFile    = "vm.json"
	stageFile = "stage.json"

	// waitLimit bounds each wait for a machine or snapshot to change state.
	waitLimit = 10 * time.Minute
)

// Driver is the boxd driver.
type Driver struct {
	api *client
}

var (
	_ machine.Driver = (*Driver)(nil)
	_ machine.Warmer = (*Driver)(nil)
)

// New returns a driver that authenticates with $BOXD_API_KEY. It dials
// nothing until it is used.
func New() *Driver { return &Driver{api: newClient()} }

func (*Driver) Name() string { return "boxd" }

func (*Driver) Capabilities() machine.Capabilities {
	return machine.Capabilities{
		GuestOS:    []machine.OS{machine.Linux},
		CloneModes: []machine.CloneMode{machine.Cold, machine.Resume},
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

// snapshotRef is a layer's layer.json and a stage's stage.json.
type snapshotRef struct {
	Name    string `json:"name"`
	ID      string `json:"id"`
	Version uint64 `json:"version"`
	CPUs    uint32 `json:"cpus"`
	Memory  uint64 `json:"memory"`
	// Own is set on a stage that took its own snapshot, which Cool deletes. A
	// stage that points at its layer's snapshot leaves it to DeleteLayer.
	Own bool `json:"own,omitempty"`
}

// vmRef is an instance's vm.json.
type vmRef struct {
	ID   string `json:"id"`
	Name string `json:"name"`
}

// Install creates a machine from one of boxd's own images, installs the agent
// as a systemd service, and snapshots it as the base layer.
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
		Name:          vmName("install"),
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
	if err := checkArch(ctx, api, vm, spec.Agent); err != nil {
		return err
	}

	log("boxd: installing the agent")
	if err := upload(ctx, api, vm, spec.Agent, "/tmp/disco-vm-agent"); err != nil {
		return fmt.Errorf("boxd: upload agent: %w", err)
	}
	if out, code, err := run(ctx, api, vm, installScript); err != nil {
		return fmt.Errorf("boxd: install agent: %w: %s", err, out)
	} else if code != 0 {
		return fmt.Errorf("boxd: install agent: exit %d: %s", code, out)
	}
	if err := d.waitAgent(ctx, api, vm); err != nil {
		return err
	}

	log("boxd: snapshotting the base layer")
	snap, err := d.snapshot(ctx, api, vm, dst.ID)
	if err != nil {
		return err
	}
	return writeJSON(filepath.Join(dst.Dir, layerFile), snap)
}

// installScript runs as boxd's default user, who has passwordless sudo.
var installScript = `set -e
sudo mkdir -p ` + filepath.Dir(agentPath) + `
sudo install -m 0755 /tmp/disco-vm-agent ` + agentPath + `
rm -f /tmp/disco-vm-agent
sudo tee /etc/systemd/system/` + agentUnit + ` >/dev/null <<'UNIT'
[Unit]
Description=disco-vm guest agent
After=local-fs.target

[Service]
ExecStart=` + agentPath + ` guest --listen unix:` + agentSocket + `
Restart=always
RestartSec=1

[Install]
WantedBy=multi-user.target
UNIT
sudo mkdir -p ` + filepath.Dir(shutdownHook) + `
sudo tee ` + shutdownHook + ` >/dev/null <<'HOOK'
#!/bin/sh
# systemd-shutdown runs this once disks are synced, just before it asks the
# kernel to power off, which a boxd machine ignores. disco-vm's boxd driver
# watches for the marker and stops the machine through boxd's API.
mkdir -p ` + filepath.Dir(haltedMarker) + `
echo "$1" > ` + haltedMarker + `
HOOK
sudo chmod 0755 ` + shutdownHook + `
sudo systemctl daemon-reload
sudo systemctl enable --now ` + agentUnit + `
# The base layer is snapshotted without a shutdown, and a cold clone boots
# its disk: what is installed must be on it.
sync
`

func (d *Driver) Prepare(ctx context.Context, inst machine.InstanceSpec) error {
	if len(inst.Chain) == 0 {
		return errors.New("boxd: instance has no image")
	}
	if err := os.MkdirAll(inst.Dir, 0o755); err != nil {
		return err
	}
	api, err := d.api.API()
	if err != nil {
		return err
	}
	switch inst.Mode {
	case "", machine.Cold:
		var layer snapshotRef
		if err := readJSON(filepath.Join(inst.Parent().Dir, layerFile), &layer); err != nil {
			return fmt.Errorf("boxd: layer %s: %w", inst.Parent().ID, err)
		}
		vm, err := d.restore(ctx, api, inst, layer.Name)
		if err != nil {
			return err
		}
		// The snapshot is of a running guest. Power it off in order, so the
		// first boot is a cold boot of the layer's disk.
		return d.powerOff(ctx, api, vm)
	case machine.Resume:
		var stage snapshotRef
		if err := readJSON(filepath.Join(inst.WarmDir, stageFile), &stage); err != nil {
			return fmt.Errorf("boxd: %w", machine.ErrNotWarm)
		}
		vm, err := d.restore(ctx, api, inst, stage.Name)
		if isNotFound(err) {
			return fmt.Errorf("boxd: snapshot %s is gone: %w", stage.Name, machine.ErrNotWarm)
		}
		if err != nil {
			return err
		}
		// Held suspended, with its memory, until Boot resumes it.
		if _, err := api.SuspendVm(ctx, &boxdapi.SuspendVmRequest{VmId: vm}); err != nil {
			return fmt.Errorf("boxd: suspend %s: %w", vm, err)
		}
		return nil
	default:
		return fmt.Errorf("boxd: clone mode %q: %w", inst.Mode, machine.ErrUnsupported)
	}
}

// restore creates the instance's machine from a snapshot and waits for it to
// run. vm.json is written as soon as the machine exists, so Destroy removes it
// even when a later step fails.
func (d *Driver) restore(ctx context.Context, api boxdapi.BoxdApiClient, inst machine.InstanceSpec, snapshot string) (string, error) {
	created, err := api.CreateVmFromSnapshot(ctx, &boxdapi.CreateVmFromSnapshotRequest{
		Snapshot: snapshot,
		Name:     vmName(inst.ID),
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
	var ref vmRef
	if err := readJSON(filepath.Join(inst.Dir, vmFile), &ref); err != nil {
		return nil, fmt.Errorf("boxd: instance %s is not prepared: %w", inst.ID, err)
	}
	api, err := d.api.API()
	if err != nil {
		return nil, err
	}
	info, err := api.GetVm(ctx, &boxdapi.GetVmRequest{VmId: ref.ID})
	if err != nil {
		return nil, fmt.Errorf("boxd: machine %s: %w", ref.Name, err)
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
	if opts.Console != nil {
		fmt.Fprintf(opts.Console, "boxd: %s (%s) is running\n", ref.Name, ref.ID)
	}
	return watch(api, ref), nil
}

// Commit boots the stopped instance from its committed disk and snapshots it:
// a boxd snapshot can only be taken of a running machine.
func (d *Driver) Commit(ctx context.Context, inst machine.InstanceSpec, dst machine.Layer) error {
	var ref vmRef
	if err := readJSON(filepath.Join(inst.Dir, vmFile), &ref); err != nil {
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
	if err := d.waitAgent(ctx, api, ref.ID); err != nil {
		return err
	}
	snap, err := d.snapshot(ctx, api, ref.ID, dst.ID)
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

// Warm points the stage at a snapshot to resume clones from. At the layer's
// own size that is the layer's snapshot; at another size it is a new snapshot
// of the layer booted at that size.
func (d *Driver) Warm(ctx context.Context, spec machine.WarmSpec) (machine.Stage, error) {
	if len(spec.Chain) == 0 {
		return nil, errors.New("boxd: warm: no image")
	}
	if spec.User != nil {
		return nil, fmt.Errorf("boxd: warm: a stage that logs in a user: %w", machine.ErrUnsupported)
	}
	var layer snapshotRef
	if err := readJSON(filepath.Join(spec.Chain[len(spec.Chain)-1].Dir, layerFile), &layer); err != nil {
		return nil, fmt.Errorf("boxd: warm: %w", err)
	}
	cpus, memory := uint32(spec.CPUs), spec.Memory
	sized := func(s snapshotRef) bool {
		return (cpus == 0 || cpus == s.CPUs) && (memory == 0 || memory == s.Memory)
	}
	path := filepath.Join(spec.Dir, stageFile)
	var old snapshotRef
	if readJSON(path, &old) == nil {
		if sized(old) {
			return nil, nil
		}
		if err := d.Cool(ctx, spec); err != nil {
			return nil, err
		}
		if err := os.MkdirAll(spec.Dir, 0o755); err != nil {
			return nil, err
		}
	}
	log := logger(spec.Log)
	if sized(layer) {
		log("boxd: clones resume from the layer's snapshot %s", layer.Name)
		return nil, writeJSON(path, layer)
	}

	api, err := d.api.API()
	if err != nil {
		return nil, err
	}
	log("boxd: snapshotting the image at %d CPUs, %d bytes", cpus, memory)
	inst := machine.InstanceSpec{ID: "warm", Dir: filepath.Join(spec.Dir, "tmp-"+fsutil.RandomHex(6)), Chain: spec.Chain, Mode: machine.Cold}
	defer func() { _ = d.Destroy(context.Background(), inst) }()
	if err := d.Prepare(ctx, inst); err != nil {
		return nil, err
	}
	m, err := d.Boot(ctx, inst, machine.BootOptions{CPUs: spec.CPUs, Memory: spec.Memory})
	if err != nil {
		return nil, err
	}
	defer func() { _ = m.Kill(context.Background()) }()
	var ref vmRef
	if err := readJSON(filepath.Join(inst.Dir, vmFile), &ref); err != nil {
		return nil, err
	}
	if err := d.waitAgent(ctx, api, ref.ID); err != nil {
		return nil, err
	}
	snap, err := d.snapshot(ctx, api, ref.ID, spec.Chain[len(spec.Chain)-1].ID)
	if err != nil {
		return nil, err
	}
	snap.Own = true
	return nil, writeJSON(path, snap)
}

func (d *Driver) Warmth(ctx context.Context, spec machine.WarmSpec) (machine.Warmth, error) {
	var stage snapshotRef
	if err := readJSON(filepath.Join(spec.Dir, stageFile), &stage); err != nil {
		return machine.Warmth{Mode: machine.Cold}, nil //nolint:nilerr // no readable stage snapshot means a cold start, not a failure
	}
	api, err := d.api.API()
	if err != nil {
		return machine.Warmth{Mode: machine.Cold}, err
	}
	got, err := api.GetSnapshot(ctx, &boxdapi.GetSnapshotRequest{Name: stage.Name})
	if isNotFound(err) || (err == nil && got.GetSnapshot().GetStatus() != "ready") {
		return machine.Warmth{Mode: machine.Cold}, nil
	}
	if err != nil {
		return machine.Warmth{Mode: machine.Cold}, fmt.Errorf("boxd: snapshot %s: %w", stage.Name, err)
	}
	return machine.Warmth{Mode: machine.Resume, Clones: -1}, nil
}

func (d *Driver) Cool(ctx context.Context, spec machine.WarmSpec) error {
	var stage snapshotRef
	if readJSON(filepath.Join(spec.Dir, stageFile), &stage) == nil && stage.Own {
		if err := d.deleteSnapshot(ctx, stage.Name); err != nil {
			return err
		}
	}
	return os.RemoveAll(spec.Dir)
}

// snapshot captures a running machine under a new name and waits until the
// snapshot can be restored.
func (d *Driver) snapshot(ctx context.Context, api boxdapi.BoxdApiClient, vm, layerID string) (snapshotRef, error) {
	name := "dvm-" + label(layerID, 12) + "-" + fsutil.RandomHex(3)
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

// powerOff shuts a running guest down through its agent, as the engine does,
// and stops the machine once the guest has halted. A guest that cannot be shut
// down in order is still stopped, so it costs nothing, but powerOff fails: a
// disk cut off mid-write is not one to boot clones from, which is the rule a
// layer commit follows too.
func (d *Driver) powerOff(ctx context.Context, api boxdapi.BoxdApiClient, vm string) error {
	orderly := d.shutdown(ctx, api, vm)
	if _, err := api.StopVm(ctx, &boxdapi.StopVmRequest{VmId: vm}); err != nil && !isNotFound(err) {
		return errors.Join(orderly, fmt.Errorf("boxd: stop %s: %w", vm, err))
	}
	if _, err := waitVM(ctx, api, vm, "stopped"); err != nil {
		return errors.Join(orderly, err)
	}
	return orderly
}

// shutdown asks the agent for a power-off and waits for the shutdown hook to
// say the guest has halted.
func (d *Driver) shutdown(ctx context.Context, api boxdapi.BoxdApiClient, vm string) error {
	client := agentClient(api, vm)
	defer client.Close()
	ready, cancel := context.WithTimeout(ctx, 2*time.Minute)
	defer cancel()
	if err := client.WaitReady(ready, nil); err != nil {
		return fmt.Errorf("boxd: power off %s: %w", vm, err)
	}
	if err := client.Shutdown(ready, false); err != nil {
		return fmt.Errorf("boxd: power off %s: %w", vm, err)
	}
	halt, cancel := context.WithTimeout(ctx, waitLimit)
	defer cancel()
	for halted(halt, api, vm) == "" {
		select {
		case <-halt.Done():
			return fmt.Errorf("boxd: %s did not halt after a power-off: %w", vm, halt.Err())
		case <-time.After(time.Second):
		}
	}
	return nil
}

// halted reads the shutdown hook's marker: "poweroff", "halt", or "reboot"
// once the guest has shut down, and empty while it runs.
// A probe that cannot run reads as still running, to be asked again.
func halted(ctx context.Context, api boxdapi.BoxdApiClient, vm string) string {
	out, code, err := run(ctx, api, vm, "cat "+haltedMarker+" 2>/dev/null")
	if err != nil || code != 0 {
		return ""
	}
	return strings.TrimSpace(out)
}

func (d *Driver) waitAgent(ctx context.Context, api boxdapi.BoxdApiClient, vm string) error {
	client := agentClient(api, vm)
	defer client.Close()
	ctx, cancel := context.WithTimeout(ctx, 5*time.Minute)
	defer cancel()
	if err := client.WaitReady(ctx, nil); err != nil {
		return fmt.Errorf("boxd: agent on %s: %w", vm, err)
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

func agentClient(api boxdapi.BoxdApiClient, vm string) *guest.Client {
	return guest.NewClient(func(ctx context.Context) (net.Conn, error) { return dial(ctx, api, vm, guest.AgentPort) })
}

// dial reaches a guest port: the agent's port is its unix socket, and any
// other is a TCP port on the guest's loopback.
func dial(ctx context.Context, api boxdapi.BoxdApiClient, vm string, port uint32) (net.Conn, error) {
	target := fmt.Sprintf("tcp:127.0.0.1:%d", port)
	if port == guest.AgentPort {
		target = "unix:" + agentSocket
	}
	return dialExec(ctx, api, vm, "sudo -n "+agentPath+" pipe "+target, vmAddr{vm: vm, port: port})
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

// run runs a shell command in the machine and returns its combined output.
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

// upload streams a local file to a path in the machine.
func upload(ctx context.Context, api boxdapi.BoxdApiClient, vm, src, dst string) error {
	f, err := os.Open(src)
	if err != nil {
		return err
	}
	defer f.Close()
	info, err := f.Stat()
	if err != nil {
		return err
	}
	stream, err := api.UploadFileStream(ctx)
	if err != nil {
		return err
	}
	first := &boxdapi.UploadFileChunk{VmId: vm, Path: dst, TotalSize: uint64(info.Size())}
	buf := make([]byte, 1<<20)
	for {
		n, err := f.Read(buf)
		if n > 0 || first != nil {
			chunk := first
			if chunk == nil {
				chunk = &boxdapi.UploadFileChunk{}
			}
			first = nil
			chunk.Data = buf[:n]
			if serr := stream.Send(chunk); serr != nil {
				return serr
			}
		}
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return err
		}
	}
	resp, err := stream.CloseAndRecv()
	if err != nil {
		return err
	}
	if written := resp.GetBytesWritten(); written != 0 && written != uint64(info.Size()) {
		return fmt.Errorf("wrote %d of %d bytes", written, info.Size())
	}
	return nil
}

// checkArch refuses an agent built for another CPU than the machine's, which
// would otherwise fail later as a service that never starts.
func checkArch(ctx context.Context, api boxdapi.BoxdApiClient, vm, agent string) error {
	f, err := elf.Open(agent)
	if err != nil {
		return fmt.Errorf("boxd: the agent %s is not a linux binary (build disco-vm with GOOS=linux and pass --agent): %w", agent, err)
	}
	defer f.Close()
	want := map[elf.Machine]string{elf.EM_X86_64: "x86_64", elf.EM_AARCH64: "aarch64"}[f.Machine]
	out, code, err := run(ctx, api, vm, "uname -m")
	if err != nil {
		return fmt.Errorf("boxd: uname -m: %w: %s", err, out)
	}
	if code != 0 {
		return fmt.Errorf("boxd: uname -m: exit %d: %s", code, out)
	}
	if got := strings.TrimSpace(out); got != want {
		return fmt.Errorf("boxd: the machine is %s but the agent %s is built for %s", got, agent, f.Machine)
	}
	return nil
}

// vmName is a fresh machine name that says where it came from.
func vmName(id string) string { return "dvm-" + label(id, 16) + "-" + fsutil.RandomHex(2) }

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
