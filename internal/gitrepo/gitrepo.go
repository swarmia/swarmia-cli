// Package gitrepo finds the git repository around a directory and reads its
// current position without spawning git, because it runs on every agent hook.
package gitrepo

import (
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
)

// ErrNotRepo is returned when no enclosing git repository is found.
var ErrNotRepo = errors.New("not inside a git repository")

// Repo locates one worktree of a repository. All paths are absolute and
// symlink-resolved, so the same repo always yields the same values.
type Repo struct {
	Root      string // the worktree's top-level directory
	GitDir    string // per-worktree git directory (where HEAD lives)
	CommonDir string // shared by all worktrees (objects, refs); == GitDir for a plain clone
}

// Find locates the worktree enclosing start.
func Find(start string) (Repo, error) {
	dir, err := filepath.Abs(start)
	if err != nil {
		return Repo{}, err
	}
	for {
		dotGit := filepath.Join(dir, ".git")
		if fi, err := os.Stat(dotGit); err == nil {
			gitDir := dotGit
			if !fi.IsDir() {
				if gitDir, err = readGitFile(dotGit); err != nil {
					return Repo{}, err
				}
			}
			return resolve(dir, gitDir)
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			return Repo{}, ErrNotRepo
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

// resolve follows a worktree gitDir's "commondir" file to the shared repository
// directory; without one, the gitDir is its own common dir.
func resolve(root, gitDir string) (Repo, error) {
	var r Repo
	var err error
	if r.Root, err = filepath.EvalSymlinks(root); err != nil {
		return Repo{}, err
	}
	if r.GitDir, err = filepath.EvalSymlinks(gitDir); err != nil {
		return Repo{}, err
	}
	r.CommonDir = r.GitDir
	if data, err := os.ReadFile(filepath.Join(r.GitDir, "commondir")); err == nil {
		common := strings.TrimSpace(string(data))
		if !filepath.IsAbs(common) {
			common = filepath.Join(r.GitDir, common)
		}
		if r.CommonDir, err = filepath.EvalSymlinks(common); err != nil {
			return Repo{}, err
		}
	}
	return r, nil
}

// State is a worktree's position: where HEAD points.
type State struct {
	Head   string // commit sha; empty on an unborn branch
	Branch string // short branch name; empty when detached
}

// ReadState reports HEAD for the worktree. It reads HEAD and refs directly
// (fast enough for every hook) and only spawns git for reftable repositories,
// whose ref storage isn't plain files.
func (r Repo) ReadState() (State, error) {
	if _, err := os.Stat(filepath.Join(r.CommonDir, "reftable")); err == nil {
		return r.readStateWithGit(), nil
	}
	data, err := os.ReadFile(filepath.Join(r.GitDir, "HEAD"))
	if err != nil {
		return State{}, err
	}
	head := strings.TrimSpace(string(data))
	ref, symbolic := strings.CutPrefix(head, "ref: ")
	if !symbolic {
		return State{Head: head}, nil // detached
	}
	st := State{Branch: strings.TrimPrefix(ref, "refs/heads/")}
	st.Head, err = resolveRef(r.CommonDir, ref)
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

func (r Repo) readStateWithGit() State {
	var st State
	if out, err := exec.Command("git", "-C", r.Root, "symbolic-ref", "--short", "-q", "HEAD").Output(); err == nil {
		st.Branch = strings.TrimSpace(string(out))
	}
	if out, err := exec.Command("git", "-C", r.Root, "rev-parse", "-q", "--verify", "HEAD").Output(); err == nil {
		st.Head = strings.TrimSpace(string(out))
	}
	return st
}

// Operation is a git operation stopped part way in the worktree: a rebase,
// cherry-pick, revert or merge, waiting on conflicts or an interactive stop.
// Sources are the commits it is applying (REBASE_HEAD, CHERRY_PICK_HEAD,
// REVERT_HEAD, MERGE_HEAD), as far as git recorded them; a rebase stopped at a
// `break`, for one, has none.
type Operation struct {
	Sources []string `json:"sources,omitempty"`
}

// InProgress reports the operation stopped in the worktree, if any.
func (r Repo) InProgress() (Operation, bool) {
	var op Operation
	busy := false
	for _, name := range []string{"REBASE_HEAD", "CHERRY_PICK_HEAD", "REVERT_HEAD", "MERGE_HEAD"} {
		data, err := os.ReadFile(filepath.Join(r.GitDir, name))
		if err != nil {
			continue
		}
		busy = true
		op.Sources = append(op.Sources, strings.Fields(string(data))...) // MERGE_HEAD lists one per parent
	}
	for _, dir := range []string{"rebase-merge", "rebase-apply", "sequencer"} {
		if _, err := os.Stat(filepath.Join(r.GitDir, dir)); err == nil {
			busy = true
		}
	}
	return op, busy
}

// Stash returns the commit of the latest stash entry, "" if there is none.
func (r Repo) Stash() string {
	if _, err := os.Stat(filepath.Join(r.CommonDir, "reftable")); err == nil {
		out, _ := exec.Command("git", "-C", r.Root, "rev-parse", "-q", "--verify", "refs/stash").Output()
		return strings.TrimSpace(string(out))
	}
	sha, _ := resolveRef(r.CommonDir, "refs/stash")
	return sha
}
