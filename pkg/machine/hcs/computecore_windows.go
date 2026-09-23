package hcs

import (
	"encoding/json"
	"errors"
	"fmt"
	"runtime"
	"sync"
	"syscall"
	"unsafe"

	"golang.org/x/sys/windows"
)

// computecore.dll is the public HCS API (computecore.h in the SDK). Every
// mutating call is asynchronous around an operation handle: create one, start
// the call, then block in HcsWaitForOperationResult for its JSON result, which
// carries HCS's error detail on failure. run wraps that dance.
var (
	computecore = windows.NewLazySystemDLL("computecore.dll")

	procHcsCreateOperation             = computecore.NewProc("HcsCreateOperation")
	procHcsCloseOperation              = computecore.NewProc("HcsCloseOperation")
	procHcsWaitForOperationResult      = computecore.NewProc("HcsWaitForOperationResult")
	procHcsEnumerateComputeSystems     = computecore.NewProc("HcsEnumerateComputeSystems")
	procHcsCreateComputeSystem         = computecore.NewProc("HcsCreateComputeSystem")
	procHcsOpenComputeSystem           = computecore.NewProc("HcsOpenComputeSystem")
	procHcsCloseComputeSystem          = computecore.NewProc("HcsCloseComputeSystem")
	procHcsStartComputeSystem          = computecore.NewProc("HcsStartComputeSystem")
	procHcsTerminateComputeSystem      = computecore.NewProc("HcsTerminateComputeSystem")
	procHcsPauseComputeSystem          = computecore.NewProc("HcsPauseComputeSystem")
	procHcsSaveComputeSystem           = computecore.NewProc("HcsSaveComputeSystem")
	procHcsModifyComputeSystem         = computecore.NewProc("HcsModifyComputeSystem")
	procHcsGetComputeSystemProperties  = computecore.NewProc("HcsGetComputeSystemProperties")
	procHcsSetComputeSystemCallback    = computecore.NewProc("HcsSetComputeSystemCallback")
	procHcsGrantVMAccess               = computecore.NewProc("HcsGrantVmAccess")
	procHcsRevokeVMAccess              = computecore.NewProc("HcsRevokeVmAccess")
	procHcsCreateEmptyGuestStateFile   = computecore.NewProc("HcsCreateEmptyGuestStateFile")
	procHcsCreateEmptyRuntimeStateFile = computecore.NewProc("HcsCreateEmptyRuntimeStateFile")
)

const (
	infinite = 0xFFFFFFFF
	// genericAll is the access HcsOpenComputeSystem accepts; GENERIC_READ is
	// refused with E_INVALIDARG even for a properties query.
	genericAll = 0x10000000

	eAccessDenied = 0x80070005
	// eResources is ERROR_NO_SYSTEM_RESOURCES as an HRESULT: a save as
	// template without the commit headroom to materialize guest memory.
	eResources = 0x800705AA
)

// hcsError is a failed HCS call with the result document HCS explained it in.
type hcsError struct {
	Op     string
	HR     uint32
	Detail string
}

func (e *hcsError) Error() string {
	msg := fmt.Sprintf("%s: HRESULT 0x%08X", e.Op, e.HR)
	if e.HR == eAccessDenied {
		msg += " (access denied: run elevated)"
	}
	if e.Detail != "" {
		msg += ": " + e.Detail
	}
	return msg
}

func hresultOf(err error) uint32 {
	var he *hcsError
	if errors.As(err, &he) {
		return he.HR
	}
	return 0
}

// errorDetail pulls the human part out of an HCS result document, which is
// JSON ({"Error":..., "ErrorMessage":..., "ErrorEvents":[...]}) or empty.
func errorDetail(doc string) string {
	if doc == "" {
		return ""
	}
	var r struct {
		ErrorMessage string
		ErrorEvents  []struct{ Message string }
	}
	if json.Unmarshal([]byte(doc), &r) != nil {
		return doc
	}
	msg := r.ErrorMessage
	for _, ev := range r.ErrorEvents {
		if ev.Message != "" && ev.Message != msg {
			msg += "; " + ev.Message
		}
	}
	if msg == "" {
		return doc
	}
	return msg
}

func utf16(s string) *uint16 {
	if s == "" {
		return nil
	}
	p, err := windows.UTF16PtrFromString(s)
	if err != nil {
		panic(err) // a NUL in a string we built ourselves
	}
	return p
}

// run drives one asynchronous HCS call to completion and returns its result
// document.
func run(op string, start func(operation uintptr) uintptr) (string, error) {
	operation, _, _ := procHcsCreateOperation.Call(0, 0)
	if operation == 0 {
		return "", &hcsError{Op: op, Detail: "HcsCreateOperation returned null"}
	}
	defer procHcsCloseOperation.Call(operation)

	startHR := uint32(start(operation))
	var result *uint16
	waitHR, _, _ := procHcsWaitForOperationResult.Call(operation, infinite, uintptr(unsafe.Pointer(&result)))
	doc := ""
	if result != nil {
		doc = windows.UTF16PtrToString(result)
		_, _ = windows.LocalFree(windows.Handle(unsafe.Pointer(result)))
	}
	hr := startHR
	if hr == 0 {
		hr = uint32(waitHR)
	}
	if hr != 0 {
		return doc, &hcsError{Op: op, HR: hr, Detail: errorDetail(doc)}
	}
	return doc, nil
}

// call calls a synchronous function that returns an HRESULT. Like
// LazyProc.Call, its arguments may be pointers converted in the call.
//
//go:uintptrescapes
func call(op string, p *windows.LazyProc, args ...uintptr) error {
	if hr, _, _ := p.Call(args...); hr != 0 {
		return &hcsError{Op: op, HR: uint32(hr)}
	}
	return nil
}

// call2 calls a synchronous function that takes only strings.
func call2(op string, p *windows.LazyProc, args ...string) error {
	ptrs := make([]*uint16, len(args))
	raw := make([]uintptr, len(args))
	for i, arg := range args {
		ptrs[i] = utf16(arg)
		raw[i] = uintptr(unsafe.Pointer(ptrs[i]))
	}
	err := call(op, p, raw...)
	runtime.KeepAlive(ptrs)
	return err
}

// system is an open handle on a compute system. A template can only be forked
// through the handle, in the process, that created it, so the handle is kept
// for as long as the system is ours.
//
// HCS frees a handle's memory on close, so every use takes mu and a closed
// system refuses calls instead of passing a dangling handle.
type system struct {
	id     string
	mu     sync.RWMutex
	handle uintptr
}

var errClosed = errors.New("hcs: the system handle is closed")

// createSystem creates (but does not start) a compute system.
func createSystem(id string, doc any) (*system, error) {
	config, err := json.Marshal(doc)
	if err != nil {
		return nil, err
	}
	var handle uintptr
	idp, configp := utf16(id), utf16(string(config))
	_, err = run("HcsCreateComputeSystem", func(op uintptr) uintptr {
		hr, _, _ := procHcsCreateComputeSystem.Call(
			uintptr(unsafe.Pointer(idp)), uintptr(unsafe.Pointer(configp)),
			op, 0, uintptr(unsafe.Pointer(&handle)))
		return hr
	})
	runtime.KeepAlive(idp)
	runtime.KeepAlive(configp)
	if err != nil {
		if handle != 0 {
			procHcsCloseComputeSystem.Call(handle)
		}
		return nil, err
	}
	return &system{id: id, handle: handle}, nil
}

// openSystem opens an existing compute system by ID.
func openSystem(id string) (*system, error) {
	var handle uintptr
	idp := utf16(id)
	err := call("HcsOpenComputeSystem", procHcsOpenComputeSystem,
		uintptr(unsafe.Pointer(idp)), genericAll, uintptr(unsafe.Pointer(&handle)))
	runtime.KeepAlive(idp)
	if err != nil {
		return nil, err
	}
	return &system{id: id, handle: handle}, nil
}

func (s *system) op(name string, p *windows.LazyProc, options string) (string, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	if s.handle == 0 {
		return "", errClosed
	}
	optp := utf16(options)
	defer runtime.KeepAlive(optp)
	return run(name, func(op uintptr) uintptr {
		hr, _, _ := p.Call(s.handle, op, uintptr(unsafe.Pointer(optp)))
		return hr
	})
}

func (s *system) start() error {
	_, err := s.op("HcsStartComputeSystem", procHcsStartComputeSystem, "")
	return err
}

func (s *system) terminate() error {
	_, err := s.op("HcsTerminateComputeSystem", procHcsTerminateComputeSystem, "")
	return err
}

func (s *system) pause(options string) error {
	_, err := s.op("HcsPauseComputeSystem", procHcsPauseComputeSystem, options)
	return err
}

func (s *system) save(options string) error {
	_, err := s.op("HcsSaveComputeSystem", procHcsSaveComputeSystem, options)
	return err
}

func (s *system) modify(request any) error {
	config, err := json.Marshal(request)
	if err != nil {
		return err
	}
	s.mu.RLock()
	defer s.mu.RUnlock()
	if s.handle == 0 {
		return errClosed
	}
	configp := utf16(string(config))
	_, err = run("HcsModifyComputeSystem", func(op uintptr) uintptr {
		hr, _, _ := procHcsModifyComputeSystem.Call(s.handle, op, uintptr(unsafe.Pointer(configp)), 0)
		return hr
	})
	runtime.KeepAlive(configp)
	return err
}

// properties is the part of the basic property set this driver reads.
type properties struct {
	ID        string `json:"Id"`
	State     string `json:"State"`
	RuntimeID string `json:"RuntimeId"`
	Owner     string `json:"Owner"`
}

func (s *system) properties() (properties, error) {
	var props properties
	doc, err := s.op("HcsGetComputeSystemProperties", procHcsGetComputeSystemProperties, "{}")
	if err != nil {
		return props, err
	}
	err = json.Unmarshal([]byte(doc), &props)
	return props, err
}

func (s *system) close() {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.handle != 0 {
		procHcsCloseComputeSystem.Call(s.handle)
		s.handle = 0
	}
}

// hcsEvent is HCS_EVENT.
type hcsEvent struct {
	Type      uint32
	EventData *uint16
	Operation uintptr
}

const (
	eventSystemExited      = 0x00000001
	eventServiceDisconnect = 0x02000000
)

// Callbacks from HCS arrive on its thread pool. Go allows only a bounded
// number of callbacks to be created, so there is one, and the context HCS
// hands back is a key into this table.
var (
	callbackOnce sync.Once
	callbackPtr  uintptr
	callbackMu   sync.Mutex
	callbacks    = map[uintptr]func(typ uint32, data string){}
	callbackNext uintptr
)

func eventCallback(event *hcsEvent, context uintptr) uintptr {
	callbackMu.Lock()
	fn := callbacks[context]
	callbackMu.Unlock()
	if fn != nil && event != nil {
		data := ""
		if event.EventData != nil {
			data = windows.UTF16PtrToString(event.EventData)
		}
		fn(event.Type, data)
	}
	return 0
}

// onEvent registers fn for the system's events and returns a func that
// unregisters it.
func (s *system) onEvent(fn func(typ uint32, data string)) (func(), error) {
	callbackOnce.Do(func() { callbackPtr = syscall.NewCallback(eventCallback) })
	callbackMu.Lock()
	callbackNext++
	key := callbackNext
	callbacks[key] = fn
	callbackMu.Unlock()
	const optionNone = 0
	s.mu.RLock()
	err := call("HcsSetComputeSystemCallback", procHcsSetComputeSystemCallback, s.handle, optionNone, key, callbackPtr)
	s.mu.RUnlock()
	if err != nil {
		callbackMu.Lock()
		delete(callbacks, key)
		callbackMu.Unlock()
		return nil, err
	}
	return func() {
		callbackMu.Lock()
		delete(callbacks, key)
		callbackMu.Unlock()
	}, nil
}

// listedSystem is one element of HcsEnumerateComputeSystems.
type listedSystem struct {
	ID                string `json:"Id"`
	Owner             string `json:"Owner"`
	State             string `json:"State"`
	RuntimeTemplateID string `json:"RuntimeTemplateId"`
}

func enumerate() ([]listedSystem, error) {
	query := utf16("{}")
	doc, err := run("HcsEnumerateComputeSystems", func(op uintptr) uintptr {
		hr, _, _ := procHcsEnumerateComputeSystems.Call(uintptr(unsafe.Pointer(query)), op)
		return hr
	})
	runtime.KeepAlive(query)
	if err != nil {
		return nil, err
	}
	var out []listedSystem
	if doc == "" {
		return nil, nil
	}
	err = json.Unmarshal([]byte(doc), &out)
	return out, err
}

// grantVMAccess adds an ACE for the VM's isolated SID to path. HCS runs each
// VM under a SID derived from its ID, so every file it opens, every ancestor
// disk included, needs one or Construct fails with a bare access denied.
func grantVMAccess(vmID, path string) error {
	if err := call2("HcsGrantVmAccess", procHcsGrantVMAccess, vmID, path); err != nil {
		return fmt.Errorf("%w: %s", err, path)
	}
	return nil
}

// revokeVMAccess removes what grantVMAccess added, so shared layer disks do
// not collect an ACE for every VM that ever booted over them.
func revokeVMAccess(vmID, path string) {
	_ = call2("HcsRevokeVmAccess", procHcsRevokeVMAccess, vmID, path)
}

func createGuestStateFile(path string) error {
	return call2("HcsCreateEmptyGuestStateFile", procHcsCreateEmptyGuestStateFile, path)
}

func createRuntimeStateFile(path string) error {
	return call2("HcsCreateEmptyRuntimeStateFile", procHcsCreateEmptyRuntimeStateFile, path)
}
