//go:build windows

// Package fslock provides exclusive advisory file locks that work across
// processes, used to serialize concurrent agent hooks and background runs.
//
// Windows support is not shipped yet (see the Makefile's NPM_PLATFORMS); this
// keeps the build compiling and is the starting point when it is.
package fslock

import (
	"errors"
	"os"
	"syscall"
	"unsafe"
)

var (
	kernel32         = syscall.NewLazyDLL("kernel32.dll")
	procLockFileEx   = kernel32.NewProc("LockFileEx")
	procUnlockFileEx = kernel32.NewProc("UnlockFileEx")
)

const (
	lockfileExclusiveLock   = 0x2
	lockfileFailImmediately = 0x1
	errorLockViolation      = syscall.Errno(33)
)

func lock(f *os.File, flags uintptr) (func(), error) {
	h := f.Fd()
	ol := new(syscall.Overlapped)
	if r, _, e := procLockFileEx.Call(h, flags, 0, 0xFFFFFFFF, 0xFFFFFFFF, uintptr(unsafe.Pointer(ol))); r == 0 {
		return nil, e
	}
	return func() {
		procUnlockFileEx.Call(h, 0, 0xFFFFFFFF, 0xFFFFFFFF, uintptr(unsafe.Pointer(ol)))
	}, nil
}

// Lock takes an exclusive lock on the whole of f, blocking until it is
// available. The returned function releases it.
func Lock(f *os.File) (unlock func(), err error) {
	return lock(f, lockfileExclusiveLock)
}

// TryLock is Lock without waiting: ok is false if someone else holds it.
func TryLock(f *os.File) (unlock func(), ok bool, err error) {
	unlock, err = lock(f, lockfileExclusiveLock|lockfileFailImmediately)
	if errors.Is(err, errorLockViolation) {
		return nil, false, nil
	}
	return unlock, err == nil, err
}
