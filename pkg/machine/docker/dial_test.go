package docker

import (
	"context"
	"encoding/json"
	"errors"
	"net"
	"strings"
	"testing"

	"github.com/discobox-ai/vm/pkg/machine"
)

// TestTheDialerReachesTheDaemon: the daemon the configuration names is
// dialed through an embedder's dialer, which is given its network and
// address, and can refuse it.
func TestTheDialerReachesTheDaemon(t *testing.T) {
	refused := errors.New("not a host this embedder lets the driver reach")
	for host, want := range map[string]string{"tcp://127.0.0.1:9": "tcp 127.0.0.1:9", "unix:///run/docker.sock": "unix /run/docker.sock"} {
		var got []string
		d, err := New(machine.Config{DriverConfig: json.RawMessage(`{"host":"` + host + `"}`),
			Dial: func(_ context.Context, network, addr string) (net.Conn, error) {
				got = append(got, network+" "+addr)
				return nil, refused
			}})
		if err != nil {
			t.Fatal(err)
		}
		if err := d.Check(t.Context()); err == nil || !strings.Contains(err.Error(), refused.Error()) {
			t.Fatalf("%s: Check through a refusing dialer: %v", host, err)
		}
		if len(got) == 0 || got[0] != want {
			t.Fatalf("%s: dialed %v, want %s", host, got, want)
		}
	}
}

// A daemon on another host publishes the service on its own loopback, which
// this process does not reach, so the driver reports and gives no ingress
// URL; one on this host's socket or loopback does (ADR 26-11-14-610).
func TestARemoteDaemonGivesNoIngress(t *testing.T) {
	for host, remote := range map[string]bool{
		"unix:///var/run/docker.sock": false,
		"tcp://127.0.0.1:2375":        false,
		"tcp://[::1]:2375":            false,
		"tcp://localhost:2375":        false,
		"tcp://10.0.0.7:2375":         true,
		"tcp://docker.example:2375":   true,
	} {
		d := newDriver(machine.Config{}, Config{}, HostDaemon(host, machine.Config{}.Dialer()))
		want := machine.URLAlways
		if remote {
			want = machine.URLNever
		}
		if got := d.Capabilities().URLs; got.Ingress != want || got.Pool != machine.URLAlways {
			t.Errorf("%s: URL reach %+v, want ingress %q and pool always", host, got, want)
		}
	}
}
