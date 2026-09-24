//go:build !linux && !darwin && !windows

package guest

import (
	"fmt"
	"net"
	"runtime"
)

func listenVsock(uint32) (net.Listener, error) {
	return nil, fmt.Errorf("guest: no hypervisor socket on %s; use tcp:", runtime.GOOS)
}

func dialVsockHost(uint32) (net.Conn, error) {
	return nil, fmt.Errorf("guest: no hypervisor socket on %s", runtime.GOOS)
}
