package snapshot

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"

	"github.com/swarmia/swarmia-cli/internal/gitrepo"
)

// MaxRepos is the most worktrees one command is snapshotted in.
const MaxRepos = 8

// maxWorkingSet is how many worktrees a session's working set remembers.
const maxWorkingSet = 32

// Before snapshots each worktree, in parallel. A worktree that fails is left
// out of the result, and its error joined into the returned one.
func (s Store) Before(ctx context.Context, repos []gitrepo.Repo) (Pending, error) {
	snaps := make([]Snapshot, len(repos))
	errs := make([]error, len(repos))
	var wg sync.WaitGroup
	for i, repo := range repos {
		wg.Go(func() { snaps[i], errs[i] = s.Take(ctx, repo) })
	}
	wg.Wait()
	p := Pending{}
	for i, repo := range repos {
		if errs[i] == nil {
			p[repo.Root] = snaps[i]
		}
	}
	return p, errors.Join(errs...)
}

// After finds what a command authored in each worktree snapshotted before it,
// in parallel, by root. A worktree that fails (one removed since, say) is
// left out, and its error joined into the returned one.
func (s Store) After(ctx context.Context, pre Pending) (map[string][]Change, error) {
	roots := make([]string, 0, len(pre))
	for root := range pre {
		roots = append(roots, root)
	}
	changes := make([][]Change, len(roots))
	errs := make([]error, len(roots))
	var wg sync.WaitGroup
	for i, root := range roots {
		wg.Go(func() {
			repo, err := gitrepo.Find(root)
			if err == nil && repo.Root != root {
				err = gitrepo.ErrNotRepo
			}
			if err != nil {
				errs[i] = err
				return
			}
			changes[i], errs[i] = s.Changes(ctx, repo, pre[root])
		})
	}
	wg.Wait()
	out := map[string][]Change{}
	for i, root := range roots {
		if errs[i] == nil && len(changes[i]) > 0 {
			out[root] = changes[i]
		}
	}
	return out, errors.Join(errs...)
}

func (s Store) workingSetPath(session string) string {
	return filepath.Join(s.Dir, "sessions", safeName(session)+".json")
}

// WorkingSet is the worktrees a session has worked in, most recent first.
func (s Store) WorkingSet(session string) []string {
	var roots []string
	if data, err := os.ReadFile(s.workingSetPath(session)); err == nil {
		_ = json.Unmarshal(data, &roots)
	}
	return roots
}

// Touch moves roots to the front of a session's working set (the first one
// first). It writes only when that changes the set. Concurrent hooks may lose
// each other's update; the next event repairs it.
func (s Store) Touch(session string, roots ...string) {
	if session == "" {
		return
	}
	old := s.WorkingSet(session)
	var set []string
	for _, root := range append(slices.Clone(roots), old...) {
		if root != "" && !slices.Contains(set, root) {
			set = append(set, root)
		}
	}
	set = set[:min(len(set), maxWorkingSet)]
	if slices.Equal(set, old) {
		return
	}
	if data, err := json.Marshal(set); err == nil {
		_ = writeAtomic(s.workingSetPath(session), data)
	}
}

// Scope picks the worktrees to snapshot around a shell command, at most
// MaxRepos, in order:
//
//  1. the one the command runs in (cwd);
//  2. those the command names: a word that is a path inside a worktree,
//     relative to cwd or absolute (cd ../lib, git -C lib, make -C lib, cat
//     lib/x.go), including a path not created yet (its existing parent);
//  3. the session's working set (see Touch), most recent first.
//
// A command that writes into a worktree none of these name goes unrecorded
// there; Snapshotted records which worktrees were covered.
func Scope(command, cwd string, workingSet []string) []gitrepo.Repo {
	var repos []gitrepo.Repo
	seen := map[string]bool{}
	add := func(dir string) {
		if len(repos) >= MaxRepos {
			return
		}
		if repo, err := gitrepo.Find(dir); err == nil && !seen[repo.Root] {
			seen[repo.Root] = true
			repos = append(repos, repo)
		}
	}
	if cwd != "" {
		add(cwd)
		dirs := map[string]bool{cwd: true}
		for _, word := range pathWords(command) {
			path := word
			if !filepath.IsAbs(path) {
				path = filepath.Join(cwd, path)
			}
			if dir := existingDir(path); dir != "" && !dirs[dir] {
				dirs[dir] = true
				add(dir)
			}
		}
	}
	for _, root := range workingSet {
		if _, err := os.Stat(root); err == nil {
			add(root)
		}
	}
	return repos
}

// maxPathWords bounds the words of a command looked at as paths, for a
// command with a long script inline.
const maxPathWords = 200

// pathWords returns the words of a command that may be paths: not options,
// variables or the like. It is lenient where splitCommands is strict: any
// word of any command, even one with substitutions, may name a worktree.
func pathWords(command string) []string {
	var words []string
	for _, w := range strings.FieldsFunc(command, func(r rune) bool {
		return r == ' ' || r == '\t' || r == '\n' || r == ';' || r == '&' || r == '|' || r == '(' || r == ')' ||
			r == '<' || r == '>' || r == '`' || r == '"' || r == '\'' || r == '='
	}) {
		if len(words) == maxPathWords {
			break
		}
		if w == "" || strings.HasPrefix(w, "-") || strings.ContainsAny(w, "$*?[]{}~") {
			continue
		}
		words = append(words, w)
	}
	return words
}

// existingDir is the deepest existing directory at or above path.
func existingDir(path string) string {
	for dir := filepath.Clean(path); ; dir = filepath.Dir(dir) {
		if fi, err := os.Stat(dir); err == nil && fi.IsDir() {
			return dir
		}
		if filepath.Dir(dir) == dir {
			return ""
		}
	}
}
