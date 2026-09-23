package guest

import (
	"net"

	"github.com/Microsoft/go-winio"
)

// listenVsock binds the port's hvsocket service ID, the vsock template GUID
// with the port in its first field, for connections from the host.
//
// TODO(windows): verify inside a guest. The host must also list the same
// service ID in the VM's HvSocket ServiceTable or the bind is refused (see
// docs/drivers/hcs.md).
func listenVsock(port uint32) (net.Listener, error) {
	return winio.ListenHvsock(&winio.HvsockAddr{
		VMID:      winio.HvsockGUIDWildcard(),
		ServiceID: winio.VsockServiceID(port),
	})
}
