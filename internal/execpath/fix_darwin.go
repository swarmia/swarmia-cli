package execpath

import (
	"os"
	"syscall"
)

// Fix re-executes the binary under its real path if it needs to (see the
// package doc). It returns only when it doesn't, or the re-exec fails.
func Fix() {
	exe, err := os.Executable()
	if err != nil {
		return
	}
	if resolved, ok := realPath(exe); ok {
		syscall.Exec(resolved, os.Args, os.Environ())
	}
}
