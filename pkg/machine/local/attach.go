package local

import (
	"context"
	"fmt"
	"net"
	"sync"
	"time"

	"github.com/discobox-ai/vm/internal/proc"
	"github.com/discobox-ai/vm/pkg/guest"
	"github.com/discobox-ai/vm/pkg/machine"
)

// Attach reaches the instance's machine through its shim, as any process can.
func (d *Driver) Attach(ctx context.Context, inst machine.Instance) (machine.Machine, error) {
	if err := d.authorizeInstance(ctx, inst); err != nil {
		return nil, err
	}
	shim, err := d.openShim(inst.ID)
	if err != nil {
		return nil, fmt.Errorf("instance %s: %w", inst.Name, machine.ErrNotRunning)
	}
	statusCtx, cancel := context.WithTimeout(ctx, 2*time.Second)
	defer cancel()
	if err := shim.status(statusCtx); err != nil {
		return nil, fmt.Errorf("instance %s: %w", inst.Name, machine.ErrNotRunning)
	}
	m := &shimMachine{shim: shim, done: make(chan struct{})}
	m.Client = guest.NewClient(func(ctx context.Context) (net.Conn, error) { return shim.dialGuest(ctx) })
	return m, nil
}

// shimMachine is a running machine its shim holds. Its guest operations are
// the shim's relay of the machine's; it is stopped once the shim has exited,
// which a shim does when its machine stops.
type shimMachine struct {
	*guest.Client
	shim     *shimClient
	done     chan struct{}
	watching sync.Once
}

func (m *shimMachine) Kill(ctx context.Context) error {
	err := m.shim.kill(ctx)
	if err != nil && !proc.Alive(m.shim.state.PID) {
		return nil
	}
	return err
}

func (m *shimMachine) Done() <-chan struct{} {
	m.watching.Do(func() {
		go func() {
			for proc.Alive(m.shim.state.PID) {
				time.Sleep(100 * time.Millisecond)
			}
			close(m.done)
		}()
	})
	return m.done
}

// Err is nil: how the machine stopped is in its shim's log.
func (m *shimMachine) Err() error {
	<-m.Done()
	return nil
}
