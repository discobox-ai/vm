package docker

import (
	"context"
	"errors"
	"fmt"
	"net"
	"sync"
	"sync/atomic"
	"time"
)

// container is a running guest. Nothing ties it to this process: the daemon
// runs it, and any number of processes may hold one for the same container
// (Attach). It waits on the container only once something asks whether it
// stopped, so one held only to dial leaves nothing running.
type container struct {
	api      *api
	ref      containerRef
	done     chan struct{}
	err      error
	once     sync.Once
	watching sync.Once
	ctx      context.Context
	stop     context.CancelFunc
	killed   atomic.Bool
}

func newContainer(a *api, ref containerRef) *container {
	ctx, stop := context.WithCancel(context.Background())
	return &container{api: a, ref: ref, done: make(chan struct{}), ctx: ctx, stop: stop}
}

// watched starts the watcher, once, and returns the channel it closes.
func (m *container) watched() <-chan struct{} {
	m.watching.Do(func() { go m.watch(m.ctx) })
	return m.done
}

// watch waits for the container to stop. A wait that fails says nothing about
// the guest (the daemon restarted, say), so it looks, and waits again.
func (m *container) watch(ctx context.Context) {
	for {
		code, err := m.api.wait(ctx, m.ref.ID)
		if ctx.Err() != nil {
			return
		}
		if err == nil {
			if code == 0 {
				m.finish(nil)
			} else {
				m.finish(fmt.Errorf("docker: %s exited %d", m.ref.Name, code))
			}
			return
		}
		c, ierr := m.api.inspectContainer(ctx, m.ref.ID)
		switch {
		case isNotFound(ierr):
			m.finish(fmt.Errorf("docker: container %s was removed", m.ref.Name))
			return
		case ierr == nil && !c.State.Running:
			m.finish(fmt.Errorf("docker: %s stopped: exit %d %s", m.ref.Name, c.State.ExitCode, c.State.Error))
			return
		}
		select {
		case <-ctx.Done():
			return
		case <-time.After(2 * time.Second):
		}
	}
}

// finish closes Done. A guest that was killed stopped as it was asked to, so
// its exit status is no error.
func (m *container) finish(err error) {
	m.once.Do(func() {
		if !m.killed.Load() {
			m.err = err
		}
		m.stop()
		close(m.done)
	})
}

func (m *container) Dial(ctx context.Context, port uint32) (net.Conn, error) {
	select {
	case <-m.done:
		return nil, errors.New("docker: guest is stopped")
	default:
	}
	return dial(ctx, m.api, m.ref.ID, port)
}

// Kill sends the container SIGKILL and waits for it to stop. Killing the
// container's init takes the guest's namespaces with it.
func (m *container) Kill(ctx context.Context) error {
	m.watched()
	m.killed.Store(true)
	if err := m.api.kill(ctx, m.ref.ID); err != nil && !isNotFound(err) {
		return fmt.Errorf("docker: kill %s: %w", m.ref.Name, err)
	}
	select {
	case <-m.done:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

func (m *container) Done() <-chan struct{} { return m.watched() }

func (m *container) Err() error {
	<-m.watched()
	return m.err
}
