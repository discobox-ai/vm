package hcs

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/Microsoft/go-winio/vhd"

	"github.com/discobox-ai/vm/pkg/machine"
	"github.com/discobox-ai/vm/pkg/machine/local"
	"github.com/discobox-ai/vm/pkg/machine/machinetest"
)

// BaseEnv names an installed hcs image, by its ID, in the driver's directory
// ($DISCO_VM_ROOT, or %LOCALAPPDATA%\disco-vm): an image `disco-vm install`
// made, which `disco-vm images` lists. It gates the tests that need a real
// hypervisor and an installed Windows.
const BaseEnv = "DISCO_VM_HCS_BASE"

// basePayload is the directory holding BaseEnv's disk.vhdx, for a test that
// drives the hypervisor without the driver's store.
func basePayload(t *testing.T) string {
	t.Helper()
	id := os.Getenv(BaseEnv)
	if id == "" {
		t.Skipf("set %s to an installed hcs image to run on HCS", BaseEnv)
	}
	return filepath.Join(machine.DefaultDir(), "images", "layers", id, "payload")
}

// TestConformance runs the suite on HCS. Each started instance's shim is
// $DISCO_VM_TEST_BINARY, a disco-vm built for this host.
func TestConformance(t *testing.T) {
	base := os.Getenv(BaseEnv)
	if base == "" {
		t.Skipf("set %s to an installed hcs image to run the conformance suite on HCS", BaseEnv)
	}
	binary := os.Getenv("DISCO_VM_TEST_BINARY")
	if binary == "" {
		t.Fatal("$DISCO_VM_TEST_BINARY names no disco-vm to run each started instance's shim")
	}
	dir := machine.DefaultDir()
	driver, err := local.New(machine.Config{
		Dir:         dir,
		ShimCommand: []string{binary, "--root", dir, "--driver", "hcs", "shim"},
	}, &Hypervisor{})
	if err != nil {
		t.Fatal(err)
	}
	machinetest.Run(t, driver, machinetest.Config{
		Image:      base,
		CPUs:       4,
		Memory:     4 << 30,
		Timeout:    10 * time.Minute,
		ScratchDir: `C:\disco-vm-conformance`,
	})
}

// An instance's service port is in its ServiceTable beside the agent's, or the
// guest's bind to it is refused.
func TestDocumentService(t *testing.T) {
	raw, err := json.Marshal(newDocument(vmConfig{Disk: "d", GuestFile: "g", StateFile: "s"}))
	if err != nil {
		t.Fatal(err)
	}
	// The agent's port, 7300, and the service's, 7302.
	for _, want := range []string{`"00001C84-FACB-11E6-BD58-64006A7986D3"`, `"00001C86-FACB-11E6-BD58-64006A7986D3"`} {
		if !strings.Contains(string(raw), want) {
			t.Errorf("document lacks %s:\n%s", want, raw)
		}
	}
}

// The document is the whole interface to HCS, and the fields that silently
// keep a guest from booting must stay the way sandboxi measured them.
func TestDocument(t *testing.T) {
	nic := &endpoint{ID: "ep", MAC: "00-15-5D-00-00-01"}
	raw, err := json.Marshal(newDocument(vmConfig{Disk: `C:\x\disk.vhdx`, GuestFile: `C:\x\g.vmgs`, StateFile: `C:\x\r.vmrs`, NIC: nic}))
	if err != nil {
		t.Fatal(err)
	}
	doc := string(raw)
	for _, want := range []string{
		`"StopOnReset":false`,
		`"ApplySecureBootTemplate":"Apply"`,
		`"SecureBootTemplateId":"` + microsoftWindowsTemplate + `"`,
		`"Path":"C:/x/disk.vhdx"`,
		`"00001C84-FACB-11E6-BD58-64006A7986D3"`,
		`"ShouldTerminateOnLastHandleClosed":true`,
		`"Count":6`,
		`"SizeInMB":4096`,
		`"EndpointId":"ep"`,
	} {
		if !strings.Contains(doc, want) {
			t.Errorf("document lacks %s:\n%s", want, doc)
		}
	}
	for _, never := range []string{"BootThis", `\\`, "RestoreState"} {
		if strings.Contains(doc, never) {
			t.Errorf("document has %s:\n%s", never, doc)
		}
	}
}

// Commit moves a differencing disk into another directory. The moved child
// must still open with its whole chain.
func TestMovedChildFindsItsParent(t *testing.T) {
	root := t.TempDir()
	for _, dir := range []string{"layer", "inst", "committed"} {
		if err := os.MkdirAll(filepath.Join(root, dir), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	parent := filepath.Join(root, "layer", diskName)
	if err := vhd.CreateVhdx(parent, 1, 1); err != nil {
		t.Fatal(err)
	}
	child := filepath.Join(root, "inst", diskName)
	if err := createDiff(child, parent); err != nil {
		t.Fatal(err)
	}
	moved := filepath.Join(root, "committed", diskName)
	if err := os.Rename(child, moved); err != nil {
		t.Fatal(err)
	}
	if err := setParent(moved, parent); err != nil {
		t.Fatal(err)
	}
	grandchild := filepath.Join(root, "inst", "next.vhdx")
	if err := createDiff(grandchild, moved); err != nil {
		t.Fatalf("a disk over the moved child: %v", err)
	}
	h, err := vhd.OpenVirtualDisk(grandchild, vhd.VirtualDiskAccessNone, vhd.OpenVirtualDiskFlagNone)
	if err != nil {
		t.Fatalf("open the chain through the moved child: %v", err)
	}
	_ = syscall.CloseHandle(h)
}

func TestVMIDIsStablePerInstance(t *testing.T) {
	a, b := vmID(`C:\root\instances\abc\machine`), vmID(`c:\root\instances\ABC\machine\`)
	if a != b {
		t.Fatalf("%s != %s for the same directory", a, b)
	}
	if a == vmID(`C:\root\instances\abd\machine`) {
		t.Fatal("two instances share a VM ID")
	}
	if len(a) != 36 {
		t.Fatalf("%q is not a GUID", a)
	}
}
