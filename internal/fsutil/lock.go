package fsutil

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
)

// ErrLocked is TryLock's answer for a file another holder has locked.
var ErrLocked = errors.New("locked by another holder")

// TryLock takes an exclusive lock on path, creating it and its directory,
// without waiting: ErrLocked if another open of it, in this process or
// another, holds it. The lock is the operating system's (flock, LockFileEx),
// so it is released when the returned func is called or when the process
// dies, however it dies. The descriptor is not inherited by a child.
func TryLock(path string) (func(), error) {
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return nil, err
	}
	f, err := os.OpenFile(path, os.O_RDWR|os.O_CREATE, 0o600)
	if err != nil {
		return nil, err
	}
	held, err := tryLock(f)
	if err != nil {
		_ = f.Close()
		return nil, fmt.Errorf("lock %s: %w", path, err)
	}
	if !held {
		_ = f.Close()
		return nil, fmt.Errorf("%s: %w", path, ErrLocked)
	}
	return func() { _ = f.Close() }, nil
}
