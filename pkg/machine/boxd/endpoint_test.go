package boxd

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	"github.com/discobox-ai/vm/pkg/machine"
	"github.com/discobox-ai/vm/pkg/machine/boxd/internal/boxdapi"
)

// TestEndpointFakeAPI checks what Prepare sets up for a service: the default
// proxy pinned to its port, bot protection off, and the URL recorded, for a
// cold clone and a resumed one. An instance with no service is left alone.
func TestEndpointFakeAPI(t *testing.T) {
	if runtime.GOOS != "linux" {
		t.Skip("the fake boxd's guests run on the host, and boxd guests are linux")
	}
	agent := linuxAgent(t)
	dir, err := os.MkdirTemp("", "boxd")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(dir) })
	d, fake := fakeDriver(t, agent, dir)
	t.Cleanup(fake.close)
	ctx := context.Background()

	base := machine.Layer{ID: "base", Dir: filepath.Join(dir, "layer")}
	if err := os.MkdirAll(base.Dir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := d.Install(ctx, machine.InstallSpec{GuestOS: machine.Linux, Agent: agent}, base); err != nil {
		t.Fatal(err)
	}
	warm := machine.WarmSpec{GuestOS: machine.Linux, Chain: []machine.Layer{base}, Dir: filepath.Join(dir, "warm")}
	if err := os.MkdirAll(warm.Dir, 0o755); err != nil {
		t.Fatal(err)
	}
	if _, err := d.Warm(ctx, warm); err != nil {
		t.Fatal(err)
	}

	for _, tc := range []struct {
		mode    machine.CloneMode
		service uint32
	}{{machine.Cold, 8123}, {machine.Resume, 8124}, {machine.Cold, 0}} {
		name := fmt.Sprintf("%s-%d", tc.mode, tc.service)
		inst := machine.InstanceSpec{ID: name, Dir: filepath.Join(dir, name), GuestOS: machine.Linux, Chain: []machine.Layer{base}, Mode: tc.mode, Service: tc.service}
		if tc.mode == machine.Resume {
			inst.WarmDir = warm.Dir
		}
		if err := d.Prepare(ctx, inst); err != nil {
			t.Fatalf("%s: prepare: %v", name, err)
		}
		url, err := d.Endpoint(ctx, inst)
		if err != nil {
			t.Fatalf("%s: endpoint: %v", name, err)
		}
		var ref vmRef
		if err := readJSON(filepath.Join(inst.Dir, vmFile), &ref); err != nil {
			t.Fatal(err)
		}
		fake.mu.Lock()
		v := fake.vms[ref.ID]
		port, bots := v.proxyPort, v.botProtection
		fake.mu.Unlock()
		switch {
		case tc.service == 0 && (url != "" || port != 0 || !bots):
			t.Errorf("%s: no service, yet endpoint %q, proxy port %d, bot protection %v", name, url, port, bots)
		case tc.service != 0 && (url != "https://"+ref.Name+".boxd.test" || port != tc.service || bots):
			t.Errorf("%s: endpoint %q, proxy port %d, bot protection %v", name, url, port, bots)
		}
		if err := d.Destroy(ctx, inst); err != nil {
			t.Fatal(err)
		}
	}

	// A proxy the API cannot find fails the clone; it does not read as a
	// stage that is gone, which would quietly clone cold instead.
	fake.mu.Lock()
	fake.proxyErr = status.Error(codes.NotFound, "no such proxy")
	fake.mu.Unlock()
	inst := machine.InstanceSpec{ID: "noproxy", Dir: filepath.Join(dir, "noproxy"), GuestOS: machine.Linux, Chain: []machine.Layer{base}, Mode: machine.Resume, WarmDir: warm.Dir, Service: 8125}
	err = d.Prepare(ctx, inst)
	if err == nil || errors.Is(err, machine.ErrNotWarm) {
		t.Fatalf("prepare with a proxy that cannot be found: %v", err)
	}
	if err := d.Destroy(ctx, inst); err != nil {
		t.Fatal(err)
	}
}

// TestEndpoint reaches an HTTP server in a boxd guest through its instance's
// endpoint, and again after a stop and a start. It needs an account, as
// TestConformance does.
func TestEndpoint(t *testing.T) {
	if os.Getenv(APIKeyEnv) == "" && os.Getenv(tokenEnv) == "" {
		t.Skipf("set %s to reach an endpoint on boxd", APIKeyEnv)
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
	for _, p := range list.GetProxies() {
		if p.GetIsDefault() && (p.GetPortMode() != "locked" || p.GetEffectivePort() != port) {
			t.Errorf("the default proxy is %s at port %d after a restart, want locked at %d", p.GetPortMode(), p.GetEffectivePort(), port)
		}
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
