package guest

import (
	"context"
	"errors"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/discobox-ai/vm/pkg/machine"
)

// Old servers omit the code, and future servers may send a code this client
// does not know. Neither case should lose the message or invent a sentinel.
func TestErrorCodeCompatibility(t *testing.T) {
	for _, code := range []string{"", "future-error", "stale"} {
		t.Run(code, func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				w.Header().Set(errorCodeHeader, code)
				http.Error(w, "instance kept: unavailable", http.StatusBadGateway)
			}))
			defer server.Close()
			c := NewClient(func(ctx context.Context) (net.Conn, error) {
				return (&net.Dialer{}).DialContext(ctx, "tcp", server.Listener.Addr().String())
			})
			for _, call := range []func() error{
				func() error { _, err := c.Info(t.Context()); return err },
				func() error { _, err := c.Exec(t.Context(), machine.ExecRequest{Argv: []string{"true"}}); return err },
			} {
				err := call()
				if err == nil || !strings.Contains(err.Error(), "instance kept: unavailable") {
					t.Fatalf("message lost: %v", err)
				}
				if errors.Is(err, machine.ErrStale) != (code == "stale") {
					t.Fatalf("code %q: %v, unexpected sentinel identity", code, err)
				}
			}
		})
	}
}
