package served

import (
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/discobox-ai/vm/pkg/machine"
)

// Every sentinel crosses the stream by its own code and a status of its
// own wherever the server answers in JSON, so errors.Is answers there on the
// client as on the server: dial's refusal of a VM whose shim is of another
// protocol (machine.ErrStale) included.
func TestSentinelsCrossByCode(t *testing.T) {
	seen := map[string]bool{}
	all := append([]struct {
		code string
		err  error
	}{}, sentinels...)
	for _, err := range []error{machine.ErrNotFound, machine.ErrExists, machine.ErrInUse, machine.ErrCommitted, machine.ErrCapacity, machine.ErrNotRunning, machine.ErrStale, machine.ErrUnsupported, machine.ErrNotImplemented, machine.ErrNoKey} {
		all = append(all, struct {
			code string
			err  error
		}{machine.ErrorCode(err), err})
	}
	for _, s := range all {
		if seen[s.code] {
			t.Fatalf("code %q names two sentinels", s.code)
		}
		seen[s.code] = true
		if statusOf(s.code) == http.StatusInternalServerError {
			t.Errorf("code %q has no status of its own", s.code)
		}
		if s.code == "version" {
			continue // a *VersionError crosses whole, not by its code
		}
		rec := httptest.NewRecorder()
		writeError(rec, fmt.Errorf("instance x: %w", s.err))
		resp := rec.Result()
		err := readError(resp)
		resp.Body.Close()
		if !errors.Is(err, s.err) || err.Error() != "instance x: "+s.err.Error() {
			t.Errorf("%v crossed as %v", s.err, err)
		}
	}
	if codeOf(machine.ErrStale) != "stale" || statusOf("stale") != http.StatusConflict {
		t.Errorf("machine.ErrStale crosses as %q, %d", codeOf(machine.ErrStale), statusOf("stale"))
	}
}
