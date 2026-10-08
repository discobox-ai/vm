package guest

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"path/filepath"
	"strconv"
	"strings"
)

// Listen opens the agent's listener from an address:
//
//	vsock:PORT   the guest's hypervisor socket (the default, vsock:7300)
//	tcp:ADDR     a TCP address, for the fake driver and for tests
//	unix:PATH    a socket only its owner can open, for a guest reached through
//	             a relay inside it (`disco-vm pipe`) rather than a hypervisor
//	             socket, such as a boxd machine
func Listen(address string) (net.Listener, error) {
	if address == "" {
		address = "vsock:" + strconv.FormatUint(uint64(AgentPort), 10)
	}
	scheme, rest, ok := strings.Cut(address, ":")
	if !ok {
		return nil, fmt.Errorf("guest: listen address %q has no scheme (vsock:, tcp:, or unix:)", address)
	}
	switch scheme {
	case "tcp":
		return net.Listen("tcp", rest)
	case "unix":
		return listenUnix(rest)
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

// listenUnix replaces a socket left by an earlier agent and makes the new one
// private to the agent's user, who is root: anyone who can reach the agent can
// run anything in the guest as root.
func listenUnix(path string) (net.Listener, error) {
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return nil, err
	}
	if err := os.Remove(path); err != nil && !os.IsNotExist(err) {
		return nil, err
	}
	listener, err := net.Listen("unix", path)
	if err != nil {
		return nil, err
	}
	if err := os.Chmod(path, 0o600); err != nil {
		_ = listener.Close()
		return nil, err
	}
	return listener, nil
}

// Dial connects to a tcp: or unix: address in Listen's form, from inside the
// guest.
func Dial(ctx context.Context, address string) (net.Conn, error) {
	scheme, rest, ok := strings.Cut(address, ":")
	if !ok || (scheme != "tcp" && scheme != "unix") {
		return nil, fmt.Errorf("guest: dial address %q is not tcp:ADDR or unix:PATH", address)
	}
	var d net.Dialer
	return d.DialContext(ctx, scheme, rest)
}

// PipeReady is the byte Pipe writes before any other, once it has connected,
// so the far end can tell a relay that reached the guest port from one that
// failed and left nothing but an exit status.
const PipeReady = 0x00

// Pipe connects to address and relays it to in and out until the guest side
// closes. It is `disco-vm pipe`: a driver with no hypervisor socket to the
// guest (boxd) reaches a guest port by running it over its own exec stream.
func Pipe(ctx context.Context, address string, in io.Reader, out io.Writer) error {
	conn, err := Dial(ctx, address)
	if err != nil {
		return err
	}
	defer conn.Close()
	if _, err := out.Write([]byte{PipeReady}); err != nil {
		return err
	}
	go func() {
		_, _ = io.Copy(conn, in)
		// The far end is done sending; keep relaying the answer.
		if half, ok := conn.(interface{ CloseWrite() error }); ok {
			_ = half.CloseWrite()
		} else {
			_ = conn.Close()
		}
	}()
	if _, err := io.Copy(out, conn); err != nil && !errors.Is(err, net.ErrClosed) {
		return err
	}
	return nil
}
