package boxd

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/discobox-ai/vm/pkg/guest"
)

// quickLock makes lockProxy's wait for a lock to read back short, for a test.
func quickLock(t *testing.T, limit time.Duration) {
	t.Helper()
	oldLimit, oldEvery, oldAgain := lockLimit, lockEvery, lockAgain
	t.Cleanup(func() { lockLimit, lockEvery, lockAgain = oldLimit, oldEvery, oldAgain })
	lockLimit, lockEvery, lockAgain = limit, 10*time.Millisecond, 100*time.Millisecond
}

// TestLockProxyReadBack checks that a lock boxd answers before it shows in
// ListProxies, or loses, is waited for and made again until it reads back,
// including when a lock asked again restarts the wait,
// for pinning the service port and closing alike, and that a proxy that
// never reads back locked still fails, naming what it forwards to.
func TestLockProxyReadBack(t *testing.T) {
	d, fake := fakeDriver(t, t.TempDir())
	ctx := context.Background()
	api, err := d.api.API(ctx)
	if err != nil {
		t.Fatal(err)
	}
	newVM := func(name string) string {
		fake.mu.Lock()
		defer fake.mu.Unlock()
		return fake.newVM(name, 0, 0).id
	}
	lag := func(reads int, restarts bool) {
		fake.mu.Lock()
		fake.proxyLag, fake.proxyLagRestarts = reads, restarts
		fake.mu.Unlock()
	}

	// A single read-back, as lockProxy made before, fails on a lag: the
	// flake (region fra, 2026-11-21).
	quickLock(t, 0)
	lag(3, false)
	if _, err := lockProxy(ctx, api, newVM("once"), guest.ServicePort); err == nil {
		t.Fatal("a lock read back once through a lag succeeded; the fake does not lag")
	}

	quickLock(t, 5*time.Second)
	if def, err := lockProxy(ctx, api, newVM("lagged"), guest.ServicePort); err != nil {
		t.Errorf("a lock that reads back late: %v", err)
	} else if def.GetPortMode() != "locked" || def.GetEffectivePort() != guest.ServicePort {
		t.Errorf("a lock that reads back late: %s at %d", def.GetPortMode(), def.GetEffectivePort())
	}
	if err := closeProxies(ctx, api, newVM("lagged-closed")); err != nil {
		t.Errorf("a close that reads back late: %v", err)
	}
	// Locked again every lockAgain, ten reads, a lag that a lock asked again
	// restarts still ends: it is not asked again on every read.
	lag(3, true)
	if _, err := lockProxy(ctx, api, newVM("restarted"), guest.ServicePort); err != nil {
		t.Errorf("a lock whose lag a lock again restarts: %v", err)
	}
	lag(0, false)

	fake.portDropped.Store(2)
	if _, err := lockProxy(ctx, api, newVM("dropped"), guest.ServicePort); err != nil {
		t.Errorf("a lock boxd lost twice: %v", err)
	}

	quickLock(t, 100*time.Millisecond)
	fake.portIgnored.Store(true)
	defer fake.portIgnored.Store(false)
	start := time.Now()
	_, err = lockProxy(ctx, api, newVM("never"), guest.ServicePort)
	if err == nil || !strings.Contains(err.Error(), `to port 8000 (auto), not locked to 7302`) {
		t.Errorf("a lock that never reads back: %v; want it not locked to %d", err, guest.ServicePort)
	}
	if waited := time.Since(start); waited < 50*time.Millisecond || waited > 5*time.Second {
		t.Errorf("a lock that never reads back failed after %v; want about the limit", waited)
	}
}
