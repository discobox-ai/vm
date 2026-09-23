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
	"github.com/discobox-ai/vm/pkg/machine/machinetest"
)

// BaseEnv names an installed hcs base layer: a directory holding disk.vhdx,
// such as images/layers/<id>/payload under a disco-vm root. It gates the
// conformance suite, which needs a real hypervisor and an installed Windows.
const BaseEnv = "DISCO_VM_HCS_BASE"

func TestConformance(t *testing.T) {
	base := os.Getenv(BaseEnv)
	if base == "" {
		t.Skipf("set %s to an installed hcs base layer to run the conformance suite on HCS", BaseEnv)
	}
	machinetest.Run(t, &Driver{}, machinetest.Config{
		Base:       &machine.Layer{ID: "base", Dir: base},
		Boot:       machine.BootOptions{CPUs: 4, Memory: 4 << 30, Console: testLog{t}},
		Timeout:    10 * time.Minute,
		ScratchDir: `C:\disco-vm-conformance`,
	})
}

type testLog struct{ t *testing.T }

func (l testLog) Write(p []byte) (int, error) {
	l.t.Log(strings.TrimRight(string(p), "\n"))
	return len(p), nil
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
	fork, _ := json.Marshal(newDocument(vmConfig{Disk: "d", GuestFile: "g", StateFile: "s", Template: "tpl", Memory: 8 << 30, CPUs: 2}))
	for _, want := range []string{`"RestoreState":{"TemplateSystemId":"tpl"}`, `"SizeInMB":8192`, `"Count":2`} {
		if !strings.Contains(string(fork), want) {
			t.Errorf("fork document lacks %s:\n%s", want, fork)
		}
	}
	if strings.Contains(string(fork), "NetworkAdapters") {
		t.Errorf("a fork document may not attach a NIC (the restore fails):\n%s", fork)
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
