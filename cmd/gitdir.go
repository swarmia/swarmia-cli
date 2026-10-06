package cmd

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
)

// errNotGitRepo is returned when no enclosing git repository is found.
var errNotGitRepo = errors.New("not inside a git repository")

// gitDirs locates the repository enclosing start without spawning git, since it
// runs on every hook invocation. gitDir is the per-worktree directory (where
// HEAD lives); commonDir is shared by all worktrees of the repo (objects, refs).
// For an ordinary clone they are the same .git directory. Both are absolute and
// symlink-resolved so the same repo always yields the same paths.
func gitDirs(start string) (gitDir, commonDir string, err error) {
	dir, err := filepath.Abs(start)
	if err != nil {
		return "", "", err
	}
	for {
		dotGit := filepath.Join(dir, ".git")
		fi, err := os.Stat(dotGit)
		if err == nil {
			if fi.IsDir() {
				gitDir = dotGit
			} else if gitDir, err = readGitFile(dotGit); err != nil {
				return "", "", err
			}
			return resolveCommonDir(gitDir)
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			return "", "", errNotGitRepo
		}
		dir = parent
	}
}

// readGitFile parses a ".git" file ("gitdir: <path>"), as used by linked
// worktrees and submodules. A relative path is relative to the file's directory.
func readGitFile(path string) (string, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return "", err
	}
	target, ok := strings.CutPrefix(strings.TrimSpace(string(data)), "gitdir:")
	if !ok {
		return "", errors.New("malformed .git file: " + path)
	}
	target = strings.TrimSpace(target)
	if !filepath.IsAbs(target) {
		target = filepath.Join(filepath.Dir(path), target)
	}
	return target, nil
}

// resolveCommonDir follows a worktree gitDir's "commondir" file to the shared
// repository directory; without one, the gitDir is its own common dir.
func resolveCommonDir(gitDir string) (string, string, error) {
	gitDir, err := filepath.EvalSymlinks(gitDir)
	if err != nil {
		return "", "", err
	}
	commonDir := gitDir
	if data, err := os.ReadFile(filepath.Join(gitDir, "commondir")); err == nil {
		commonDir = strings.TrimSpace(string(data))
		if !filepath.IsAbs(commonDir) {
			commonDir = filepath.Join(gitDir, commonDir)
		}
		if commonDir, err = filepath.EvalSymlinks(commonDir); err != nil {
			return "", "", err
		}
	}
	return gitDir, commonDir, nil
}
