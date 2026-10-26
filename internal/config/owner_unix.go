//go:build unix

package config

import (
	"os"
	"syscall"
)

// trusted reports whether a directory's .swarmia.json was put there by the
// user or an administrator (root), not by another user of a shared directory
// such as /tmp.
func trusted(info os.FileInfo) bool {
	st, ok := info.Sys().(*syscall.Stat_t)
	return !ok || int(st.Uid) == os.Getuid() || st.Uid == 0
}
