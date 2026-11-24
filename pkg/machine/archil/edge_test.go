package archil

import (
	"context"
	"errors"
	"testing"

	"github.com/discobox-ai/vm/pkg/machine"
)

// A VM whose ingress URL is asked off has only its pool URL, through an
// upgrade too, and a VM made from it asks for its own.
func TestIngressOff(t *testing.T) {
	f := newFakeAPI(t, t.TempDir())
	ctx := context.Background()
	d := f.driver(t, "ingress", "")
	off := machine.URLRequests{Ingress: machine.URLRequest{Mode: machine.URLOff, Strict: true}}
	inst, err := d.Create(ctx, machine.InstanceSpec{Name: "off", URLs: off, Egress: &machine.Egress{Allow: []string{"example.com"}, CloseDNS: true}})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = d.Destroy(context.Background(), inst) })
	service := func(inst machine.Instance, ingress bool, when string) {
		t.Helper()
		s, err := d.Service(ctx, inst)
		if err != nil || s.PoolURL == "" || (s.IngressURL != "") != ingress || s.Credential == nil {
			t.Fatalf("%s: service %+v, %v; want an ingress URL %t", when, s, err, ingress)
		}
	}
	service(inst, false, "asked off")
	upgraded, err := d.Upgrade(ctx, inst, nil)
	if err != nil {
		t.Fatal(err)
	}
	service(upgraded, false, "upgraded")
	if got, err := d.Instance(ctx, "off"); err != nil || got.Egress == nil || !got.Egress.CloseDNS || !got.Bare {
		t.Fatalf("upgraded: %+v, %v; want its fence closing DNS, bare", got, err)
	}
	committed, err := d.Commit(ctx, upgraded)
	if err != nil {
		t.Fatal(err)
	}
	inst = committed
	copied, err := d.Create(ctx, machine.InstanceSpec{Name: "on", From: &committed})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = d.Destroy(context.Background(), copied) })
	service(copied, true, "made from one asked off")
	if copied.Egress != nil {
		t.Fatalf("made from a fenced VM with no fence asked: %+v", copied.Egress)
	}
}

// A fence that closes DNS and allows a resolver it denies is refused, and
// one that closes DNS is refused nowhere it is held.
func TestCloseDNSAllowsNoResolver(t *testing.T) {
	f := newFakeAPI(t, t.TempDir())
	d := f.driver(t, "dns", "")
	_, err := d.Create(context.Background(), machine.InstanceSpec{Name: "leaky", Egress: &machine.Egress{Allow: []string{"8.8.8.8"}, CloseDNS: true}})
	if err == nil {
		t.Fatal("a fence closing DNS and allowing 8.8.8.8 was made")
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	if len(f.sandboxes) > 0 {
		t.Fatalf("a refused create left %d sandboxes", len(f.sandboxes))
	}
}

// A fork carries no token of its source's: Archil was seen to copy none,
// and documents nothing of it, and the fake copies them.
func TestForkRevokesItsSourcesTokens(t *testing.T) {
	f := newFakeAPI(t, t.TempDir())
	ctx := context.Background()
	d := f.driver(t, "fork", "")
	src, err := d.Create(ctx, machine.InstanceSpec{Name: "src"})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = d.Destroy(context.Background(), src) })
	if src, err = d.Commit(ctx, src); err != nil {
		t.Fatal(err)
	}
	// A token minted after the commit, as a caller may.
	if _, err := d.Service(ctx, src); err != nil {
		t.Fatal(err)
	}
	if len(f.honoredTokens(src.ID)) != 1 {
		t.Fatalf("the source has tokens %v, want one", f.honoredTokens(src.ID))
	}
	copied, err := d.Create(ctx, machine.InstanceSpec{Name: "copy", From: &src})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = d.Destroy(context.Background(), copied) })
	if left := f.honoredTokens(copied.ID); len(left) > 0 {
		t.Fatalf("a fork honors its source's tokens %v", left)
	}
	if err := d.Destroy(ctx, copied); err != nil {
		t.Fatal(err)
	}
	if err := d.Destroy(ctx, src); err != nil {
		t.Fatal(err)
	}
	if left := f.honoredTokens(src.ID); len(left) > 0 {
		t.Fatalf("a destroyed VM honors tokens %v", left)
	}
	if _, err := d.Instance(ctx, "src"); !errors.Is(err, machine.ErrNotFound) {
		t.Fatalf("destroyed: %v", err)
	}
}
