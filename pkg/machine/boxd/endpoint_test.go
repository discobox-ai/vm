package boxd

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"testing"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	"github.com/discobox-ai/vm/pkg/machine"
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
