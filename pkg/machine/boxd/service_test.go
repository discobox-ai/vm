package boxd

import (
	"context"
	"os"
	"path/filepath"
	"runtime"
	"testing"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	"github.com/discobox-ai/vm/pkg/guest"
	"github.com/discobox-ai/vm/pkg/machine"
)

// TestServiceFakeAPI checks what Prepare sets up for the service: the default
// proxy pinned to guest.ServicePort, bot protection off, and the URL
// recorded.
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

	base := machine.Layer{ID: "base", Dir: filepath.Join(dir, "layer")}
	if err := os.MkdirAll(base.Dir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := d.Install(ctx, machine.InstallSpec{GuestOS: machine.Linux}, base); err != nil {
		t.Fatal(err)
	}
	inst := machine.InstanceSpec{ID: "cold", Dir: filepath.Join(dir, "cold"), GuestOS: machine.Linux, Chain: []machine.Layer{base}}
	if err := d.Prepare(ctx, inst); err != nil {
		t.Fatalf("prepare: %v", err)
	}
	service, err := d.Service(ctx, inst)
	if err != nil {
		t.Fatalf("service: %v", err)
	}
	if service.Listen != "tcp:0.0.0.0:7302" || service.Port != guest.ServicePort {
		t.Errorf("service %+v", service)
	}
	var ref vmRef
	if err := readJSON(filepath.Join(inst.Dir, vmFile), &ref); err != nil {
		t.Fatal(err)
	}
	fake.mu.Lock()
	v := fake.vms[ref.ID]
	port, bots := v.proxyPort, v.botProtection
	fake.mu.Unlock()
	if service.URL != "https://"+ref.Name+".boxd.test" || port != guest.ServicePort || bots {
		t.Errorf("URL %q, proxy port %d, bot protection %v", service.URL, port, bots)
	}
	if err := d.Destroy(ctx, inst); err != nil {
		t.Fatal(err)
	}

	// A proxy the API cannot find fails the clone.
	fake.mu.Lock()
	fake.proxyErr = status.Error(codes.NotFound, "no such proxy")
	fake.mu.Unlock()
	inst = machine.InstanceSpec{ID: "noproxy", Dir: filepath.Join(dir, "noproxy"), GuestOS: machine.Linux, Chain: []machine.Layer{base}}
	if err := d.Prepare(ctx, inst); err == nil {
		t.Fatal("prepare with a proxy that cannot be found succeeded")
	}
	if err := d.Destroy(ctx, inst); err != nil {
		t.Fatal(err)
	}
}
