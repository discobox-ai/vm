package boxd

import (
	"context"
	"errors"
	"fmt"
	"net"
	"sync"
	"sync/atomic"
	"time"

	"github.com/discobox-ai/vm/pkg/machine/boxd/internal/boxdapi"
)

// pollEvery is how often a running machine's state is read. boxd has no event
// stream for it, and Done only has to notice a power-off, not race it.
var pollEvery = 2 * time.Second

// killLimit bounds Kill on its own, since its callers pass a context with no
// deadline: past it, Kill gives the machine up rather than hang its shim on an
// API that does not answer.
var killLimit = 5 * time.Minute

// vm is a running boxd machine. Nothing ties it to this process: the shim that
// holds it only watches it.
type vm struct {
	api  boxdapi.BoxdApiClient
	ref  vmRef
	done chan struct{}
	err  error
	once sync.Once
	stop context.CancelFunc

	// poll and killLimit are read once, when the machine is booted.
	poll, killLimit time.Duration

	// mu orders Kill against the watcher restarting a guest that rebooted, so
	// a killed machine is never started again.
	mu     sync.Mutex
	killed atomic.Bool
}

func watch(api boxdapi.BoxdApiClient, ref vmRef) *vm {
	ctx, stop := context.WithCancel(context.Background())
	m := &vm{api: api, ref: ref, done: make(chan struct{}), stop: stop, poll: pollEvery, killLimit: killLimit}
	go m.watch(ctx)
	return m
}

func (m *vm) watch(ctx context.Context) {
	for {
		select {
		case <-ctx.Done():
			return
		case <-time.After(m.poll):
		}
		call, cancel := context.WithTimeout(ctx, 30*time.Second)
		info, err := m.api.GetVm(call, &boxdapi.GetVmRequest{VmId: m.ref.ID})
		cancel()
		switch {
		case isNotFound(err):
			m.finish(fmt.Errorf("boxd: machine %s was destroyed", m.ref.Name))
			return
		case err != nil:
			// The API being unreachable says nothing about the guest.
			continue
		}
		switch info.GetStatus() {
		case "stopped":
			m.finish(nil)
			return
		case "failed", "destroying":
			m.finish(fmt.Errorf("boxd: machine %s is %s", m.ref.Name, info.GetStatus()))
			return
		case "running":
			// A guest that shut itself down is left running by boxd; the
			// shutdown hook's marker says so, and the driver stops it.
			probe, cancel := context.WithTimeout(ctx, 30*time.Second)
			how := halted(probe, m.api, m.ref.ID)
			cancel()
			if err := m.afterHalt(ctx, how); err != nil {
				m.finish(err)
				return
			}
		}
	}
}

// afterHalt finishes what a guest's own shutdown asked for: a power-off stops
// the machine, and a reboot stops and starts it again.
func (m *vm) afterHalt(ctx context.Context, how string) error {
	if how == "" {
		return nil
	}
	if _, err := m.api.StopVm(ctx, &boxdapi.StopVmRequest{VmId: m.ref.ID}); err != nil && !isNotFound(err) {
		return fmt.Errorf("boxd: stop %s after the guest's %s: %w", m.ref.Name, how, err)
	}
	if how != "reboot" {
		return nil // the next poll sees it stopped
	}
	stopped, cancel := context.WithTimeout(ctx, 2*time.Minute)
	_, err := waitVM(stopped, m.api, m.ref.ID, "stopped")
	cancel()
	if err != nil {
		return err
	}
	// Held across the start, which is bounded, so Kill waits at most that
	// long for it and then stops the machine it started.
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.killed.Load() {
		return nil // stays stopped; the next poll finishes it
	}
	call, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	if _, err := m.api.StartVm(call, &boxdapi.StartVmRequest{VmId: m.ref.ID}); err != nil {
		return fmt.Errorf("boxd: start %s after the guest's reboot: %w", m.ref.Name, err)
	}
	return nil
}

func (m *vm) finish(err error) {
	m.once.Do(func() {
		if !m.killed.Load() {
			m.err = err
		}
		m.stop()
		close(m.done)
	})
}

// abandon finishes the machine without knowing that it stopped, with an error
// that says so, whether or not it was killed.
func (m *vm) abandon(err error) {
	m.once.Do(func() {
		m.err = err
		m.stop()
		close(m.done)
	})
}

func (m *vm) Dial(ctx context.Context, port uint32) (net.Conn, error) {
	select {
	case <-m.done:
		return nil, errors.New("boxd: guest is stopped")
	default:
	}
	return dial(ctx, m.api, m.ref.ID, port)
}

// Kill stops the machine through the API, retrying until boxd takes the
// request, and waits for the watcher to see it stopped. A caller's context
// ending returns its error and leaves the machine be. killLimit ending means
// boxd cannot be reached or will not stop it: Kill gives the machine up, so
// Done closes and Err says it may still be running. The instance still names
// the machine, so removing the instance destroys it later.
func (m *vm) Kill(ctx context.Context) error {
	limit, cancel := context.WithTimeout(context.Background(), m.killLimit)
	defer cancel()
	m.mu.Lock()
	m.killed.Store(true)
	m.mu.Unlock()

	var last error
	for stopped := false; !stopped; {
		select {
		case <-m.done:
			return nil
		default:
		}
		call, cancelCall := context.WithTimeout(ctx, 30*time.Second)
		_, err := m.api.StopVm(call, &boxdapi.StopVmRequest{VmId: m.ref.ID})
		cancelCall()
		if stopped = err == nil || isNotFound(err); !stopped {
			last = err
			select {
			case <-m.done:
				return nil
			case <-ctx.Done():
				return ctx.Err()
			case <-limit.Done():
				return m.giveUp(fmt.Errorf("boxd could not be asked to stop it: %w", last))
			case <-time.After(2 * time.Second):
			}
		}
	}
	select {
	case <-m.done:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	case <-limit.Done():
		return m.giveUp(fmt.Errorf("it was not seen stopped within %s", m.killLimit))
	}
}

func (m *vm) giveUp(why error) error {
	err := fmt.Errorf("boxd: kill %s: %w; it may still be running, and removing the instance destroys it", m.ref.Name, why)
	m.abandon(err)
	return err
}

func (m *vm) Done() <-chan struct{} { return m.done }

func (m *vm) Err() error {
	<-m.done
	return m.err
}
