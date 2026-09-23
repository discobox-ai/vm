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
	StopOnReset     bool          `json:"StopOnReset"`
	Chipset         chipset       `json:"Chipset"`
	ComputeTopology topology      `json:"ComputeTopology"`
	Devices         devices       `json:"Devices"`
	GuestState      guestState    `json:"GuestState"`
	RestoreState    *restoreState `json:"RestoreState,omitempty"`
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

type processor struct {
	Count int `json:"Count"`
}

type devices struct {
	NetworkAdapters map[string]networkAdapter `json:"NetworkAdapters,omitempty"`
	Scsi            map[string]scsi           `json:"Scsi"`
	HvSocket        hvSocket                  `json:"HvSocket"`
	VideoMonitor    *videoMonitor             `json:"VideoMonitor,omitempty"`
	Keyboard        *struct{}                 `json:"Keyboard,omitempty"`
	Mouse           *struct{}                 `json:"Mouse,omitempty"`
}

// videoMonitor is the VM's emulated video card, served by the VM worker as
// RDP (standard RDP security only) on a named pipe that only AccessSids may
// open. It shows the guest from power-on, Setup and blue screens included,
// with nothing running in the guest. Keyboard and Mouse are where it delivers
// input; without them the console shows the screen and drops every key.
type videoMonitor struct {
	HorizontalResolution int                `json:"HorizontalResolution"`
	VerticalResolution   int                `json:"VerticalResolution"`
	ConnectionOptions    *consoleConnection `json:"ConnectionOptions,omitempty"`
}

type consoleConnection struct {
	AccessSids []string `json:"AccessSids"`
	NamedPipe  string   `json:"NamedPipe"`
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

type restoreState struct {
	TemplateSystemID string `json:"TemplateSystemId"`
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
)

// vmConfig is what varies between the documents this driver creates.
type vmConfig struct {
	Disk      string
	GuestFile string
	StateFile string
	CPUs      int
	Memory    uint64 // bytes
	// NIC is attached at create for a cold boot. A fork's memory has no
	// adapter in it, so a clone gets its NIC hot-added after start instead.
	NIC *endpoint
	// Template makes a fork clone of the named template system.
	Template string
	// Console serves the video console on this pipe to ConsoleSID. Every VM
	// has the video devices, so a template and its clones agree on them.
	Console    string
	ConsoleSID string
}

// hvsockPorts are the guest ports listed in the ServiceTable. The guest's
// bind is refused for any port not listed. 7301 (display) joins when the
// display lands.
var hvsockPorts = []uint32{guest.AgentPort}

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
				Processor: processor{Count: cpus},
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
	video := &videoMonitor{HorizontalResolution: consoleWidth, VerticalResolution: consoleHeight}
	if c.Console != "" && c.ConsoleSID != "" {
		video.ConnectionOptions = &consoleConnection{AccessSids: []string{c.ConsoleSID}, NamedPipe: c.Console}
	}
	d.VirtualMachine.Devices.VideoMonitor, d.VirtualMachine.Devices.Keyboard, d.VirtualMachine.Devices.Mouse = video, &struct{}{}, &struct{}{}
	if c.NIC != nil {
		d.VirtualMachine.Devices.NetworkAdapters = map[string]networkAdapter{c.NIC.ID: {EndpointID: c.NIC.ID, MacAddress: c.NIC.MAC}}
	}
	if c.Template != "" {
		d.VirtualMachine.RestoreState = &restoreState{TemplateSystemID: c.Template}
	}
	return d
}

// addNIC is the modify request that hot-adds a NIC to a running system.
func addNIC(nic *endpoint) any {
	return map[string]any{
		"ResourcePath": "VirtualMachine/Devices/NetworkAdapters/" + nic.ID,
		"RequestType":  "Add",
		"Settings":     networkAdapter{EndpointID: nic.ID, MacAddress: nic.MAC},
	}
}
