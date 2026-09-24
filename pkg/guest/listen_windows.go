package guest

import (
	"context"
	"net"

	"github.com/Microsoft/go-winio"
)

// listenVsock binds the port's hvsocket service ID, the vsock template GUID
// with the port in its first field, for connections from the host.
//
// Verified inside a guest. The host lists the same service ID in the VM's
// HvSocket ServiceTable, or the bind is refused (see
// docs/drivers/hcs.md).
func listenVsock(port uint32) (net.Listener, error) {
	return winio.ListenHvsock(&winio.HvsockAddr{
		VMID:      winio.HvsockGUIDWildcard(),
		ServiceID: winio.VsockServiceID(port),
	})
}

// dialVsockHost connects to the host (the parent partition) on the port's
// hvsocket service ID.
func dialVsockHost(port uint32) (net.Conn, error) {
	return winio.Dial(context.Background(), &winio.HvsockAddr{
		VMID:      winio.HvsockGUIDParent(),
		ServiceID: winio.VsockServiceID(port),
	})
}
