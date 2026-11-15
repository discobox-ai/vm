package hcs

import (
	"context"
	"path/filepath"
	"testing"
	"time"
	"unsafe"

	"golang.org/x/sys/windows"

	"github.com/discobox-ai/vm/pkg/machine"
	"github.com/discobox-ai/vm/pkg/machine/local"
)

var procHcnOpenEndpoint = computenetwork.NewProc("HcnOpenEndpoint")

// endpointExists reports whether HNS still holds the endpoint id.
func endpointExists(t *testing.T, id string) bool {
	t.Helper()
	g, err := windows.GUIDFromString("{" + id + "}")
	if err != nil {
		t.Fatal(err)
	}
	var ep uintptr
	var record *uint16
	hr, _, _ := procHcnOpenEndpoint.Call(uintptr(unsafe.Pointer(&g)), uintptr(unsafe.Pointer(&ep)), uintptr(unsafe.Pointer(&record)))
	hcnResult(record)
	if hr != 0 {
		return false
	}
	_, _, _ = procHcnCloseEndpoint.Call(ep)
	return true
}

// killedShim boots inst and then does what a killed process does: its
// handles close, and HCS ends the VM (ShouldTerminateOnLastHandleClosed), but
// none of its own cleanup runs. It returns the boot's endpoint.
func killedShim(t *testing.T, d *Hypervisor, inst local.InstanceSpec) *endpoint {
	t.Helper()
	m, err := d.boot(inst, local.BootOptions{CPUs: 2, Memory: 2 << 30})
	if err != nil {
		t.Fatal(err)
	}
	nic := m.nic
	t.Cleanup(func() { deleteEndpoint(nic.ID) })
	m.unregister()
	m.sys.close()
	id := vmID(inst.Dir)
	for deadline := time.Now().Add(time.Minute); ; {
		sys, err := openSystem(id)
		if err != nil {
			break
		}
		sys.close()
		if time.Now().After(deadline) {
			t.Fatalf("HCS still has system %s a minute after its last handle closed", id)
		}
		time.Sleep(100 * time.Millisecond)
	}
	if !endpointExists(t, nic.ID) {
		t.Fatalf("endpoint %s is gone with its VM; nothing here to free", nic.ID)
	}
	return nic
}

func killedInstance(t *testing.T, name string) (*Hypervisor, local.InstanceSpec) {
	t.Helper()
	base := basePayload(t)
	d := &Hypervisor{}
	inst := local.InstanceSpec{ID: name, Dir: filepath.Join(t.TempDir(), name), GuestOS: machine.Windows,
		Chain: []local.Layer{{ID: "base", Dir: base}}}
	if err := d.Prepare(context.Background(), inst); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = d.Destroy(context.Background(), inst) })
	return d, inst
}

// A shim killed while its VM runs never reaches vm.exited, so the endpoint
// its boot made is not deleted there. Destroy must free it all the same.
func TestDestroyFreesTheEndpointOfAKilledShim(t *testing.T) {
	d, inst := killedInstance(t, "killed")
	nic := killedShim(t, d, inst)
	if err := d.Destroy(context.Background(), inst); err != nil {
		t.Fatal(err)
	}
	if endpointExists(t, nic.ID) {
		t.Fatalf("endpoint %s outlives its instance's Destroy", nic.ID)
	}
}

// HNS refuses a MAC a leaked endpoint holds (0x803B002F), so the next boot
// of a killed shim's instance frees that endpoint first, and keeps its MAC.
func TestTheNextBootFreesTheEndpointOfAKilledShim(t *testing.T) {
	d, inst := killedInstance(t, "rebooted")
	first := killedShim(t, d, inst)
	m, err := d.boot(inst, local.BootOptions{CPUs: 2, Memory: 2 << 30})
	if err != nil {
		t.Fatalf("boot after a killed shim: %v", err)
	}
	defer m.shutdown()
	if endpointExists(t, first.ID) {
		t.Errorf("the killed shim's endpoint %s outlives the next boot", first.ID)
	}
	if m.nic.MAC != first.MAC {
		t.Errorf("the next boot's MAC is %s, not the instance's %s", m.nic.MAC, first.MAC)
	}
}

// Commit removes state.json, the only record of a killed shim's endpoint, so
// it frees the endpoint first.
func TestCommitFreesTheEndpointOfAKilledShim(t *testing.T) {
	d, inst := killedInstance(t, "committed")
	nic := killedShim(t, d, inst)
	if err := d.Commit(context.Background(), inst); err != nil {
		t.Fatal(err)
	}
	if endpointExists(t, nic.ID) {
		t.Fatalf("endpoint %s outlives its instance's Commit", nic.ID)
	}
}

// A shim killed before the endpoint's ID was recorded left a state.json with
// the MAC alone, and HNS refuses that MAC while the endpoint holds it. The
// next boot cannot tell that endpoint from another VM's, so it leaves it and
// boots with another MAC.
func TestTheNextBootOverAnUnrecordedEndpointTakesAnotherMAC(t *testing.T) {
	d, inst := killedInstance(t, "unrecorded")
	first := killedShim(t, d, inst)
	if err := writeState(inst.Dir, instanceState{MAC: first.MAC}); err != nil {
		t.Fatal(err)
	}
	m, err := d.boot(inst, local.BootOptions{CPUs: 2, Memory: 2 << 30})
	if err != nil {
		t.Fatalf("boot over an unrecorded endpoint holding its MAC: %v", err)
	}
	defer m.shutdown()
	if m.nic.MAC == first.MAC {
		t.Fatalf("HNS gave the next boot %s, which the unrecorded endpoint %s holds", m.nic.MAC, first.ID)
	}
	if got := readState(inst.Dir).MAC; got != m.nic.MAC {
		t.Errorf("state.json's MAC is %s, not the boot's %s", got, m.nic.MAC)
	}
	if !endpointExists(t, first.ID) {
		t.Errorf("the unrecorded endpoint %s was deleted; nothing here knows it is this instance's", first.ID)
	}
}
