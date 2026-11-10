//go:build !windows

package fsutil

import (
	"errors"
	"os"

	"golang.org/x/sys/unix"
)

// tryLock flocks f exclusively, reporting false if another open file holds it.
func tryLock(f *os.File) (bool, error) {
	err := unix.Flock(int(f.Fd()), unix.LOCK_EX|unix.LOCK_NB)
	if errors.Is(err, unix.EWOULDBLOCK) {
		return false, nil
	}
	return err == nil, err
}
