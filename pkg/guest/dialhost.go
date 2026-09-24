package guest

import (
	"fmt"
	"net"
	"os"
	"path/filepath"
	"strconv"
	"strings"
)

// HostPortsEnv is set in a fake guest: it names a directory where the fake
// driver publishes each port the host listens on, as a file named for the port
// that holds a TCP address, because a fake guest is a host process and has no
// hypervisor socket.
const HostPortsEnv = "DISCO_VM_HOST_PORTS"

// DialHost connects out to the host on a port, which the host serves with
// machine.HostListener (and an instance's forwards): vsock to the host (CID 2)
// on Linux and macOS guests, hvsocket to the parent partition on Windows.
func DialHost(port uint32) (net.Conn, error) {
	if dir := os.Getenv(HostPortsEnv); dir != "" {
		addr, err := os.ReadFile(filepath.Join(dir, strconv.FormatUint(uint64(port), 10)))
		if err != nil {
			return nil, fmt.Errorf("guest: the host does not listen on port %d: %w", port, err)
		}
		return net.Dial("tcp", strings.TrimSpace(string(addr)))
	}
	return dialVsockHost(port)
}
