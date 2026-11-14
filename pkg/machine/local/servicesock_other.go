//go:build !windows

package local

import (
	"context"
	"net"

	"github.com/discobox-ai/vm/pkg/machine"
)

// serviceSocketName is the owner-only Unix socket a shim serves the
// instance's service on, where its guests do not share the host's loopback
// (listenService).
const serviceSocketName = "service.sock"

// listenServiceSocket listens on service.sock in dir, owner-only, and
// returns its unix:// URL.
func listenServiceSocket(ctx context.Context, dir, _ string) (net.Listener, string, func(), error) {
	l, path, cleanup, err := listenSocket(ctx, dir, serviceSocketName)
	if err != nil {
		return nil, "", nil, err
	}
	return l, machine.SocketURL(path), cleanup, nil
}
