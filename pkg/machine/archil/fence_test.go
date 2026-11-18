package archil

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"sync"
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
	var mu sync.Mutex
	var calls []string
	var created map[string]any
	status := "running"
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		defer mu.Unlock()
		calls = append(calls, r.Method+" "+r.URL.Path)
		reply := func(data string) { _, _ = io.WriteString(w, `{"success":true,"data":`+data+`}`) }
		switch {
		case r.Method == http.MethodPost && r.URL.Path == "/api/sandboxes":
			raw, _ := io.ReadAll(r.Body)
			_ = json.Unmarshal(raw, &created)
			reply(`{"sandbox_id":"sb-1","name":"dvm-p--dev","status":"pending"}`)
		case r.Method == http.MethodGet && r.URL.Path == "/api/sandboxes/sb-1/network":
			reply(`{"egress":{"default":"deny","allow":["example.com"],"deny":["8.8.8.8","1.1.1.1"]}}`)
		case r.Method == http.MethodPost && r.URL.Path == "/api/sandboxes/sb-1/stop":
			status = "stopped"
			reply(`{}`)
		case r.Method == http.MethodGet && r.URL.Path == "/api/sandboxes/sb-1":
			reply(`{"sandbox_id":"sb-1","name":"dvm-p--dev","status":"` + status + `"}`)
		case strings.HasPrefix(r.URL.Path, "/api/sandboxes/sb-1/ports/"):
			reply(`{"hostname":"7302-r.zone.archil.app"}`)
		default:
			reply(`{}`)
		}
	}))
	defer srv.Close()
	d := &Driver{pool: "p", api: &api{base: srv.URL, keys: machine.StaticKey(machine.Key{Driver: "archil", Secret: "k"}), http: srv.Client()}}
	spec := machine.InstanceSpec{Name: "dev", Egress: &machine.Egress{Allow: []string{"example.com"}, CloseDNS: true}}
	if _, _, err := d.makeSandbox(context.Background(), spec, nil, &made{d: d}); err != nil {
		t.Fatal(err)
	}
	mu.Lock()
	defer mu.Unlock()
	network, _ := created["network"].(map[string]any)
	egress, _ := network["egress"].(map[string]any)
	var deny []string
	if list, ok := egress["deny"].([]any); ok {
		for _, v := range list {
			s, _ := v.(string)
			deny = append(deny, s)
		}
	}
	if !slices.Contains(deny, "8.8.8.8") || !slices.Contains(deny, "1.1.1.1") {
		t.Errorf("a fenced sandbox was created denying %q: DNS stays open through 8.8.8.8 and 1.1.1.1", deny)
	}
	if v, ok := created["enable_service_ingress"]; !ok || v != false {
		t.Errorf("a sandbox was created with enable_service_ingress %v, not false", v)
	}
	for _, c := range calls {
		if strings.HasPrefix(c, "PUT /api/sandboxes/sb-1/ports/") {
			t.Errorf("Create exposed a port (%s): anyone on the internet reaches and wakes it", c)
		}
	}
}
