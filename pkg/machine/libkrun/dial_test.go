//go:build linux

package libkrun

import (
	"context"
	"encoding/json"
	"net"
	"net/http/httptest"
	"runtime"
	"slices"
	"strings"
	"sync"
	"testing"

	"github.com/google/go-containerregistry/pkg/crane"
	"github.com/google/go-containerregistry/pkg/name"
	"github.com/google/go-containerregistry/pkg/registry"
	"github.com/google/go-containerregistry/pkg/v1/mutate"
	"github.com/google/go-containerregistry/pkg/v1/remote"

	"github.com/discobox-ai/vm/pkg/machine"
	"github.com/discobox-ai/vm/pkg/machine/linuximage"
)

// dials is a Config.Dial that dials directly and records each address it
// is given.
type dials struct {
	mu   sync.Mutex
	addr []string
}

func (d *dials) dial(ctx context.Context, network, addr string) (net.Conn, error) {
	d.mu.Lock()
	d.addr = append(d.addr, addr)
	d.mu.Unlock()
	var nd net.Dialer
	return nd.DialContext(ctx, network, addr)
}

// addrs is every address dialed so far.
func (d *dials) addrs() []string {
	d.mu.Lock()
	defer d.mu.Unlock()
	return slices.Clone(d.addr)
}

// A fresh VM's base pulled from a registry dials through the embedder's
// Config.Dial, as the driver's other connections do, so an embedder that
// checks where a driver connects sees the registry too.
func TestBasePullDialsThroughConfigDial(t *testing.T) {
	usableKVM(t)
	img, err := crane.Image(map[string][]byte{linuximage.KrunKernel: []byte("k"), linuximage.Initrd: []byte("i"), linuximage.Root: []byte("r")})
	if err != nil {
		t.Fatal(err)
	}
	config, err := img.ConfigFile()
	if err != nil {
		t.Fatal(err)
	}
	config.OS, config.Architecture = "linux", "amd64"
	if img, err = mutate.ConfigFile(img, config); err != nil {
		t.Fatal(err)
	}
	server := httptest.NewServer(registry.New())
	t.Cleanup(server.Close)
	ref := strings.TrimPrefix(server.URL, "http://") + "/guest:v1"
	tag, err := name.NewTag(ref)
	if err != nil {
		t.Fatal(err)
	}
	if err := remote.Write(tag, img); err != nil {
		t.Fatal(err)
	}

	var dialed dials
	rt := writeRuntime(t, fakeRuntime(t, launchVersion, ""))
	conf, err := json.Marshal(Config{Runtime: rt})
	if err != nil {
		t.Fatal(err)
	}
	d, err := machine.New("libkrun", machine.Config{Dir: t.TempDir(), Base: ref, DriverConfig: conf, Dial: dialed.dial})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := d.Create(t.Context(), machine.InstanceSpec{Name: "fresh"}); err != nil {
		t.Fatal(err)
	}
	if got := dialed.addrs(); !slices.Contains(got, server.Listener.Addr().String()) {
		t.Fatalf("the base pull from %s dialed %v through Config.Dial", server.Listener.Addr(), got)
	}
}

// The runtime pull calls the registry through the transport it is given,
// which Prepare's is the driver's Config.HTTPTransport: through an
// embedder's Config.Dial. (DefaultRuntime is a constant, so the driver
// cannot be pointed at a stand-in registry for it.)
func TestPullRuntimeDialsThroughConfigDial(t *testing.T) {
	ref := pushRuntime(t, fakeRuntime(t, launchVersion, ""), runtime.GOARCH)
	var dialed dials
	transport := machine.Config{Dial: dialed.dial}.HTTPTransport()
	if _, err := pullRuntime(t.Context(), transport, t.TempDir(), ref); err != nil {
		t.Fatal(err)
	}
	registry, _, _ := strings.Cut(ref, "/")
	if got := dialed.addrs(); !slices.Contains(got, registry) {
		t.Fatalf("the runtime pull from %s dialed %v through Config.Dial", registry, got)
	}
}
