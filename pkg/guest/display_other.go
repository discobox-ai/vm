//go:build !windows

package guest

import (
	"errors"
	"io"
)

// ServeDisplay serves nothing: a macOS guest's display is the framework's
// own window on the host, and a Linux guest has none.
func ServeDisplay() error { return nil }

// DisplayHost is Windows's.
func DisplayHost(io.Reader, io.Writer) error {
	return errors.New("guest: display-host is for Windows guests")
}
