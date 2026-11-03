// Package execpath restarts the binary under its real path if it was started
// via a symlink then "..", which on macOS 27 breaks TLS ("SecPolicyCreateSSL error: 0").
package execpath

import (
	"os"
	"path/filepath"
)

// realPath is exe's real path if exe, read as text, doesn't lead to the same
// file: it relies on a symlink being followed before a "..". Else ok is false.
func realPath(exe string) (resolved string, ok bool) {
	clean := filepath.Clean(exe)
	if clean == exe {
		return "", false
	}
	want, err := os.Stat(exe)
	if err != nil {
		return "", false
	}
	if got, err := os.Stat(clean); err == nil && os.SameFile(got, want) {
		return "", false
	}
	resolved, err = filepath.EvalSymlinks(exe)
	if err != nil {
		return "", false
	}
	return resolved, true
}
