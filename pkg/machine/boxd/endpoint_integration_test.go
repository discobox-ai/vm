package boxd

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/discobox-ai/vm/pkg/machine"
	"github.com/discobox-ai/vm/pkg/machine/boxd/internal/boxdapi"
)

// IntegrationEnv opts into tests that run against boxd itself.
const IntegrationEnv = "DISCO_VM_INTEGRATION"

// TestEndpoint reaches an HTTP server in a boxd guest through its instance's
// endpoint, and again after a stop and a start. It makes billable machines,
// so it runs only when asked for and given an account.
func TestEndpoint(t *testing.T) {
	if os.Getenv(IntegrationEnv) != "1" || (os.Getenv(APIKeyEnv) == "" && os.Getenv(tokenEnv) == "") {
		t.Skipf("set %s=1 and %s to reach an endpoint on boxd, which bills the account", IntegrationEnv, APIKeyEnv)
	}
	d := New()
	ctx := context.Background()
	base := machine.Layer{ID: "base", Dir: os.Getenv(BaseEnv)}
	if base.Dir == "" {
		base.Dir = t.TempDir()
		if err := d.Install(ctx, machine.InstallSpec{GuestOS: machine.Linux, Media: "latest", Agent: linuxAgent(t), Log: testLog{t}}, base); err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = d.DeleteLayer(context.Background(), base) })
	}
	const port = 8123
	inst := machine.InstanceSpec{ID: "endpoint", Dir: t.TempDir(), GuestOS: machine.Linux, Chain: []machine.Layer{base}, Mode: machine.Cold, Service: port}
	t.Cleanup(func() { _ = d.Destroy(context.Background(), inst) })
	if err := d.Prepare(ctx, inst); err != nil {
		t.Fatal(err)
	}
	url, err := d.Endpoint(ctx, inst)
	if err != nil || !strings.HasPrefix(url, "https://") {
		t.Fatalf("endpoint %q, %v", url, err)
	}
	t.Logf("endpoint %s", url)
	var ref vmRef
	if err := readJSON(filepath.Join(inst.Dir, vmFile), &ref); err != nil {
		t.Fatal(err)
	}
	api, _ := d.api.API()

	for boot := 1; boot <= 2; boot++ {
		m, err := d.Boot(ctx, inst, machine.BootOptions{})
		if err != nil {
			t.Fatal(err)
		}
		if err := d.waitAgent(ctx, api, ref.ID); err != nil {
			t.Fatal(err)
		}
		// Under systemd, since what an Exec starts ends with it.
		marker := fmt.Sprintf("disco-vm endpoint %s boot %d", ref.ID, boot)
		serve := fmt.Sprintf(`set -e
mkdir -p /tmp/www && echo '%s' > /tmp/www/index.html
sudo systemd-run python3 -m http.server %d --directory /tmp/www`, marker, port)
		if out, code, err := run(ctx, api, ref.ID, serve); err != nil || code != 0 {
			t.Fatalf("serve: exit %d, %v: %s", code, err, out)
		}
		// A new proxy can answer 502, or not resolve, for its first seconds.
		deadline := time.Now().Add(3 * time.Minute)
		for {
			body, err := httpGet(ctx, url)
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
		if again, err := d.Endpoint(ctx, inst); err != nil || again != url {
			t.Fatalf("endpoint after a stop: %q, %v; want %q", again, err, url)
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

func httpGet(ctx context.Context, url string) (string, error) {
	ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return "", err
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(io.LimitReader(resp.Body, 4096))
	if resp.StatusCode != http.StatusOK {
		return string(body), fmt.Errorf("%s", resp.Status)
	}
	return string(body), err
}
