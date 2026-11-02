package hcs

import (
	"strings"

	"github.com/Microsoft/go-winio"

	"github.com/discobox-ai/vm/pkg/guest"
)

// The compute-system document for a Windows guest booting its own disk. It is
// built from structs, so it is always well-formed JSON and never has a BOM
// (HCS rejects one as `Invalid JSON document '$'`). Every field here was
// measured in sandboxi; three of them silently prevent boot when wrong:
//
//   - Chipset.Uefi has no BootThis. Pinning the boot device makes the
//     firmware log Hyper-V-Worker 18603 and boot nothing; left out, it finds
//     the disk itself.
//   - SecureBootTemplateId needs ApplySecureBootTemplate "Apply", or the
//     certificates are never loaded and every boot source fails (18604).
//   - StopOnReset is false. Windows Setup reboots between its passes, and a
//     true here turns the first reboot into a power-off (18515).

type document struct {
	Owner                             string         `json:"Owner"`
	SchemaVersion                     schemaVersion  `json:"SchemaVersion"`
	ShouldTerminateOnLastHandleClosed bool           `json:"ShouldTerminateOnLastHandleClosed"`
	VirtualMachine                    virtualMachine `json:"VirtualMachine"`
}

type schemaVersion struct {
	Major int `json:"Major"`
	Minor int `json:"Minor"`
}

type virtualMachine struct {
	StopOnReset     bool       `json:"StopOnReset"`
	Chipset         chipset    `json:"Chipset"`
	ComputeTopology topology   `json:"ComputeTopology"`
	Devices         devices    `json:"Devices"`
	GuestState      guestState `json:"GuestState"`
}

type chipset struct {
	Uefi uefi `json:"Uefi"`
}

type uefi struct {
	ApplySecureBootTemplate string `json:"ApplySecureBootTemplate"`
	SecureBootTemplateID    string `json:"SecureBootTemplateId"`
}

type topology struct {
	Memory    memory    `json:"Memory"`
	Processor processor `json:"Processor"`
}

type memory struct {
	SizeInMB uint64 `json:"SizeInMB"`
	Backing  string `json:"Backing"`
}

// processor exposes the host's virtualization extensions to every guest, so
// a guest can run its own hypervisor: WSL 2, Hyper-V, Windows Sandbox, and
// Docker Desktop all need one. It rules out dynamic memory, which these VMs
// do not use: their memory is Virtual-backed, committed as the guest touches
// it, not taken at boot.
type processor struct {
	Count                          int  `json:"Count"`
	ExposeVirtualizationExtensions bool `json:"ExposeVirtualizationExtensions"`
}

type devices struct {
	NetworkAdapters map[string]networkAdapter `json:"NetworkAdapters,omitempty"`
	Scsi            map[string]scsi           `json:"Scsi"`
	HvSocket        hvSocket                  `json:"HvSocket"`
	VideoMonitor    *videoMonitor             `json:"VideoMonitor,omitempty"`
	Keyboard        *struct{}                 `json:"Keyboard,omitempty"`
	Mouse           *struct{}                 `json:"Mouse,omitempty"`
}

// videoMonitor is the VM's emulated video card: the guest's display adapter,
// whose desktop the agent captures for a viewer (guest.ServeDisplay). The VM
// worker could serve it as RDP on a named pipe, but only on some host
// editions, so disco-vm does not ask it to.
type videoMonitor struct {
	HorizontalResolution int `json:"HorizontalResolution"`
	VerticalResolution   int `json:"VerticalResolution"`
}

type networkAdapter struct {
	EndpointID string `json:"EndpointId"`
	MacAddress string `json:"MacAddress"`
}

type scsi struct {
	Attachments map[string]attachment `json:"Attachments"`
}

type attachment struct {
	Type string `json:"Type"`
	Path string `json:"Path"`
}

type hvSocket struct {
	HvSocketConfig hvSocketConfig `json:"HvSocketConfig"`
}

type hvSocketConfig struct {
	DefaultBindSecurityDescriptor    string                     `json:"DefaultBindSecurityDescriptor"`
	DefaultConnectSecurityDescriptor string                     `json:"DefaultConnectSecurityDescriptor"`
	ServiceTable                     map[string]hvSocketService `json:"ServiceTable"`
}

type hvSocketService struct {
	BindSecurityDescriptor    string `json:"BindSecurityDescriptor"`
	ConnectSecurityDescriptor string `json:"ConnectSecurityDescriptor"`
	AllowWildcardBinds        bool   `json:"AllowWildcardBinds"`
}

type guestState struct {
	GuestStateFilePath   string `json:"GuestStateFilePath"`
	RuntimeStateFilePath string `json:"RuntimeStateFilePath"`
}

const (
	// microsoftWindowsTemplate is the standard Secure Boot template, which a
	// stock Windows bootloader validates against.
	microsoftWindowsTemplate = "1734c6e8-3154-4dda-ba5f-a874cc483422"
	// sddlEveryone lets the guest bind as whatever it runs as.
	sddlEveryone = "D:P(A;;FA;;;WD)"
	// sddlAdmins is who on the host may connect: the agent runs anything as
	// SYSTEM in the guest, so a non-admin host user must not reach it.
	sddlAdmins = "D:P(A;;FA;;;BA)(A;;FA;;;SY)"

	defaultCPUs     = 6 // 1 vCPU was unusably slow in sandboxi
	defaultMemoryMB = 4096
	// The video adapter's resolution at boot; a viewer asks the guest for
	// its own size once it is open.
	displayWidth, displayHeight = 1920, 1080
)

// vmConfig is what varies between the documents this driver creates.
type vmConfig struct {
	Disk      string
	GuestFile string
	StateFile string
	CPUs      int
	Memory    uint64 // bytes
	// NIC is attached at create.
	NIC *endpoint
}

// hvsockPorts are the guest ports every VM's ServiceTable lists; the guest's
// bind is refused for any port not listed.
var hvsockPorts = []uint32{guest.AgentPort, guest.DisplayPort, guest.ServicePort}

func slashes(path string) string { return strings.ReplaceAll(path, `\`, "/") }

func newDocument(c vmConfig) document {
	cpus, mb := c.CPUs, c.Memory/(1<<20)
	if cpus <= 0 {
		cpus = defaultCPUs
	}
	if mb == 0 {
		mb = defaultMemoryMB
	}
	services := map[string]hvSocketService{}
	for _, port := range hvsockPorts {
		id := winio.VsockServiceID(port)
		services[strings.ToUpper(id.String())] = hvSocketService{
			BindSecurityDescriptor: sddlEveryone, ConnectSecurityDescriptor: sddlAdmins, AllowWildcardBinds: true,
		}
	}
	d := document{
		Owner:                             "disco-vm",
		SchemaVersion:                     schemaVersion{Major: 2, Minor: 4},
		ShouldTerminateOnLastHandleClosed: true,
		VirtualMachine: virtualMachine{
			StopOnReset: false,
			Chipset:     chipset{Uefi: uefi{ApplySecureBootTemplate: "Apply", SecureBootTemplateID: microsoftWindowsTemplate}},
			ComputeTopology: topology{
				Memory:    memory{SizeInMB: mb, Backing: "Virtual"},
				Processor: processor{Count: cpus, ExposeVirtualizationExtensions: true},
			},
			Devices: devices{
				Scsi: map[string]scsi{"0": {Attachments: map[string]attachment{"0": {Type: "VirtualDisk", Path: slashes(c.Disk)}}}},
				HvSocket: hvSocket{HvSocketConfig: hvSocketConfig{
					DefaultBindSecurityDescriptor:    sddlEveryone,
					DefaultConnectSecurityDescriptor: sddlAdmins,
					ServiceTable:                     services,
				}},
			},
			GuestState: guestState{GuestStateFilePath: slashes(c.GuestFile), RuntimeStateFilePath: slashes(c.StateFile)},
		},
	}
	video := &videoMonitor{HorizontalResolution: displayWidth, VerticalResolution: displayHeight}
	d.VirtualMachine.Devices.VideoMonitor, d.VirtualMachine.Devices.Keyboard, d.VirtualMachine.Devices.Mouse = video, &struct{}{}, &struct{}{}
	if c.NIC != nil {
		d.VirtualMachine.Devices.NetworkAdapters = map[string]networkAdapter{c.NIC.ID: {EndpointID: c.NIC.ID, MacAddress: c.NIC.MAC}}
	}
	return d
}
