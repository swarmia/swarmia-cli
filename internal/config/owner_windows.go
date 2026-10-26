//go:build windows

package config

import "os"

// trusted reports whether a directory's .swarmia.json was put there by the
// user. Windows support is not shipped yet (see the Makefile's
// NPM_PLATFORMS); until it is, every file is.
func trusted(os.FileInfo) bool { return true }
