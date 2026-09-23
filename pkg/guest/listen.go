package guest

import (
	"fmt"
	"net"
	"strconv"
	"strings"
)

// Listen opens the agent's listener from an address:
//
//	vsock:PORT   the guest's hypervisor socket (the default, vsock:7300)
//	tcp:ADDR     a TCP address, for the fake driver and for tests
func Listen(address string) (net.Listener, error) {
	if address == "" {
		address = "vsock:" + strconv.FormatUint(uint64(AgentPort), 10)
	}
	scheme, rest, ok := strings.Cut(address, ":")
	if !ok {
		return nil, fmt.Errorf("guest: listen address %q has no scheme (vsock: or tcp:)", address)
	}
	switch scheme {
	case "tcp":
		return net.Listen("tcp", rest)
	case "vsock":
		port, err := strconv.ParseUint(rest, 10, 32)
		if err != nil {
			return nil, fmt.Errorf("guest: vsock port %q: %w", rest, err)
		}
		return listenVsock(uint32(port))
	default:
		return nil, fmt.Errorf("guest: unknown listen scheme %q", scheme)
	}
}
