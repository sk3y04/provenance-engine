//go:build !windows

package filelock

import (
	"errors"
	"os"

	"golang.org/x/sys/unix"
)

func tryLock(f *os.File) error {
	return unix.Flock(int(f.Fd()), unix.LOCK_EX|unix.LOCK_NB)
}

func isWouldBlock(err error) bool {
	return errors.Is(err, unix.EWOULDBLOCK)
}

func unlock(f *os.File) error {
	return unix.Flock(int(f.Fd()), unix.LOCK_UN)
}
