//go:build !windows

package guest

import (
	"errors"
	"io"
)

// ConsoleHost is Windows's: a Unix pty has no owner that keeps another account
// from reading it.
func ConsoleHost(_, _ uint16, _, _ string, _ io.Reader, _ io.Writer) (int, error) {
	return -1, errors.New("guest: console-host is for Windows guests")
}
