package hcs

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/Microsoft/go-winio/pkg/guid"
	"golang.org/x/sys/windows"

	"github.com/discobox-ai/vm/internal/fsutil"
	"github.com/discobox-ai/vm/pkg/guest"
	"github.com/discobox-ai/vm/pkg/machine"
	"github.com/discobox-ai/vm/pkg/machine/local"
)

func init() {
	machine.Register("hcs", func(cfg machine.Config) (machine.Driver, error) { return local.New(cfg, &Hypervisor{}) })
}

// Hypervisor runs Windows guests as HCS virtual machines, under a
// local.Driver.
type Hypervisor struct{}

var _ local.Hypervisor = (*Hypervisor)(nil)

func (*Hypervisor) Name() string { return "hcs" }

// Capabilities is what the driver offers on this host: Windows guests, and a
// display: the VM's video console, shown by mstsc.
func (*Hypervisor) Capabilities() machine.Capabilities {
	return machine.Capabilities{
		GuestOS: []machine.OS{machine.Windows},
		Display: true,
		Forward: true,
		Agent:   true,
	}
}

// The files the driver keeps. A layer's Dir holds its disk; an instance's Dir
// holds its differencing disk, its guest and runtime state, and state.json.
const (
	diskName      = "disk.vhdx"
	guestFileName = "guest.vmgs"
	stateFileName = "runtime.vmrs"
	stateJSON     = "state.json"
	// forkDiskName is the template disk a fork clone, from before fork
	// clones were removed (ADR 26-10-15-851), hard-linked into its own
	// directory. Its disk differences over it, so it boots like any other
	// instance but cannot be committed.
	forkDiskName = "template.vhdx"
)

func layerDisk(layer local.Layer) string { return filepath.Join(layer.Dir, diskName) }

// instanceState is what the driver remembers about an instance between boots.
type instanceState struct {
	// MAC is asked for again on every boot, so the guest keeps one adapter.
	MAC string `json:"mac,omitempty"`
	// Fork is a fork clone's claim on its template, set from its Prepare
	// until its first boot, from before fork clones were removed. Such an
	// instance has no disk to boot.
	Fork json.RawMessage `json:"fork,omitempty"`
}

func readState(dir string) instanceState {
	var st instanceState
	_ = fsutil.ReadJSON(filepath.Join(dir, stateJSON), &st)
	return st
}

func writeState(dir string, st instanceState) error {
	return fsutil.WriteJSON(filepath.Join(dir, stateJSON), st)
}

// vmID is an instance's compute-system ID. It is stable per instance, so
// every boot of it is the same VM identity and gets the same ACEs, and it is
// derived from the instance's directory, which is unique on this host.
func vmID(dir string) string {
	ns := guid.GUID{Data1: 0xd15c0b0c, Data2: 0x766d, Data3: 0x5000, Data4: [8]byte{0x80, 0, 0, 0, 0, 0, 0, 1}}
	id, err := guid.NewV5(ns, []byte(strings.ToLower(filepath.Clean(dir))))
	if err != nil {
		panic(err)
	}
	return id.String()
}

func newID() string {
	id, err := guid.NewV4()
	if err != nil {
		panic(err)
	}
	return id.String()
}

// Check reports whether this host can run HCS virtual machines.
func (*Hypervisor) Check(context.Context) error {
	if err := computecore.Load(); err != nil {
		return fmt.Errorf("hcs: computecore.dll is missing: enable the Virtual Machine Platform Windows feature and reboot (%w)", err)
	}
	if !windows.GetCurrentProcessToken().IsElevated() {
		return errors.New("hcs: creating VMs needs an elevated (administrator) process: run disco-vm from an elevated shell")
	}
	if _, err := enumerate(); err != nil {
		return fmt.Errorf("hcs: the Host Compute Service is not answering: enable the Virtual Machine Platform Windows feature, reboot, and check the vmcompute service (%w)", err)
	}
	return nil
}

func (d *Hypervisor) Prepare(_ context.Context, inst local.InstanceSpec) error {
	if len(inst.Chain) == 0 {
		return errors.New("hcs: instance has no image")
	}
	if err := os.MkdirAll(inst.Dir, 0o755); err != nil {
		return err
	}
	return createDiff(filepath.Join(inst.Dir, diskName), layerDisk(inst.Parent()))
}

// grantPaths is every file and directory a VM booting inst must be able to
// open: its own, and every disk in its chain. A missing ACE anywhere in a
// differencing chain fails Construct with an access denied that names no
// path, so every layer is walked, not just the parent.
func grantPaths(dir string, chain []local.Layer) []string {
	paths := []string{dir}
	entries, _ := os.ReadDir(dir)
	for _, entry := range entries {
		paths = append(paths, filepath.Join(dir, entry.Name()))
	}
	for _, layer := range chain {
		paths = append(paths, layer.Dir)
		disks, _ := filepath.Glob(filepath.Join(layer.Dir, "*.vhdx"))
		paths = append(paths, disks...)
	}
	return paths
}

func grantAll(id string, paths []string) error {
	for _, path := range paths {
		if err := grantVMAccess(id, path); err != nil {
			return err
		}
	}
	return nil
}

// freshState replaces an instance's guest and runtime state files. A cold
// boot reads neither (HCS does not restore RuntimeStateFilePath for this
// document), so every cold boot starts from empty ones.
func freshState(dir string) error {
	for _, f := range []struct {
		name   string
		create func(string) error
	}{{guestFileName, createGuestStateFile}, {stateFileName, createRuntimeStateFile}} {
		path := filepath.Join(dir, f.name)
		if err := os.Remove(path); err != nil && !errors.Is(err, fs.ErrNotExist) {
			return err
		}
		if err := f.create(path); err != nil {
			return fmt.Errorf("%w: %s", err, path)
		}
	}
	return nil
}

// terminateStale ends a system left under id by a boot that went wrong. The
// ID is ours by construction, so whatever holds it is ours to end.
func terminateStale(id string) {
	if sys, err := openSystem(id); err == nil {
		_ = sys.terminate()
		sys.close()
	}
}

// Boot starts the instance and waits for its agent. With opts.GUI it opens a
// window on the VM's video console that closes when the VM stops. Closing the
// window leaves the VM running.
func (d *Hypervisor) Boot(ctx context.Context, inst local.InstanceSpec, opts local.BootOptions) (local.Machine, error) {
	m, err := d.boot(inst, opts)
	if err != nil {
		return nil, err
	}
	if opts.GUI {
		d.view(m, inst, opts)
	}
	if err := m.WaitReady(ctx, m.done); err != nil {
		m.shutdown()
		return nil, err
	}
	return m, nil
}

// view opens a window on the VM's video console.
func (d *Hypervisor) view(m *vm, inst local.InstanceSpec, opts local.BootOptions) {
	title := opts.Title
	if title == "" {
		title = inst.ID
	}
	v, err := openViewer(m.sys.id, title)
	if err != nil {
		// A VM nobody can watch still runs; say so rather than fail it.
		if opts.Console != nil {
			fmt.Fprintf(opts.Console, "hcs: %v\n", err)
		}
		return
	}
	go func() {
		<-m.Done()
		v.Close()
	}()
}

func (d *Hypervisor) boot(inst local.InstanceSpec, opts local.BootOptions) (*vm, error) {
	if len(opts.Shares) > 0 {
		return nil, fmt.Errorf("hcs: shared directories: %w", machine.ErrUnsupported)
	}
	log := opts.Console
	if log == nil {
		log = io.Discard
	}
	st := readState(inst.Dir)
	if st.Fork != nil {
		return nil, fmt.Errorf("hcs: instance %s was prepared as a fork clone and never booted, and fork clones are gone; remove it and run its image again", inst.ID)
	}
	id := vmID(inst.Dir)
	terminateStale(id)
	if err := freshState(inst.Dir); err != nil {
		return nil, err
	}
	if err := grantAll(id, grantPaths(inst.Dir, inst.Chain)); err != nil {
		return nil, err
	}
	nic, err := createEndpoint(st.MAC)
	if err != nil {
		return nil, err
	}
	st.MAC = nic.MAC
	_ = writeState(inst.Dir, st)
	fmt.Fprintf(log, "hcs: booting %s as %s\n", inst.ID, id)
	return startVM(id, vmConfig{
		Disk:      filepath.Join(inst.Dir, diskName),
		GuestFile: filepath.Join(inst.Dir, guestFileName),
		StateFile: filepath.Join(inst.Dir, stateFileName),
		CPUs:      opts.CPUs,
		Memory:    opts.Memory,
		NIC:       nic,
		Console:   consolePipe(id), ConsoleSID: currentUserSID(),
	})
}

// Commit moves the stopped instance's differencing disk into dst and points
// it at its parent again, since a differencing disk moved across directories
// cannot be trusted to find its parent by its old relative locator.
func (d *Hypervisor) Commit(_ context.Context, inst local.InstanceSpec, dst local.Layer) error {
	if sys, err := openSystem(vmID(inst.Dir)); err == nil {
		sys.close()
		return fmt.Errorf("hcs: instance %s is still running; it must be stopped to commit", inst.ID)
	}
	if _, err := os.Stat(filepath.Join(inst.Dir, forkDiskName)); err == nil {
		return fmt.Errorf("hcs: instance %s is a fork clone, whose disk depends on its template's; it cannot be committed", inst.ID)
	}
	if err := os.MkdirAll(dst.Dir, 0o755); err != nil {
		return err
	}
	disk := layerDisk(dst)
	if err := os.Rename(filepath.Join(inst.Dir, diskName), disk); err != nil {
		return fmt.Errorf("hcs: move the instance disk into the layer: %w", err)
	}
	return setParent(disk, layerDisk(inst.Parent()))
}

// Destroy ends the instance's VM if one is running and removes its files.
func (d *Hypervisor) Destroy(_ context.Context, inst local.InstanceSpec) error {
	if inst.Dir == "" {
		return nil
	}
	id := vmID(inst.Dir)
	terminateStale(id)
	for _, path := range grantPaths(inst.Dir, inst.Chain) {
		revokeVMAccess(id, path)
	}
	return removeAll(inst.Dir)
}

// removeAll retries for a moment: vmcompute can hold a VM's files briefly
// after the VM is gone, and Windows will not delete an open file.
func removeAll(dir string) error {
	var err error
	for range 50 {
		if err = os.RemoveAll(dir); err == nil {
			return nil
		}
		time.Sleep(200 * time.Millisecond)
	}
	return err
}

// Bases is empty: every base layer is installed from media.
func (*Hypervisor) Bases(context.Context) ([]machine.Base, error) { return nil, nil }

// Service is the hvsocket service ID of guest.ServicePort, which every VM's
// ServiceTable lists; the instance's shim serves it on the host's loopback.
func (*Hypervisor) Service(local.InstanceSpec) (string, uint32, error) {
	return fmt.Sprintf("vsock:%d", guest.ServicePort), guest.ServicePort, nil
}
