package boxd

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"io"
	"net/http"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/discobox-ai/vm/pkg/guest"
	"github.com/discobox-ai/vm/pkg/machine"
	"github.com/discobox-ai/vm/pkg/machine/boxd/internal/boxdapi"
)

// IntegrationEnv opts into tests that run against boxd itself.
const IntegrationEnv = "DISCO_VM_INTEGRATION"

// TestService reaches an HTTP server in a boxd guest, listening where the
// driver says, through its instance's URL, and again after a stop and a
// start. It makes billable machines,
// so it runs only when asked for and given an account.
func TestService(t *testing.T) {
	if os.Getenv(IntegrationEnv) != "1" || (os.Getenv(APIKeyEnv) == "" && os.Getenv(TokenEnv) == "") {
		t.Skipf("set %s=1 and %s to reach a service on boxd, which bills the account", IntegrationEnv, APIKeyEnv)
	}
	d := envDriver(t, machine.KeyFromEnv("boxd"))
	ctx := context.Background()
	var from machine.Instance
	if name := os.Getenv(FromEnv); name != "" {
		found, err := d.Instance(ctx, name)
		if err != nil {
			t.Fatal(err)
		}
		from = found
	} else {
		committed, err := commitFresh(ctx, d, "service-base-"+randomID()[:8])
		if err != nil {
			t.Fatal(err)
		}
		from = committed
		t.Cleanup(func() { _ = d.Destroy(context.Background(), committed) })
	}
	inst, err := d.Create(ctx, machine.InstanceSpec{Name: "service-" + randomID()[:8], From: &from})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = d.Destroy(context.Background(), inst) })
	service, err := d.Service(ctx, inst)
	if err != nil || !strings.HasPrefix(service.IngressURL, "https://") || service.Listen != "tcp:0.0.0.0:7302" {
		t.Fatalf("service %+v, %v", service, err)
	}
	url, port := service.IngressURL, guest.ServicePort
	t.Logf("URL %s", url)
	ref := vmRef{ID: inst.ID}
	api, _ := d.api.API(ctx)

	for boot := 1; boot <= 2; boot++ {
		m, err := d.Boot(ctx, inst, machine.BootOptions{})
		if err != nil {
			t.Fatal(err)
		}
		// Under systemd, since what an Exec starts ends with it.
		marker := fmt.Sprintf("disco-vm service %s boot %d", ref.ID, boot)
		serve := fmt.Sprintf(`set -e
mkdir -p /tmp/www && echo '%s' > /tmp/www/index.html
sudo systemd-run python3 -m http.server %d --directory /tmp/www`, marker, port)
		if out, code, err := run(ctx, api, ref.ID, serve); err != nil || code != 0 {
			t.Fatalf("serve: exit %d, %v: %s", code, err, out)
		}
		// A new proxy can answer 502, or not resolve, for its first seconds.
		deadline := time.Now().Add(3 * time.Minute)
		for {
			_, _, body, err := httpGet(ctx, url)
			if err == nil && strings.TrimSpace(body) == marker {
				break
			}
			if time.Now().After(deadline) {
				t.Fatalf("boot %d: GET %s: %q, %v; want %q", boot, url, body, err, marker)
			}
			time.Sleep(5 * time.Second)
		}
		if err := m.Kill(ctx); err != nil {
			t.Fatal(err)
		}
		if again, err := d.Service(ctx, inst); err != nil || again.IngressURL != url {
			t.Fatalf("URL after a stop: %q, %v; want %q", again.IngressURL, err, url)
		}
	}
	list, err := api.ListProxies(ctx, &boxdapi.ListProxiesRequest{Vm: ref.ID})
	if err != nil {
		t.Fatal(err)
	}
	found := false
	for _, p := range list.GetProxies() {
		if !p.GetIsDefault() {
			continue
		}
		found = true
		if p.GetPortMode() != "locked" || p.GetEffectivePort() != port {
			t.Errorf("the default proxy is %s at port %d after a restart, want locked at %d", p.GetPortMode(), p.GetEffectivePort(), port)
		}
	}
	if !found {
		t.Error("no default proxy after a restart")
	}
}

// TestNoIngressURL makes a VM whose ingress URL is asked off, strictly, as a
// sandbox's is (isolated), and checks on boxd itself that its default proxy,
// which boxd keeps, stays locked to port 0 and is its only one across two
// boots, and that servers on the service port and the ports boxd's proxy
// picks on its own answer at no URL, where boxd answers as a closed proxy
// instead: Create turned bot protection off, so no browser challenge's 503
// hides it. A name with no machine is the control: it can answer a bare 502
// too, so only the closed proxy's header counts.
// It makes billable machines, so it runs only when asked for and given an
// account.
func TestNoIngressURL(t *testing.T) {
	if os.Getenv(IntegrationEnv) != "1" || (os.Getenv(APIKeyEnv) == "" && os.Getenv(TokenEnv) == "") {
		t.Skipf("set %s=1 and %s to check a VM with no ingress URL on boxd, which bills the account", IntegrationEnv, APIKeyEnv)
	}
	d := envDriver(t, machine.KeyFromEnv("boxd"))
	ctx := context.Background()
	inst, err := d.Create(ctx, machine.InstanceSpec{Name: "noingress-" + randomID()[:8], Isolated: true, URLs: machine.URLRequests{Ingress: machine.URLRequest{Mode: machine.URLOff, Strict: true}}})
	if err != nil {
		t.Fatalf("create a VM with its ingress URL off: %v", err)
	}
	t.Cleanup(func() { _ = d.Destroy(context.Background(), inst) })
	api, _ := d.api.API(ctx)
	def, err := defaultProxy(ctx, api, inst.ID)
	if err != nil || def == nil || def.GetDomain() == "" {
		t.Fatalf("default proxy %v, %v", def, err)
	}
	url := "https://" + def.GetDomain()
	nobody, err := unroutedURL(def)
	if err != nil {
		t.Fatal(err)
	}
	t.Logf("probing %s, and %s for the control", url, nobody)

	// The control: a name with no machine in the same zone. It does not
	// resolve, so it fails, or, through an egress proxy (a discobox's), it
	// answers a bare 502. A check it passes would pass with no proxy routed.
	code, header, body, err := httpGet(ctx, nobody)
	t.Logf("control GET %s: %d, %s %q, %v", nobody, code, proxyHeader, header.Get(proxyHeader), err)
	if closedProxyAnswer(code, header) {
		t.Fatalf("control GET %s: %d %v %q, %v; want no closed proxy's answer", nobody, code, header, body, err)
	}

	for boot := 1; boot <= 2; boot++ {
		m, err := d.Boot(ctx, inst, machine.BootOptions{})
		if err != nil {
			t.Fatal(err)
		}
		marker := fmt.Sprintf("disco-vm private %s boot %d", inst.ID, boot)
		serve := fmt.Sprintf(`set -e
mkdir -p /tmp/www && echo '%s' > /tmp/www/index.html
for port in %d 80 8000 8080; do sudo systemd-run python3 -m http.server $port --directory /tmp/www; done`, marker, guest.ServicePort)
		if out, code, err := run(ctx, api, inst.ID, serve); err != nil || code != 0 {
			t.Fatalf("serve: exit %d, %v: %s", code, err, out)
		}
		list, err := api.ListProxies(ctx, &boxdapi.ListProxiesRequest{Vm: inst.ID})
		if err != nil || len(list.GetProxies()) != 1 || !list.GetProxies()[0].GetIsDefault() || !closed(list.GetProxies()[0]) {
			t.Fatalf("boot %d: proxies %v, %v; want the default one alone, locked to port 0", boot, list.GetProxies(), err)
		}
		if service, err := d.Service(ctx, inst); err != nil || service.IngressURL != "" {
			t.Fatalf("boot %d: service %+v, %v; want no URL", boot, service, err)
		}
		// Nothing answering proves nothing: the name may not resolve yet, or
		// its certificate not be issued. So boxd has to be seen answering
		// as the closed proxy, within the three minutes TestService gives a
		// new proxy to come up; the guest, at no time. A proxy that forwards
		// can also answer 502 while it warms up, so the watch lasts those
		// whole three minutes, and a minute past the first closed answer.
		deadline, refused := time.Now().Add(3*time.Minute), time.Time{}
		for refused.IsZero() || time.Now().Before(deadline) || time.Since(refused) < time.Minute {
			code, header, body, err := httpGet(ctx, url)
			if strings.TrimSpace(body) == marker {
				t.Fatalf("boot %d: %s serves the guest", boot, url)
			}
			if closedProxyAnswer(code, header) && refused.IsZero() {
				refused = time.Now()
			}
			if refused.IsZero() && time.Now().After(deadline) {
				t.Fatalf("boot %d: GET %s: %d %v %q, %v; want boxd to answer 502 with %s: %s", boot, url, code, header, body, err, proxyHeader, closedProxyValue)
			}
			time.Sleep(5 * time.Second)
		}
		if err := m.Kill(ctx); err != nil {
			t.Fatal(err)
		}
	}
}

// proxyHeader is the header boxd's proxy names itself with, and
// closedProxyValue its value for a proxy that has no port to forward to with
// bot protection off. Found on boxd itself (fra, 2026-11-15): a 502 without
// it came for a name with no machine, from a discobox's egress, which could
// not resolve it, and for a name under a zone that is not the region's
// (<machine>.fra.boxd.sh, where fra's zone is boxd.sh).
const (
	proxyHeader      = "x-boxd-proxy"
	closedProxyValue = "placeholder"
)

// closedProxyAnswer is whether a response is boxd's answer for a closed
// proxy, not its bare 502 for a name it routes nowhere.
func closedProxyAnswer(code int, header http.Header) bool {
	return code == http.StatusBadGateway && header.Get(proxyHeader) == closedProxyValue
}

// unroutedURL is a URL in the zone of a machine's proxy, <machine>.<zone>,
// that no machine has: the zone is the region's (ListRegions; fra's is
// boxd.sh, nyc's nyc.boxd.sh), so only the machine's label is replaced.
func unroutedURL(p *boxdapi.ProxyInfo) (string, error) {
	zone, ok := strings.CutPrefix(p.GetDomain(), p.GetVmName()+".")
	if p.GetVmName() == "" || !ok || !strings.Contains(zone, ".") {
		return "", fmt.Errorf("proxy domain %q is not <machine>.<zone> for machine %q", p.GetDomain(), p.GetVmName())
	}
	return "https://dvm-nomachine-" + randomID()[:12] + "." + zone, nil
}

// TestClosedProxyCheck holds TestNoIngressURL's check to what boxd was
// found to answer: a closed proxy counts, a bare 502 for a name with no
// machine does not, and the control's name stays in the zone of the proxy's.
func TestClosedProxyCheck(t *testing.T) {
	for _, tc := range []struct {
		domain, zone string
	}{
		{"dvm-p--a.boxd.sh", ".boxd.sh"},
		{"dvm-p--a.nyc.boxd.sh", ".nyc.boxd.sh"},
		{"other.boxd.sh", ""},
		{"dvm-p--a.sh", ""},
		{"dvm-p--a", ""},
	} {
		got, err := unroutedURL(&boxdapi.ProxyInfo{VmName: "dvm-p--a", Domain: tc.domain})
		if tc.zone == "" {
			if err == nil {
				t.Errorf("unroutedURL(%q) = %q, want an error", tc.domain, got)
			}
			continue
		}
		if err != nil || !strings.HasPrefix(got, "https://dvm-nomachine-") || !strings.HasSuffix(got, tc.zone) || strings.Contains(got, "dvm-p--a") {
			t.Errorf("unroutedURL(%q) = %q, %v; want a name with no machine under %q", tc.domain, got, err, tc.zone)
		}
	}
	placeholder := http.Header{}
	placeholder.Set(proxyHeader, closedProxyValue)
	challenge := http.Header{}
	challenge.Set(proxyHeader, "challenge")
	for _, tc := range []struct {
		code   int
		header http.Header
		want   bool
	}{
		{http.StatusBadGateway, placeholder, true},
		{http.StatusBadGateway, http.Header{}, false},
		{http.StatusServiceUnavailable, challenge, false},
		{http.StatusServiceUnavailable, placeholder, false},
		{0, nil, false},
	} {
		if got := closedProxyAnswer(tc.code, tc.header); got != tc.want {
			t.Errorf("closedProxyAnswer(%d, %v) = %v, want %v", tc.code, tc.header, got, tc.want)
		}
	}
}

func httpGet(ctx context.Context, url string) (int, http.Header, string, error) {
	ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return 0, nil, "", err
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return 0, nil, "", err
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(io.LimitReader(resp.Body, 4096))
	if resp.StatusCode != http.StatusOK {
		return resp.StatusCode, resp.Header, string(body), fmt.Errorf("%s", resp.Status)
	}
	return resp.StatusCode, resp.Header, string(body), err
}

// randomID is an image ID nothing else has.
func randomID() string {
	b := make([]byte, 32)
	_, _ = rand.Read(b)
	return hex.EncodeToString(b)
}
