package gitrepo

import (
	"os"
	"path/filepath"
	"reflect"
	"sort"
	"testing"
	"time"
)

func TestLocallyCreatedCommits(t *testing.T) {
	upstream := initRepo(t) // stands in for a teammate's clone
	git(t, upstream, "commit", "-q", "--allow-empty", "-m", "teammate work")

	dir, _ := filepath.EvalSymlinks(t.TempDir())
	repo := filepath.Join(dir, "clone")
	git(t, dir, "clone", "-q", upstream, repo)
	var want []string
	sha := func(rev string) string { return git(t, repo, "rev-parse", rev) }

	git(t, repo, "commit", "-q", "--allow-empty", "-m", "mine")
	want = append(want, sha("HEAD"))
	git(t, repo, "commit", "-q", "--allow-empty", "--amend", "-m", "mine, amended")
	want = append(want, sha("HEAD"))

	// A rebase replays my commit: the pick is created here.
	git(t, upstream, "commit", "-q", "--allow-empty", "-m", "more teammate work")
	git(t, repo, "fetch", "-q")
	git(t, repo, "rebase", "-q", "origin/main")
	want = append(want, sha("HEAD"))

	// A commit in a linked worktree.
	wt := filepath.Join(dir, "wt")
	git(t, repo, "worktree", "add", "-q", wt, "-b", "side")
	git(t, wt, "commit", "-q", "--allow-empty", "-m", "in the worktree")
	want = append(want, git(t, wt, "rev-parse", "HEAD"))

	// Moving HEAD to commits made elsewhere creates nothing.
	git(t, repo, "reset", "-q", "--hard", "origin/main")
	git(t, repo, "checkout", "-q", "-b", "other")

	r, err := Find(repo)
	if err != nil {
		t.Fatal(err)
	}
	got, err := r.LocallyCreatedCommits(time.Now().Add(-time.Hour))
	if err != nil {
		t.Fatal(err)
	}
	var gotList []string
	for s := range got {
		gotList = append(gotList, s)
	}
	sort.Strings(gotList)
	sort.Strings(want)
	if !reflect.DeepEqual(gotList, want) {
		t.Errorf("created = %v\nwant      %v", gotList, want)
	}
	for _, teammate := range []string{git(t, upstream, "rev-parse", "HEAD"), git(t, upstream, "rev-parse", "HEAD~1")} {
		if got[teammate] {
			t.Errorf("teammate commit %s counted as created here", teammate[:7])
		}
	}

	// Entries older than since are ignored.
	if old, _ := r.LocallyCreatedCommits(time.Now().Add(time.Hour)); len(old) != 0 {
		t.Errorf("entries from before since: %v", old)
	}
}

func TestLocallyCreatedCommitsWithoutReflog(t *testing.T) {
	repo := initRepo(t)
	os.RemoveAll(filepath.Join(repo, ".git", "logs"))
	got, err := mustFind(t, repo).LocallyCreatedCommits(time.Time{})
	if err != nil || len(got) != 0 {
		t.Errorf("got %v, %v; want empty, nil", got, err)
	}
}

func TestCreatedHerePattern(t *testing.T) {
	for msg, want := range map[string]bool{
		"commit: fix the thing":                 true,
		"commit (initial): init":                true,
		"commit (amend): fix the thing":         true,
		"commit (merge): Merge branch 'x'":      true,
		"rebase (pick): fix the thing":          true,
		"rebase -i (fixup): fixup! x":           true,
		"pull --rebase (pick): fix":             true,
		"cherry-pick: fix the thing":            true,
		"revert: Revert \"x\"":                  true,
		"am: apply patch":                       true,
		"rebase (start): checkout origin/main":  false,
		"rebase (finish): returning to main":    false,
		"pull: Fast-forward":                    false,
		"merge origin/main: Fast-forward":       false,
		"reset: moving to HEAD~1":               false,
		"checkout: moving from main to f":       false,
		"clone: from git@github.com:acme/app":   false,
		"commitment: not a real reflog message": false,
	} {
		if got := createdHere.MatchString(msg); got != want {
			t.Errorf("%q: got %v, want %v", msg, got, want)
		}
	}
}
