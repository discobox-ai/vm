package boxd

import (
	"context"
	"errors"
	"os"
	"runtime"
	"testing"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	"github.com/discobox-ai/vm/pkg/guest"
	"github.com/discobox-ai/vm/pkg/machine"
)

// TestServiceFakeAPI checks what Create sets up for the service: by default,
// the default proxy pinned to guest.ServicePort, bot protection off, and the
// URL; for a VM whose public URL is asked off, no proxy and no URL. And the
// private URL: the URL on no networks, and the machine's internal DNS name on
// some.
func TestServiceFakeAPI(t *testing.T) {
	if runtime.GOOS != "linux" {
		t.Skip("the fake boxd's guests run on the host, and boxd guests are linux")
	}
	dir, err := os.MkdirTemp("", "boxd")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(dir) })
	d, fake := fakeDriver(t, dir)
	ctx := context.Background()

	base, err := commitFresh(ctx, d, "base")
	if err != nil {
		t.Fatal(err)
	}
	inst, err := d.Create(ctx, machine.InstanceSpec{Name: "cold", From: &base})
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	service, err := d.Service(ctx, inst)
	if err != nil {
		t.Fatalf("service: %v", err)
	}
	if service.Listen != "tcp:0.0.0.0:7302" {
		t.Errorf("service %+v", service)
	}
	fake.mu.Lock()
	v := fake.vms[inst.ID]
	port, bots := v.proxyPort, v.botProtection
	fake.mu.Unlock()
	if service.URL != "https://dvm-default--cold.boxd.test" || port != guest.ServicePort || bots {
		t.Errorf("URL %q, proxy port %d, bot protection %v", service.URL, port, bots)
	}
	if service.PrivateURL != service.URL {
		t.Errorf("an instance on no networks has private URL %q, not its URL", service.PrivateURL)
	}
	if err := d.Destroy(ctx, inst); err != nil {
		t.Fatal(err)
	}

	// A sandbox's shape: isolated, on a network, its public URL off. Its peers
	// reach it directly, and nothing reaches it from the internet.
	for _, from := range []*machine.Instance{&base, nil} {
		peer, err := d.Create(ctx, machine.InstanceSpec{Name: "peer", From: from, Isolated: true, Networks: []string{"pool"}, URLs: machine.URLRequests{Public: machine.URLRequest{Mode: machine.URLOff, Strict: true}}})
		if err != nil {
			t.Fatalf("create on a network (from %v): %v", from != nil, err)
		}
		service, err := d.Service(ctx, peer)
		if err != nil || service.URL != "" || service.PrivateURL != "http://dvm-default--peer.boxd:7302" {
			t.Errorf("an instance on a network, public URL off (from %v): %+v, %v", from != nil, service, err)
		}
		fake.mu.Lock()
		removed, bots := fake.vms[peer.ID].proxyRemoved, fake.vms[peer.ID].botProtection
		fake.mu.Unlock()
		if !removed || !bots {
			t.Errorf("a machine whose public URL is off kept its default proxy (%v) or had bot protection turned off (%v)", !removed, !bots)
		}
		if err := d.Destroy(ctx, peer); err != nil {
			t.Fatal(err)
		}
	}

	// On no networks and its public URL off, nothing reaches the service.
	alone, err := d.Create(ctx, machine.InstanceSpec{Name: "alone", From: &base, URLs: machine.URLRequests{Public: machine.URLRequest{Mode: machine.URLOff, Strict: true}}})
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	if service, err := d.Service(ctx, alone); err != nil || service.URL != "" || service.PrivateURL != "" {
		t.Errorf("an instance on no networks, public URL off: %+v, %v", service, err)
	}
	if err := d.Destroy(ctx, alone); err != nil {
		t.Fatal(err)
	}
	// A machine gone is no service, though boxd lists it no proxies, as it
	// lists one whose public URL is off none.
	if service, err := d.Service(ctx, alone); !errors.Is(err, machine.ErrNotFound) {
		t.Errorf("the service of a machine gone: %+v, %v; want ErrNotFound", service, err)
	}

	// A proxy the API cannot find, one boxd will not remove, or one that is
	// still there once removed fails the create, which leaves no machine.
	for _, c := range []struct {
		name string
		mode machine.URLMode
		set  func(bool)
	}{
		{"noproxy", machine.URLDefault, func(on bool) { fake.proxyErr = errOn(on, status.Error(codes.NotFound, "no such proxy")) }},
		{"undeletable", machine.URLOff, func(on bool) {
			fake.deleteProxyErr = errOn(on, status.Error(codes.FailedPrecondition, "the default proxy cannot be removed"))
		}},
		{"kept", machine.URLOff, func(on bool) { fake.proxyKept.Store(on) }},
	} {
		fake.mu.Lock()
		c.set(true)
		fake.mu.Unlock()
		_, err := d.Create(ctx, machine.InstanceSpec{Name: c.name, From: &base, URLs: machine.URLRequests{Public: machine.URLRequest{Mode: c.mode}}})
		fake.mu.Lock()
		c.set(false)
		left := len(fake.vms)
		fake.mu.Unlock()
		if err == nil {
			t.Fatalf("%s: create succeeded", c.name)
		}
		if left != 0 {
			t.Fatalf("%s: a failed create left %d machines", c.name, left)
		}
	}
}

func errOn(on bool, err error) error {
	if on {
		return err
	}
	return nil
}
