package cmd

import (
	"errors"
	"os"
	"os/exec"
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

// gitState is the repository position recorded with each event, so captured
// edits know which commit they were made on top of.
type gitState struct {
	Head   string `json:"head,omitempty"`   // commit sha; empty on an unborn branch
	Branch string `json:"branch,omitempty"` // short branch name; empty when detached
}

// readGitState reports HEAD for the worktree enclosing dir. It reads HEAD and
// refs directly (fast enough for every hook) and only spawns git for reftable
// repositories, whose ref storage isn't plain files.
func readGitState(dir string) (gitState, error) {
	gitDir, commonDir, err := gitDirs(dir)
	if err != nil {
		return gitState{}, err
	}
	if _, err := os.Stat(filepath.Join(commonDir, "reftable")); err == nil {
		return readGitStateWithGit(dir)
	}
	data, err := os.ReadFile(filepath.Join(gitDir, "HEAD"))
	if err != nil {
		return gitState{}, err
	}
	head := strings.TrimSpace(string(data))
	ref, symbolic := strings.CutPrefix(head, "ref: ")
	if !symbolic {
		return gitState{Head: head}, nil // detached
	}
	st := gitState{Branch: strings.TrimPrefix(ref, "refs/heads/")}
	st.Head, err = resolveRef(commonDir, ref)
	return st, err
}

// resolveRef looks a ref up as a loose file, then in packed-refs. A ref found in
// neither is an unborn branch and resolves to "".
func resolveRef(commonDir, ref string) (string, error) {
	if data, err := os.ReadFile(filepath.Join(commonDir, filepath.FromSlash(ref))); err == nil {
		return strings.TrimSpace(string(data)), nil
	}
	data, err := os.ReadFile(filepath.Join(commonDir, "packed-refs"))
	if err != nil {
		if os.IsNotExist(err) {
			return "", nil
		}
		return "", err
	}
	for _, line := range strings.Split(string(data), "\n") {
		if sha, name, ok := strings.Cut(strings.TrimSpace(line), " "); ok && name == ref {
			return sha, nil
		}
	}
	return "", nil
}

func readGitStateWithGit(dir string) (gitState, error) {
	var st gitState
	if out, err := exec.Command("git", "-C", dir, "symbolic-ref", "--short", "-q", "HEAD").Output(); err == nil {
		st.Branch = strings.TrimSpace(string(out))
	}
	if out, err := exec.Command("git", "-C", dir, "rev-parse", "-q", "--verify", "HEAD").Output(); err == nil {
		st.Head = strings.TrimSpace(string(out))
	}
	return st, nil
}
