package boxd

import (
	"context"
	"os"
	"runtime"
	"testing"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	"github.com/discobox-ai/vm/pkg/guest"
	"github.com/discobox-ai/vm/pkg/machine"
)

// TestServiceFakeAPI checks what Create sets up for the service: the default
// proxy pinned to guest.ServicePort, bot protection off, and the URL; and the
// private URL, the URL on no networks and the machine's internal DNS name on
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

	base, err := snapshotOfBase(ctx, d, "0123456789abcdef")
	if err != nil {
		t.Fatal(err)
	}
	inst, err := d.Create(ctx, machine.InstanceSpec{Name: "cold", ImageID: base.ID, GuestOS: machine.Linux})
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
	peer, err := d.Create(ctx, machine.InstanceSpec{Name: "peer", ImageID: base.ID, GuestOS: machine.Linux, Isolated: true, Networks: []string{"pool"}})
	if err != nil {
		t.Fatalf("create on a network: %v", err)
	}
	if service, err := d.Service(ctx, peer); err != nil || service.PrivateURL != "http://dvm-default--peer.boxd:7302" {
		t.Errorf("an instance on a network: %+v, %v", service, err)
	}
	if err := d.Destroy(ctx, peer); err != nil {
		t.Fatal(err)
	}

	// A proxy the API cannot find fails the clone, which leaves no machine.
	fake.mu.Lock()
	fake.proxyErr = status.Error(codes.NotFound, "no such proxy")
	fake.mu.Unlock()
	if _, err := d.Create(ctx, machine.InstanceSpec{Name: "noproxy", ImageID: base.ID, GuestOS: machine.Linux}); err == nil {
		t.Fatal("create with a proxy that cannot be found succeeded")
	}
	fake.mu.Lock()
	defer fake.mu.Unlock()
	if len(fake.vms) != 0 {
		t.Fatalf("a failed create left %d machines", len(fake.vms))
	}
}
