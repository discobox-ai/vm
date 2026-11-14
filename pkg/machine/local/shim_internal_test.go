package local

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// A shim from before /display answers 404, which reads as a shim to restart,
// not as the mux's own text.
func TestDisplayFromAShimWithoutTheRoute(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer t" {
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return
		}
		http.NotFound(w, r)
	}))
	t.Cleanup(server.Close)
	c := &shimClient{state: shimState{Addr: server.Listener.Addr().String(), Token: "t"}}
	_, err := c.display(t.Context())
	if err == nil || !strings.Contains(err.Error(), "restart the instance") {
		t.Fatalf("display from an old shim: %v, want one saying to restart the instance", err)
	}
}
