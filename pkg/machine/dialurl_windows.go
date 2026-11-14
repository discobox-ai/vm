package machine

import (
	"context"
	"net"

	"github.com/Microsoft/go-winio"
)

// socketPath is the path of a unix:// URL's socket: /C:/dir/s.sock is
// C:/dir/s.sock, a path that starts with its drive.
func socketPath(urlPath string) string {
	if len(urlPath) >= 3 && urlPath[0] == '/' && urlPath[2] == ':' {
		return urlPath[1:]
	}
	return urlPath
}

// dialPipe connects to the named pipe at path, \\.\pipe\NAME.
func dialPipe(ctx context.Context, path string) (net.Conn, error) {
	return winio.DialPipeContext(ctx, path)
}
