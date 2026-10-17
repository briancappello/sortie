//go:build unix

package gitsync

import (
	"errors"
	"os"
	"syscall"
)

// tryLockFile takes an exclusive lock on f without waiting and reports
// whether it got it. A flock lock belongs to the open file description,
// so two descriptors of one process exclude each other as two processes
// do.
func tryLockFile(f *os.File) (bool, error) {
	fd := int(f.Fd()) //nolint:gosec // G115: a Unix file descriptor is a small non-negative int carried as uintptr
	for {
		err := syscall.Flock(fd, syscall.LOCK_EX|syscall.LOCK_NB)
		switch {
		case err == nil:
			return true, nil
		case errors.Is(err, syscall.EINTR):
			continue
		case errors.Is(err, syscall.EWOULDBLOCK):
			return false, nil
		default:
			return false, err
		}
	}
}
