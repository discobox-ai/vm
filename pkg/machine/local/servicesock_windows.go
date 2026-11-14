package local

import (
	"context"
	"fmt"
	"net"

	"github.com/Microsoft/go-winio"
	"golang.org/x/sys/windows"

	"github.com/discobox-ai/vm/internal/fsutil"
	"github.com/discobox-ai/vm/pkg/machine"
)

// listenServiceSocket listens on a named pipe only the owner (this
// process's user) may open, \\.\pipe\disco-vm-ID-RANDOM, and returns its
// npipe:// URL. Pipe names are global to the host, so the name carries a
// random part another user cannot have taken first; a name already taken
// fails the listen.
func listenServiceSocket(_ context.Context, _, id string) (net.Listener, string, func(), error) {
	user, err := windows.GetCurrentProcessToken().GetTokenUser()
	if err != nil {
		return nil, "", nil, fmt.Errorf("shim: the owner of the service pipe: %w", err)
	}
	name := "disco-vm-" + short(id) + "-" + fsutil.RandomHex(8)
	l, err := winio.ListenPipe(`\\.\pipe\`+name, &winio.PipeConfig{
		SecurityDescriptor: "D:P(A;;GA;;;" + user.User.Sid.String() + ")",
	})
	if err != nil {
		return nil, "", nil, fmt.Errorf("shim: listen on the service pipe %s: %w", name, err)
	}
	return l, machine.PipeURL(name), func() {}, nil
}
