//go:build unix

package cmd

import (
	"os"
	"syscall"
)

// lockFile takes an exclusive advisory lock on f, blocking until it is
// available. The returned function releases it.
func lockFile(f *os.File) (unlock func(), err error) {
	if err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX); err != nil {
		return nil, err
	}
	return func() { syscall.Flock(int(f.Fd()), syscall.LOCK_UN) }, nil
}
