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
// URL; for a VM whose ingress URL is asked off, no proxy open, bot protection
// off, and no URL. And the
// pool URL: the URL on no networks, and the machine's internal DNS name on
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
	if service.IngressURL != "https://dvm-default--cold.boxd.test" || port != guest.ServicePort || bots {
		t.Errorf("URL %q, proxy port %d, bot protection %v", service.IngressURL, port, bots)
	}
	if service.PoolURL != service.IngressURL {
		t.Errorf("an instance on no networks has pool URL %q, not its URL", service.PoolURL)
	}
	if err := d.Destroy(ctx, inst); err != nil {
		t.Fatal(err)
	}

	// A sandbox's shape: isolated, on a network, its ingress URL off. Its peers
	// reach it directly, and nothing reaches it from the internet: its
	// default proxy, which boxd keeps, is locked to port 0, and any other
	// proxy is removed.
	fake.mu.Lock()
	fake.namedProxy = "extra"
	fake.mu.Unlock()
	for _, from := range []*machine.Instance{&base, nil} {
		peer, err := d.Create(ctx, machine.InstanceSpec{Name: "peer", From: from, Isolated: true, Networks: []string{"pool"}, URLs: machine.URLRequests{Ingress: machine.URLRequest{Mode: machine.URLOff, Strict: true}}})
		if err != nil {
			t.Fatalf("create on a network (from %v): %v", from != nil, err)
		}
		service, err := d.Service(ctx, peer)
		if err != nil || service.IngressURL != "" || service.PoolURL != "http://dvm-default--peer.boxd:7302" {
			t.Errorf("an instance on a network, ingress URL off (from %v): %+v, %v", from != nil, service, err)
		}
		fake.mu.Lock()
		pv := fake.vms[peer.ID]
		locked, port, named, bots := pv.proxyLocked, pv.proxyPort, len(pv.named), pv.botProtection
		fake.mu.Unlock()
		if !locked || port != 0 || named != 0 || bots {
			t.Errorf("a machine whose ingress URL is off: default proxy locked %v to port %d, %d named proxies left, bot protection %v; want locked to 0, none, off", locked, port, named, bots)
		}
		if err := d.Destroy(ctx, peer); err != nil {
			t.Fatal(err)
		}
	}
	fake.mu.Lock()
	fake.namedProxy = ""
	fake.mu.Unlock()

	// On no networks and its ingress URL off, nothing reaches the service.
	alone, err := d.Create(ctx, machine.InstanceSpec{Name: "alone", From: &base, URLs: machine.URLRequests{Ingress: machine.URLRequest{Mode: machine.URLOff, Strict: true}}})
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	if service, err := d.Service(ctx, alone); err != nil || service.IngressURL != "" || service.PoolURL != "" {
		t.Errorf("an instance on no networks, ingress URL off: %+v, %v", service, err)
	}
	if err := d.Destroy(ctx, alone); err != nil {
		t.Fatal(err)
	}
	// A machine gone is no service, though boxd lists it no proxies.
	if service, err := d.Service(ctx, alone); !errors.Is(err, machine.ErrNotFound) {
		t.Errorf("the service of a machine gone: %+v, %v; want ErrNotFound", service, err)
	}

	// Bot protection the API will not turn off, a proxy the API cannot find
	// or lock, a port it does not keep, or a named proxy boxd will not
	// remove or keeps once removed fails the create, which leaves no
	// machine.
	for _, c := range []struct {
		name string
		mode machine.URLMode
		set  func(bool)
	}{
		{"unprotectable", machine.URLDefault, func(on bool) { fake.botProtectionErr = errOn(on, status.Error(codes.Unavailable, "try again")) }},
		{"unprotectable-closed", machine.URLOff, func(on bool) { fake.botProtectionErr = errOn(on, status.Error(codes.Unavailable, "try again")) }},
		{"noproxy", machine.URLDefault, func(on bool) { fake.proxyErr = errOn(on, status.Error(codes.NotFound, "no such proxy")) }},
		{"unlockable", machine.URLOff, func(on bool) { fake.proxyErr = errOn(on, status.Error(codes.InvalidArgument, "invalid port")) }},
		{"open", machine.URLOff, func(on bool) { fake.portIgnored.Store(on) }},
		{"undeletable", machine.URLOff, func(on bool) {
			fake.namedProxy = map[bool]string{true: "extra"}[on]
			fake.deleteProxyErr = errOn(on, status.Error(codes.PermissionDenied, "not yours"))
		}},
		{"kept", machine.URLOff, func(on bool) {
			fake.namedProxy = map[bool]string{true: "extra"}[on]
			fake.proxyKept.Store(on)
		}},
	} {
		fake.mu.Lock()
		c.set(true)
		fake.mu.Unlock()
		_, err := d.Create(ctx, machine.InstanceSpec{Name: c.name, From: &base, URLs: machine.URLRequests{Ingress: machine.URLRequest{Mode: c.mode}}})
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
