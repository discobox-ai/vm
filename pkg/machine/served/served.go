// Package served is a driver served over a stream, and the driver that is
// its client (discobox ADR 26-11-11-963 §3).
//
// An embedder that holds a driver (discobox's server, over vz or libkrun)
// serves it with Handler to a caller it cannot hand the driver itself (a
// pool agent in a VM), on a channel the embedder authenticates. The caller
// opens New on that channel and gets a machine.Driver, which it opens an
// engine on as it would any driver. The embedder decides what the caller may
// do:
//
//   - **The driver's scope.** The served driver is made for one pool, and
//     every name a call carries is resolved in it, by the server: a caller
//     names VMs, never IDs or paths, so it reaches nothing outside the pool.
//   - **An allow-list** of operations (Op) and of the fields a create may
//     set (Field). Anything not listed is refused (ErrRefused), and so is a
//     field the protocol does not know. Capabilities are reported narrowed
//     to what is allowed, so an engine on the client refuses up front what
//     the server would.
//   - **A hook on every create** (Options.Create), where the embedder
//     forces what it must (no network device, a size within its bounds, a
//     forward of its own) or refuses the spec.
//   - **A version.** Every request carries the client's, and one that is not
//     the server's is refused with a *VersionError naming both, before
//     anything else is read.
//
// The protocol is HTTP/1.1, one connection per call, as the agent's is
// (pkg/guest): the client opens a new stream for each call with the dialer
// it is given, and the connection carries nothing past it. A guest
// operation, and a dial of a guest port, upgrade their connection, as a
// shim's control API does (pkg/machine/local):
//
//	GET    /v1/driver               the driver's name, narrowed capabilities, and allow-list
//	POST   /v1/check                Driver.Check
//	GET    /v1/vms                  Driver.Instances
//	POST   /v1/vms                  Driver.Create of a createRequest
//	GET    /v1/vms/{name}           Driver.Instance
//	DELETE /v1/vms/{name}           Driver.Destroy
//	POST   /v1/vms/{name}/start     Driver.Start
//	GET    /v1/vms/{name}/state     whether Driver.Attach finds it running
//	POST   /v1/vms/{name}/kill      Machine.Kill
//	POST   /v1/vms/{name}/commit    Driver.Commit
//	POST   /v1/vms/{name}/upgrade   Driver.Upgrade, from a VM named in the body
//	GET    /v1/vms/{name}/service   Driver.Service
//	GET    /v1/vms/{name}/guest     101 disco-vm-guest, then the agent's protocol (guest.Relay)
//	GET    /v1/vms/{name}/dial      ?port=N → 101 disco-vm-dial, then the guest port's stream
//
// What is never served: Install, a boot in the serving process (Boot), a
// window, host directories, caches, and keys. The client's Boot is its Start,
// as boxd's is: the machine is the server's, and outlives the client.
package served

import (
	"errors"
	"fmt"
	"slices"

	"github.com/discobox-ai/vm/pkg/machine"
)

// Op is an operation an embedder allows a client (Options.Allow). Each
// names a group of the driver's calls, in the terms of the engine that uses
// them.
type Op string

const (
	// OpCapabilities is the driver's name and capabilities, and its Check.
	// A client asks for them when it is made, so every list needs it.
	OpCapabilities Op = "capabilities"
	// OpCreate makes a VM (Driver.Create), with the fields Options.Fields
	// allows.
	OpCreate Op = "create"
	// OpGet finds a VM by name (Driver.Instance).
	OpGet Op = "get"
	// OpList lists the pool's VMs (Driver.Instances).
	OpList Op = "list"
	// OpRemove removes a VM (Driver.Destroy).
	OpRemove Op = "remove"
	// OpStart starts a VM that outlives the call (Driver.Start).
	OpStart Op = "start"
	// OpState is whether a VM runs (Driver.Attach), which is also how a
	// client watches its machine stop.
	OpState Op = "state"
	// OpStop stops a running VM: in order (Guest.Shutdown), or forced
	// (Machine.Kill).
	OpStop Op = "stop"
	// OpService is where a VM's service listens, and its URLs
	// (Driver.Service).
	OpService Op = "service"
	// OpExec runs a process in a running VM (Guest.Exec).
	OpExec Op = "exec"
	// OpCopy copies files into and out of a running VM (Guest.CopyTo,
	// Guest.CopyFrom).
	OpCopy Op = "copy"
	// OpInfo describes a running VM's guest (Guest.Info), and is how a
	// client checks its agent's health.
	OpInfo Op = "info"
	// OpDial opens a stream to a running VM's agent port or service port
	// (Machine.Dial), which disco-vm limits to those two.
	OpDial Op = "dial"
	// OpCommit commits a stopped VM (Driver.Commit). The client reports
	// Capabilities.Commit only where FieldFrom is allowed too.
	OpCommit Op = "commit"
	// OpUpgrade gives a stopped VM a new root (Driver.Upgrade).
	OpUpgrade Op = "upgrade"
)

// Ops is every operation, in the order the protocol lists them.
var Ops = []Op{
	OpCapabilities, OpCreate, OpGet, OpList, OpRemove, OpStart, OpState, OpStop,
	OpService, OpExec, OpCopy, OpInfo, OpDial, OpCommit, OpUpgrade,
}

// Field is a field of machine.InstanceSpec a client may set at create
// (Options.Fields), past its name, which every create has.
type Field string

const (
	// FieldFrom is InstanceSpec.From: a committed VM of the pool, by name.
	FieldFrom Field = "from"
	// FieldCPUs is InstanceSpec.CPUs.
	FieldCPUs Field = "cpus"
	// FieldMemory is InstanceSpec.Memory.
	FieldMemory Field = "memory"
	// FieldForwards is InstanceSpec.Forwards.
	FieldForwards Field = "forwards"
	// FieldIsolated is InstanceSpec.Isolated.
	FieldIsolated Field = "isolated"
	// FieldNetworks is InstanceSpec.Networks.
	FieldNetworks Field = "networks"
	// FieldEgress is InstanceSpec.Egress.
	FieldEgress Field = "egress"
	// FieldData is InstanceSpec.Data.
	FieldData Field = "data"
)

// Fields is every field a create can carry. A cache volume is not one:
// caches are not served.
var Fields = []Field{
	FieldFrom, FieldCPUs, FieldMemory, FieldForwards, FieldIsolated, FieldNetworks, FieldEgress, FieldData,
}

// ErrRefused reports a call, or a field of one, that the embedder's
// allow-list does not list.
var ErrRefused = errors.New("served: not allowed by the embedder")

// ErrVersion reports a client whose version is not the server's; the error
// is a *VersionError.
var ErrVersion = errors.New("served: the client's version is not the server's")

// VersionError is a call refused because the client's version is not the
// server's. An embedder reports both, to say which side to upgrade.
type VersionError struct {
	Server string `json:"server"`
	Client string `json:"client"`
}

func (e *VersionError) Error() string {
	return fmt.Sprintf("served: the server's engine is version %s, and this client's %s; upgrade the client to %s", e.Server, e.Client, e.Server)
}

// Is makes a *VersionError match ErrVersion.
func (e *VersionError) Is(target error) bool { return target == ErrVersion }

// VersionHeader carries the client's version on every request.
const VersionHeader = "Disco-Vm-Version"

// driverInfo is GET /v1/driver: what the client reports as its own.
type driverInfo struct {
	Name         string               `json:"name"`
	Capabilities machine.Capabilities `json:"capabilities"`
	Allow        []Op                 `json:"allow"`
	Fields       []Field              `json:"fields,omitempty"`
}

// createRequest is POST /v1/vms. Each field but Name is a Field, and a
// field the server does not know is refused.
type createRequest struct {
	Name     string              `json:"name"`
	From     string              `json:"from,omitempty"`
	CPUs     int                 `json:"cpus,omitempty"`
	Memory   uint64              `json:"memory,omitempty"`
	Forwards []machine.Forward   `json:"forwards,omitempty"`
	Isolated bool                `json:"isolated,omitempty"`
	Networks []string            `json:"networks,omitempty"`
	Egress   *machine.Egress     `json:"egress,omitempty"`
	Data     *machine.DataVolume `json:"data,omitempty"`
}

// set lists the fields the request sets.
func (r createRequest) set() []Field {
	var set []Field
	add := func(f Field, ok bool) {
		if ok {
			set = append(set, f)
		}
	}
	add(FieldFrom, r.From != "")
	add(FieldCPUs, r.CPUs != 0)
	add(FieldMemory, r.Memory != 0)
	add(FieldForwards, len(r.Forwards) > 0)
	add(FieldIsolated, r.Isolated)
	add(FieldNetworks, len(r.Networks) > 0)
	add(FieldEgress, r.Egress != nil)
	add(FieldData, r.Data != nil)
	return set
}

// upgradeRequest is POST /v1/vms/{name}/upgrade.
type upgradeRequest struct {
	// From names the committed VM the new root is a copy of; empty is a
	// fresh root.
	From string `json:"from,omitempty"`
}

// stateResponse is GET /v1/vms/{name}/state.
type stateResponse struct {
	Running bool `json:"running"`
}

// errorResponse is every refusal and failure: a message, and a code that
// names the sentinel the client wraps.
type errorResponse struct {
	Error   string        `json:"error"`
	Code    string        `json:"code,omitempty"`
	Version *VersionError `json:"version,omitempty"`
}

// sentinels are the errors that cross the stream by code, so a caller's
// errors.Is answers on the client as on the server.
var sentinels = []struct {
	code string
	err  error
}{
	{"refused", ErrRefused},
	{"version", ErrVersion},
	{"not-found", machine.ErrNotFound},
	{"exists", machine.ErrExists},
	{"in-use", machine.ErrInUse},
	{"committed", machine.ErrCommitted},
	{"capacity", machine.ErrCapacity},
	{"not-running", machine.ErrNotRunning},
	{"unsupported", machine.ErrUnsupported},
	{"not-implemented", machine.ErrNotImplemented},
	{"no-key", machine.ErrNoKey},
}

// codeOf is the code of the first sentinel err wraps, or "".
func codeOf(err error) string {
	for _, s := range sentinels {
		if errors.Is(err, s.err) {
			return s.code
		}
	}
	return ""
}

// sentinelOf is the sentinel of a code, or nil.
func sentinelOf(code string) error {
	i := slices.IndexFunc(sentinels, func(s struct {
		code string
		err  error
	}) bool {
		return s.code == code
	})
	if i < 0 {
		return nil
	}
	return sentinels[i].err
}

// remoteError is the server's error on the client: its message, wrapping
// the sentinel its code names.
type remoteError struct {
	msg string
	is  error
}

func (e *remoteError) Error() string { return e.msg }
func (e *remoteError) Unwrap() error { return e.is }

const (
	// guestUpgrade is the Upgrade token of a connection to a VM's guest
	// operations.
	guestUpgrade = "disco-vm-guest"
	// dialUpgrade is the Upgrade token of a connection to a guest port.
	dialUpgrade = "disco-vm-dial"
)
