package archil

import (
	"context"
	"encoding/json"
	"runtime"
	"slices"
	"testing"

	"github.com/discobox-ai/vm/pkg/machine"
)

// A fenced VM is made with DNS closed (Archil's allow list leaves 53 open to
// 8.8.8.8 and 1.1.1.1 unless they are denied), with service ingress off, and
// its port 7302 is never exposed: it is reached only with a port token.
//
// From the triage of #186, with one change: the fence asks for DNS closed
// (CloseDNS), since an allow-list closes it only when asked (ADR
// 26-11-17-311 §2).
func TestCreateFencesAtTheEdge(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("the fake Archil reads the driver's lines with sh")
	}
	f := newFakeAPI(t, t.TempDir())
	d := f.driver(t, "p", "", false)
	spec := machine.InstanceSpec{Name: "dev", Egress: &machine.Egress{Allow: []string{"example.com"}, CloseDNS: true}}
	inst, err := d.Create(context.Background(), spec)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = d.Destroy(context.Background(), inst) })
	f.mu.Lock()
	defer f.mu.Unlock()
	s := f.sandboxes[0]
	var p egressPolicy
	if err := json.Unmarshal(s.network, &p); err != nil || p.Egress == nil {
		t.Fatalf("the sandbox's policy %s: %v", s.network, err)
	}
	if !slices.Contains(p.Egress.Deny, "8.8.8.8") || !slices.Contains(p.Egress.Deny, "1.1.1.1") {
		t.Errorf("a fenced sandbox was created denying %q: DNS stays open through 8.8.8.8 and 1.1.1.1", p.Egress.Deny)
	}
	if v, ok := f.creates[0]["enable_service_ingress"]; !ok || v != false {
		t.Errorf("a sandbox was created with enable_service_ingress %v, not false", v)
	}
	for port, exposed := range s.ports {
		if exposed {
			t.Errorf("Create exposed port %d: anyone on the internet reaches and wakes it", port)
		}
	}
}
