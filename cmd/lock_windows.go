//go:build windows

package cmd

import (
	"os"
	"syscall"
	"unsafe"
)

// Windows support is not shipped yet (see the Makefile's NPM_PLATFORMS); this
// keeps the build compiling and is the starting point when it is.

var (
	kernel32         = syscall.NewLazyDLL("kernel32.dll")
	procLockFileEx   = kernel32.NewProc("LockFileEx")
	procUnlockFileEx = kernel32.NewProc("UnlockFileEx")
)

const lockfileExclusiveLock = 0x2

// lockFile takes an exclusive lock on the whole of f, blocking until it is
// available. The returned function releases it.
func lockFile(f *os.File) (unlock func(), err error) {
	h := f.Fd()
	ol := new(syscall.Overlapped)
	if r, _, e := procLockFileEx.Call(h, lockfileExclusiveLock, 0, 0xFFFFFFFF, 0xFFFFFFFF, uintptr(unsafe.Pointer(ol))); r == 0 {
		return nil, e
	}
	return func() {
		procUnlockFileEx.Call(h, 0, 0xFFFFFFFF, 0xFFFFFFFF, uintptr(unsafe.Pointer(ol)))
	}, nil
}
