package hcs

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"sync"
	"sync/atomic"

	"github.com/Microsoft/go-winio"
	"github.com/Microsoft/go-winio/pkg/guid"

	"github.com/discobox-ai/vm/pkg/machine"
)

// vm is a running compute system this process holds. The document sets
// ShouldTerminateOnLastHandleClosed, so the VM lives exactly as long as the
// handle here: a shim that crashes takes its VM with it and cannot orphan it.
type vm struct {
	sys       *system
	runtimeID guid.GUID
	// nic is deleted once the VM is gone.
	nic *endpoint

	done       chan struct{}
	exitOnce   sync.Once
	err        error
	killed     atomic.Bool
	unregister func()
}

var _ machine.Machine = (*vm)(nil)

// startVM creates and starts a compute system from c. Its files must already
// be granted to id. On failure nothing is left running, and c.NIC is freed.
func startVM(id string, c vmConfig) (*vm, error) {
	sys, err := createSystem(id, newDocument(c))
	if err != nil {
		if c.NIC != nil {
			deleteEndpoint(c.NIC.ID)
		}
		return nil, err
	}
	m, err := watch(sys, c.NIC)
	if err != nil {
		return nil, err
	}
	if err := sys.start(); err != nil {
		m.shutdown()
		return nil, err
	}
	if err := m.readRuntimeID(); err != nil {
		m.shutdown()
		return nil, err
	}
	return m, nil
}

// watch makes sys a vm and subscribes to its exit. It owns sys and nic from
// here on, including on error.
func watch(sys *system, nic *endpoint) (*vm, error) {
	m := &vm{sys: sys, nic: nic, done: make(chan struct{})}
	unregister, err := sys.onEvent(m.event)
	if err != nil {
		m.exited(err)
		return nil, err
	}
	m.unregister = unregister
	return m, nil
}

func (m *vm) readRuntimeID() error {
	props, err := m.sys.properties()
	if err != nil {
		return err
	}
	if m.runtimeID, err = guid.FromString(props.RuntimeID); err != nil {
		return fmt.Errorf("hcs: system %s has no runtime ID (%q): %w", m.sys.id, props.RuntimeID, err)
	}
	return nil
}

// event is HCS telling us about the system, on HCS's thread pool.
func (m *vm) event(typ uint32, data string) {
	switch typ {
	case eventSystemExited:
		var status struct {
			Status   int32
			ExitType string
		}
		_ = json.Unmarshal([]byte(data), &status)
		var err error
		if status.Status != 0 || status.ExitType == "UnexpectedExit" {
			err = fmt.Errorf("hcs: the VM stopped unexpectedly: %s", data)
		}
		go m.exited(err)
	case eventServiceDisconnect:
		go m.exited(errors.New("hcs: lost the connection to vmcompute"))
	}
}

// exited releases everything the VM held and closes done, once.
func (m *vm) exited(err error) {
	m.exitOnce.Do(func() {
		if m.killed.Load() {
			err = nil
		}
		m.err = err
		if m.unregister != nil {
			m.unregister()
		}
		m.sys.close()
		if m.nic != nil {
			deleteEndpoint(m.nic.ID)
		}
		close(m.done)
	})
}

// shutdown terminates the VM and waits for it to be released.
func (m *vm) shutdown() {
	select {
	case <-m.done:
		return
	default:
	}
	m.killed.Store(true)
	if err := m.sys.terminate(); err != nil {
		// Already gone, or it never started: either way nothing will report
		// an exit, so release it here.
		m.exited(nil)
	}
	<-m.done
}

func (m *vm) Dial(ctx context.Context, port uint32) (net.Conn, error) {
	select {
	case <-m.done:
		return nil, errors.New("hcs: the VM is stopped")
	default:
	}
	return winio.Dial(ctx, &winio.HvsockAddr{VMID: m.runtimeID, ServiceID: winio.VsockServiceID(port)})
}

func (m *vm) Kill(context.Context) error {
	m.shutdown()
	return nil
}

func (m *vm) Done() <-chan struct{} { return m.done }

func (m *vm) Err() error {
	<-m.done
	return m.err
}
