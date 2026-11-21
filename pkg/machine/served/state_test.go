package served

import (
	"context"
	"errors"
	"net"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/discobox-ai/vm/pkg/machine"
)

// A client reads the state the server sends, attaches to a running VM, and
// refuses a paused one with ErrPaused and a stopped one with ErrNotRunning.
// A state it does not know is an error, not a guess.
func TestStateCrossesAndAPausedVMIsErrPaused(t *testing.T) {
	for _, c := range []struct {
		body   string
		want   machine.State
		attach error
		fails  bool
	}{
		{`{"state":"running"}`, machine.StateRunning, nil, false},
		{`{"state":"paused"}`, machine.StatePaused, machine.ErrPaused, false},
		{`{"state":"stopped"}`, machine.StateStopped, machine.ErrNotRunning, false},
		{`{"state":"frozen"}`, "", nil, true},
	} {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if r.URL.Path != "/v1/vms/vm/state" {
				http.NotFound(w, r)
				return
			}
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(c.body))
		}))
		d := &Driver{http: &http.Client{Transport: &http.Transport{
			DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
				var dialer net.Dialer
				return dialer.DialContext(ctx, "tcp", srv.Listener.Addr().String())
			},
		}}}
		inst := machine.Instance{Name: "vm"}
		got, err := d.State(t.Context(), inst)
		if (err != nil) != c.fails || got != c.want {
			t.Errorf("%s: state %q, %v; want %q (fails %v)", c.body, got, err, c.want, c.fails)
		}
		_, err = d.Attach(t.Context(), inst)
		switch {
		case c.fails && err == nil:
			t.Errorf("%s: attached, want an error", c.body)
		case !c.fails && !errors.Is(err, c.attach) && (err != nil || c.attach != nil):
			t.Errorf("%s: attach: %v; want %v", c.body, err, c.attach)
		}
		srv.Close()
	}
}

// Boot refuses a paused VM with ErrPaused, and a VM whose state the server
// could not read with that error: it never starts either, since a start
// would resume a paused VM.
func TestBootRefusesAPausedVMAndAStateItCannotRead(t *testing.T) {
	for _, c := range []struct {
		name   string
		status int
		body   string
		want   error
	}{
		{"paused", http.StatusOK, `{"state":"paused"}`, machine.ErrPaused},
		{"unreadable", http.StatusBadGateway, `{"error":"the provider did not answer","code":"unavailable"}`, machine.ErrUnavailable},
	} {
		t.Run(c.name, func(t *testing.T) {
			started := false
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				switch r.URL.Path {
				case "/v1/vms/vm/state":
					w.Header().Set("Content-Type", "application/json")
					w.WriteHeader(c.status)
					_, _ = w.Write([]byte(c.body))
				case "/v1/vms/vm/start":
					started = true
					w.WriteHeader(http.StatusNoContent)
				default:
					http.NotFound(w, r)
				}
			}))
			defer srv.Close()
			d := &Driver{http: &http.Client{Transport: &http.Transport{
				DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
					var dialer net.Dialer
					return dialer.DialContext(ctx, "tcp", srv.Listener.Addr().String())
				},
			}}}
			if _, err := d.Boot(t.Context(), machine.Instance{Name: "vm"}, machine.BootOptions{}); !errors.Is(err, c.want) {
				t.Fatalf("boot: %v, want %v", err, c.want)
			}
			if started {
				t.Fatal("boot asked the server to start the VM")
			}
		})
	}
}
