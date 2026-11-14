package hcs

import (
	"net"
	"testing"
)

// Two Remote Desktop windows opening at once claim loopback addresses of
// their own, and skip one another program holds, so neither gets the other's
// sign-in. The sign-ins they store are forgotten again.
func TestRDPWindowsClaimAddressesOfTheirOwn(t *testing.T) {
	held, err := (&net.ListenConfig{}).Listen(t.Context(), "tcp", net.JoinHostPort("127.77.0.1", rdpRelayPort))
	if err != nil {
		t.Skipf("127.77.0.1:%s is not free here: %v", rdpRelayPort, err)
	}
	defer held.Close()

	type claim struct {
		l      net.Listener
		forget func()
		err    error
	}
	claims := make(chan claim, 2)
	for range 2 {
		go func() {
			l, forget, err := listenRDP(t.Context(), `TEST\disco-vm-test`, "disco-vm-test")
			claims <- claim{l, forget, err}
		}()
	}
	hosts := map[string]bool{}
	for range 2 {
		c := <-claims
		if c.err != nil {
			t.Fatal(c.err)
		}
		defer c.forget()
		defer c.l.Close()
		host, _, _ := net.SplitHostPort(c.l.Addr().String())
		if host == "127.77.0.1" || hosts[host] {
			t.Fatalf("a window claimed %s, held by another or by the other window (%v)", host, hosts)
		}
		hosts[host] = true
	}
}
