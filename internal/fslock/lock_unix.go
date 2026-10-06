//go:build unix

// Package fslock provides exclusive advisory file locks that work across
// processes, used to serialize concurrent agent hooks and background runs.
package fslock

import (
	"errors"
	"os"
	"syscall"
)

// Lock takes an exclusive lock on f, blocking until it is available. The
// returned function releases it.
func Lock(f *os.File) (unlock func(), err error) {
	if err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX); err != nil {
		return nil, err
	}
	return func() { syscall.Flock(int(f.Fd()), syscall.LOCK_UN) }, nil
}

// TryLock is Lock without waiting: ok is false if someone else holds it.
func TryLock(f *os.File) (unlock func(), ok bool, err error) {
	err = syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB)
	if errors.Is(err, syscall.EWOULDBLOCK) {
		return nil, false, nil
	}
	if err != nil {
		return nil, false, err
	}
	return func() { syscall.Flock(int(f.Fd()), syscall.LOCK_UN) }, true, nil
}
