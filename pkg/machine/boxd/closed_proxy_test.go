package boxd

import (
	"context"
	"testing"
	"time"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

// TestClosedProxyBotProtectionOff checks that closing a machine's proxies
// turns its bot protection off, on from boxd as a new machine's is, so a
// request to its closed default proxy meets boxd's 502, not a browser
// challenge's 503; that a close which fails leaves it on, since a proxy may
// still be open; and that a refusal to turn it off fails the close.
func TestClosedProxyBotProtectionOff(t *testing.T) {
	d, fake := fakeDriver(t, t.TempDir())
	ctx := context.Background()
	api, err := d.api.API(ctx)
	if err != nil {
		t.Fatal(err)
	}
	fake.mu.Lock()
	v := fake.newVM("closed", 0, 0)
	before := v.botProtection
	fake.mu.Unlock()
	if !before {
		t.Fatal("a new machine's bot protection is off; the fake should start it on, as boxd does")
	}

	if err := closeProxies(ctx, api, v.id); err != nil {
		t.Fatalf("close: %v", err)
	}
	fake.mu.Lock()
	locked, port, bots := v.proxyLocked, v.proxyPort, v.botProtection
	fake.mu.Unlock()
	if !locked || port != closedPort || bots {
		t.Errorf("closed: default proxy locked %v to port %d, bot protection %v; want locked to %d, off", locked, port, bots, closedPort)
	}

	quickLock(t, 100*time.Millisecond)
	fake.portIgnored.Store(true)
	fake.mu.Lock()
	open := fake.newVM("open", 0, 0)
	fake.mu.Unlock()
	err = closeProxies(ctx, api, open.id)
	fake.portIgnored.Store(false)
	fake.mu.Lock()
	bots = open.botProtection
	fake.mu.Unlock()
	if err == nil || !bots {
		t.Errorf("a close boxd did not keep: %v, bot protection %v; want an error, on", err, bots)
	}

	fake.mu.Lock()
	refused := fake.newVM("refused", 0, 0)
	fake.botProtectionErr = status.Error(codes.Unavailable, "try again")
	fake.mu.Unlock()
	if err := closeProxies(ctx, api, refused.id); err == nil {
		t.Error("closing a machine whose bot protection boxd will not turn off succeeded")
	}
}
