package hcs

import (
	"encoding/json"
	"fmt"
	"runtime"
	"strings"
	"unsafe"

	"golang.org/x/sys/windows"
)

// Guests get a NIC on an HNS network of our own. The Hyper-V "Default Switch"
// is made by vmms, the management stack a Home host does not have; HNS makes
// networks without it, as WSL's own proves. sandboxi found the document that
// works: ICS, with Flags 1 (DNS proxy), 2 (DHCP server) and 32 (a switch of
// our own rather than one layered onto someone else's). Without 2 the guest
// comes up on APIPA and resolves nothing.
const (
	networkID    = "{D15C0B0C-0001-4000-8000-000000000001}"
	networkName  = "disco-vm"
	networkFlags = 1 | 2 | 32
)

var (
	computenetwork = windows.NewLazySystemDLL("computenetwork.dll")

	procHcnOpenNetwork             = computenetwork.NewProc("HcnOpenNetwork")
	procHcnCreateNetwork           = computenetwork.NewProc("HcnCreateNetwork")
	procHcnCloseNetwork            = computenetwork.NewProc("HcnCloseNetwork")
	procHcnCreateEndpoint          = computenetwork.NewProc("HcnCreateEndpoint")
	procHcnQueryEndpointProperties = computenetwork.NewProc("HcnQueryEndpointProperties")
	procHcnCloseEndpoint           = computenetwork.NewProc("HcnCloseEndpoint")
	procHcnDeleteEndpoint          = computenetwork.NewProc("HcnDeleteEndpoint")
)

// hcnResult takes a CoTaskMem string HCN returned, freeing it.
func hcnResult(p *uint16) string {
	if p == nil {
		return ""
	}
	s := windows.UTF16PtrToString(p)
	windows.CoTaskMemFree(unsafe.Pointer(p))
	return s
}

func hcnError(op string, hr uintptr, record *uint16) error {
	return &hcsError{Op: op, HR: uint32(hr), Detail: errorDetail(hcnResult(record))}
}

func mustGUID(s string) windows.GUID {
	g, err := windows.GUIDFromString(s)
	if err != nil {
		panic(err)
	}
	return g
}

// openNetwork opens our network, creating it on first use on this host. The
// network stays between runs: it is idle with nothing attached, and making it
// per VM would churn the guest subnet.
func openNetwork() (uintptr, error) {
	id := mustGUID(networkID)
	var network uintptr
	var record *uint16
	hr, _, _ := procHcnOpenNetwork.Call(uintptr(unsafe.Pointer(&id)), uintptr(unsafe.Pointer(&network)), uintptr(unsafe.Pointer(&record)))
	if hr == 0 {
		hcnResult(record)
		return network, nil
	}
	hcnResult(record)
	settings := fmt.Sprintf(`{"SchemaVersion":{"Major":2,"Minor":0},"Name":%q,"Type":"ICS","Flags":%d}`, networkName, networkFlags)
	settingsp := utf16(settings)
	record = nil
	hr, _, _ = procHcnCreateNetwork.Call(uintptr(unsafe.Pointer(&id)), uintptr(unsafe.Pointer(settingsp)),
		uintptr(unsafe.Pointer(&network)), uintptr(unsafe.Pointer(&record)))
	runtime.KeepAlive(settingsp)
	if hr != 0 {
		return 0, fmt.Errorf("create the %s network (the hns service must be running): %w", networkName, hcnError("HcnCreateNetwork", hr, record))
	}
	hcnResult(record)
	return network, nil
}

// endpoint is a guest NIC: an HNS endpoint and the MAC HNS gave it.
type endpoint struct {
	ID  string `json:"id"`
	MAC string `json:"mac"`
}

// createEndpoint makes a NIC, asking for mac when one is given so a restarted
// guest keeps its adapter instead of installing another. HNS owns the pool and
// may still pick another, so callers use the MAC returned.
func createEndpoint(mac string) (*endpoint, error) {
	network, err := openNetwork()
	if err != nil {
		return nil, err
	}
	defer procHcnCloseNetwork.Call(network)
	id, err := windows.GenerateGUID()
	if err != nil {
		return nil, err
	}
	settings := map[string]any{
		"SchemaVersion":      map[string]int{"Major": 2, "Minor": 0},
		"HostComputeNetwork": strings.Trim(networkID, "{}"),
	}
	if mac != "" {
		settings["MacAddress"] = mac
	}
	doc, _ := json.Marshal(settings)
	docp := utf16(string(doc))
	var ep uintptr
	var record *uint16
	hr, _, _ := procHcnCreateEndpoint.Call(network, uintptr(unsafe.Pointer(&id)), uintptr(unsafe.Pointer(docp)),
		uintptr(unsafe.Pointer(&ep)), uintptr(unsafe.Pointer(&record)))
	runtime.KeepAlive(docp)
	if hr != 0 {
		return nil, hcnError("HcnCreateEndpoint", hr, record)
	}
	hcnResult(record)
	defer procHcnCloseEndpoint.Call(ep)

	query := utf16(`{"SchemaVersion":{"Major":2,"Minor":0}}`)
	var props *uint16
	record = nil
	hr, _, _ = procHcnQueryEndpointProperties.Call(ep, uintptr(unsafe.Pointer(query)), uintptr(unsafe.Pointer(&props)), uintptr(unsafe.Pointer(&record)))
	runtime.KeepAlive(query)
	out := &endpoint{ID: strings.Trim(id.String(), "{}")}
	if hr != 0 {
		deleteEndpoint(out.ID)
		return nil, hcnError("HcnQueryEndpointProperties", hr, record)
	}
	hcnResult(record)
	var p struct{ MacAddress string }
	if err := json.Unmarshal([]byte(hcnResult(props)), &p); err != nil || p.MacAddress == "" {
		deleteEndpoint(out.ID)
		return nil, fmt.Errorf("HNS gave endpoint %s no MAC address", out.ID)
	}
	out.MAC = p.MacAddress
	return out, nil
}

// deleteEndpoint frees a NIC. An endpoint outlives its VM otherwise, and a
// replayed create over one still attached fails with 0x803B0014.
func deleteEndpoint(id string) {
	g, err := windows.GUIDFromString("{" + id + "}")
	if err != nil {
		return
	}
	var record *uint16
	procHcnDeleteEndpoint.Call(uintptr(unsafe.Pointer(&g)), uintptr(unsafe.Pointer(&record)))
	hcnResult(record)
}
