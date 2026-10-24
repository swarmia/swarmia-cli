package gitrepo

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// caseInsensitive skips the test unless dir's filesystem ignores case, as
// macOS's does by default.
func caseInsensitive(t *testing.T, dir string) {
	t.Helper()
	if _, err := os.Stat(strings.ToUpper(dir)); err != nil {
		t.Skip("the filesystem is case-sensitive")
	}
}

// On a case-insensitive filesystem a repository found through a path in
// another case is the same repository, spelled as the filesystem has it.
func TestFindSpellsRootAsTheFilesystemDoes(t *testing.T) {
	parent, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	dir := filepath.Join(parent, "aimo")
	if err := os.MkdirAll(filepath.Join(dir, "sub"), 0o755); err != nil {
		t.Fatal(err)
	}
	git(t, dir, "init", "-q")
	caseInsensitive(t, dir)

	for _, path := range []string{filepath.Join(parent, "Aimo"), filepath.Join(parent, "AIMO", "Sub")} {
		repo, err := Find(path)
		if err != nil {
			t.Fatal(err)
		}
		if repo.Root != dir || repo.GitDir != filepath.Join(dir, ".git") {
			t.Errorf("Find(%s) = %+v, want root %s", path, repo, dir)
		}
	}
	if got, err := Canonical(filepath.Join(parent, "AIMO", "SUB")); err != nil || got != filepath.Join(dir, "sub") {
		t.Errorf("Canonical = %q, %v", got, err)
	}
}
