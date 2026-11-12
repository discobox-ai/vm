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
