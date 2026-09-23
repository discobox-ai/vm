//go:build !windows

package guest

import (
	"os"
	"os/exec"

	"github.com/creack/pty"
)

// ptyHandle is a started process's terminal: reading it is the process's
// output, writing it is the process's input.
type ptyHandle struct{ *os.File }

func (p ptyHandle) Resize(rows, cols uint16) error {
	return pty.Setsize(p.File, &pty.Winsize{Rows: rows, Cols: cols})
}

func startPTY(cmd *exec.Cmd, rows, cols uint16) (ptyHandle, error) {
	if rows == 0 || cols == 0 {
		rows, cols = 24, 80
	}
	f, err := pty.StartWithSize(cmd, &pty.Winsize{Rows: rows, Cols: cols})
	if err != nil {
		return ptyHandle{}, err
	}
	return ptyHandle{f}, nil
}
