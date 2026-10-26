package gitrepo

import (
	"bytes"
	"os"
	"syscall"
	"unsafe"
)

// fGetPath is fcntl's F_GETPATH: the path of an open file as the filesystem
// has it.
const fGetPath = 50

// canonical asks the filesystem for path's own spelling (see Canonical):
// macOS's are case-insensitive by default, so a path in any case opens the
// same file, and only F_GETPATH says how it's spelled. "" if it can't tell.
func canonical(path string) string {
	f, err := os.Open(path)
	if err != nil {
		return ""
	}
	defer f.Close()
	var buf [1024]byte // MAXPATHLEN
	if _, _, errno := syscall.Syscall(syscall.SYS_FCNTL, f.Fd(), fGetPath, uintptr(unsafe.Pointer(&buf[0]))); errno != 0 {
		return ""
	}
	if n := bytes.IndexByte(buf[:], 0); n > 0 {
		return string(buf[:n])
	}
	return ""
}
