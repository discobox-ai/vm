package served_test

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"testing"

	"github.com/discobox-ai/vm/pkg/machine"
	"github.com/discobox-ai/vm/pkg/machine/served"
)

// goneDriver attaches machines whose guest operations all report the
// machine gone, as a stale or stopped machine's do.
type goneDriver struct {
	machine.Driver
	err error
}

func (d goneDriver) Attach(ctx context.Context, inst machine.Instance) (machine.Machine, error) {
	m, err := d.Driver.Attach(ctx, inst)
	if err != nil {
		return nil, err
	}
	return goneMachine{m, d.err}, nil
}

type goneMachine struct {
	machine.Machine
	err error
}

func (m goneMachine) gone(op string) error { return fmt.Errorf("%s: %w", op, m.err) }

func (m goneMachine) Exec(context.Context, machine.ExecRequest) (machine.Process, error) {
	return nil, m.gone("exec")
}
func (m goneMachine) CopyTo(context.Context, string, io.Reader) error { return m.gone("copy to") }
func (m goneMachine) CopyFrom(context.Context, string) (io.ReadCloser, error) {
	return nil, m.gone("copy from")
}
func (m goneMachine) Info(context.Context) (machine.GuestInfo, error) {
	return machine.GuestInfo{}, m.gone("info")
}
func (m goneMachine) Shutdown(context.Context, bool) error { return m.gone("shutdown") }

// A sentinel the server's machine returns from a guest operation reaches
// the client's errors.Is, as one from any other call does, and health
// through the relay does not report a machine that cannot answer as well.
func TestGuestOperationSentinelsCrossTheStream(t *testing.T) {
	for _, sentinel := range []error{machine.ErrNotRunning, machine.ErrStale} {
		t.Run(machine.ErrorCode(sentinel), func(t *testing.T) { guestOperationSentinels(t, sentinel) })
	}
}

func guestOperationSentinels(t *testing.T, sentinel error) {
	t.Helper()
	ctx := t.Context()
	bin := agent(t)
	driver := fakeDriver(t, bin, t.TempDir(), "p")
	dial := serve(t, goneDriver{driver, sentinel}, served.Options{Version: "v1", Allow: served.Ops, Fields: served.Fields})
	c := client(t, dial, "v1")

	inst, err := driver.Create(ctx, machine.InstanceSpec{Name: "gone"})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = driver.Destroy(context.Background(), inst) })
	booted, err := driver.Start(ctx, inst, machine.BootOptions{})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = booted.Kill(context.Background()) })

	m, err := c.Attach(ctx, inst)
	if err != nil {
		t.Fatal(err)
	}
	for op, call := range map[string]func() error{
		"exec":      func() error { _, err := m.Exec(ctx, machine.ExecRequest{Argv: []string{"true"}}); return err },
		"copy to":   func() error { return m.CopyTo(ctx, "/scratch", bytes.NewReader(nil)) },
		"copy from": func() error { _, err := m.CopyFrom(ctx, "/etc"); return err },
		"info":      func() error { _, err := m.Info(ctx); return err },
		"shutdown":  func() error { return m.Shutdown(ctx, false) },
	} {
		if err := call(); !errors.Is(err, sentinel) {
			t.Errorf("%s: %v, want %v", op, err, sentinel)
		}
	}
	if h, ok := m.(interface{ Health(context.Context) error }); ok {
		if err := h.Health(ctx); !errors.Is(err, sentinel) {
			t.Errorf("health through the relay: %v, want %v", err, sentinel)
		}
	}
}
