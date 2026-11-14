package machine

import (
	"errors"
	"fmt"
	"net"
	"net/netip"
	"strconv"
	"strings"
)

// InMachineAuth declares that a driver can authenticate as the machine it
// runs on, with no key (Config.Keys nil): a credential a process on that
// machine fetches from an endpoint the machine's provider serves there
// (boxd's metadata adapter). The endpoints are the provider's, not the
// driver's: they answer any process on such a machine, whether the driver
// is configured to use them or not. So an embedder that runs other
// processes on a machine of the provider (sandboxes, a proxy, a builder)
// keeps them from Endpoints whatever the configuration, and reads Field
// only to know whether the driver itself authenticates that way, without
// knowing the driver.
type InMachineAuth struct {
	// Field is the key of the configuration's privileged boolean field
	// that turns it on, which is off by default. Off, or given a key, the
	// driver asks no endpoint.
	Field string `json:"field"`
	// Endpoints are every address a process on the machine reaches for the
	// credential.
	Endpoints []MachineEndpoint `json:"endpoints"`
}

// MachineEndpoint is one address on a machine's network, as seen from the
// machine: its default gateway, or a host or CIDR, and a TCP port.
// Exactly one of Gateway and Host is set.
type MachineEndpoint struct {
	// Gateway is the machine's default gateway, which only the machine
	// knows (boxd's: the default route's gateway in /proc/net/route).
	Gateway bool `json:"gateway,omitempty"`
	// Host is an IP address, a CIDR, or a host name.
	Host string `json:"host,omitempty"`
	// Port is the TCP port.
	Port int `json:"port"`
}

// String is the endpoint as HOST:PORT, its host <gateway> where it is the
// machine's default gateway.
func (e MachineEndpoint) String() string {
	host := e.Host
	if e.Gateway {
		host = "<gateway>"
	}
	return net.JoinHostPort(host, strconv.Itoa(e.Port))
}

// check refuses an in-machine declaration a driver should never register.
func (a InMachineAuth) check(fields []ConfigField) error {
	i := -1
	for j, f := range fields {
		if f.Key == a.Field {
			i = j
		}
	}
	switch {
	case i < 0 || fields[i].Retired:
		return fmt.Errorf("in-machine auth is turned on by %q, which is not a current field", a.Field)
	case fields[i].Type != ConfigBoolean:
		return fmt.Errorf("in-machine auth is turned on by %q, which is not a boolean", a.Field)
	case !fields[i].Privileged:
		return fmt.Errorf("in-machine auth is turned on by %q, which is not privileged: it acts as the machine", a.Field)
	case fields[i].Default != "" && fields[i].Default != "false":
		return fmt.Errorf("in-machine auth is turned on by %q, which is on by default: a driver acts as the machine only when its configuration says so", a.Field)
	case len(a.Endpoints) == 0:
		return errors.New("in-machine auth has no endpoint")
	}
	for _, e := range a.Endpoints {
		if err := e.check(); err != nil {
			return fmt.Errorf("in-machine auth: %w", err)
		}
	}
	return nil
}

func (e MachineEndpoint) check() error {
	if e.Gateway == (e.Host != "") {
		return errors.New("an endpoint is not exactly one of the gateway and a host")
	}
	if e.Port < 1 || e.Port > 65535 {
		return fmt.Errorf("endpoint port %d", e.Port)
	}
	if e.Host == "" {
		return nil
	}
	if _, err := netip.ParseAddr(e.Host); err == nil {
		return nil
	}
	if _, err := netip.ParsePrefix(e.Host); err == nil {
		return nil
	}
	if strings.ContainsAny(e.Host, ":/ ") {
		return fmt.Errorf("endpoint host %q is not an address, a CIDR, or a host name", e.Host)
	}
	return nil
}
