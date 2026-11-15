//go:build !darwin

package execpath

// Fix does nothing: only macOS needs it (see the package doc).
func Fix() {}
