//go:build !windows

package machine

import (
	"context"
	"fmt"
	"net"
)

// socketPath is the path of a unix:// URL's socket, as it is.
func socketPath(urlPath string) string { return urlPath }

// dialPipe refuses: a named pipe is Windows'.
func dialPipe(_ context.Context, path string) (net.Conn, error) {
	return nil, fmt.Errorf("the named pipe %s is reached on Windows alone: %w", path, ErrUnsupported)
}
