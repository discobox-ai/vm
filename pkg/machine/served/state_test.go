package served

import (
	"context"
	"net"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/discobox-ai/vm/pkg/machine"
)

// A client reads the state the server sends, and attaches to a VM that is
// not stopped, a paused one too. A state it does not know is an error, not a
// guess.
func TestStateCrossesAndAPausedVMAttaches(t *testing.T) {
	for _, c := range []struct {
		body     string
		want     machine.State
		attaches bool
		fails    bool
	}{
		{`{"state":"running"}`, machine.StateRunning, true, false},
		{`{"state":"paused"}`, machine.StatePaused, true, false},
		{`{"state":"stopped"}`, machine.StateStopped, false, false},
		{`{"state":"frozen"}`, "", false, true},
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
		if (err == nil) != c.attaches {
			t.Errorf("%s: attach: %v; want attached %v", c.body, err, c.attaches)
		}
		srv.Close()
	}
}
