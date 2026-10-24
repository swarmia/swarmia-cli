//go:build !darwin

package gitrepo

// canonical has nothing to add to filepath.EvalSymlinks here (see Canonical).
func canonical(string) string { return "" }
