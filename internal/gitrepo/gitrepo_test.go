package gitrepo

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// git runs a git command in dir with a fixed identity, failing the test on error.
func git(t *testing.T, dir string, args ...string) string {
	t.Helper()
	cmd := exec.Command("git", append([]string{"-C", dir}, args...)...)
	cmd.Env = append(os.Environ(),
		"GIT_AUTHOR_NAME=t", "GIT_AUTHOR_EMAIL=t@example.com",
		"GIT_COMMITTER_NAME=t", "GIT_COMMITTER_EMAIL=t@example.com",
		"GIT_CONFIG_GLOBAL=/dev/null", "GIT_CONFIG_NOSYSTEM=1",
	)
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("git %v: %v\n%s", args, err, out)
	}
	return strings.TrimSpace(string(out))
}

// initRepo creates a repo with one commit on main and returns its
// symlink-resolved path (macOS temp dirs live behind /var → /private/var).
func initRepo(t *testing.T) string {
	t.Helper()
	dir, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	git(t, dir, "init", "-q", "-b", "main")
	git(t, dir, "commit", "-q", "--allow-empty", "-m", "init")
	return dir
}

func mustFind(t *testing.T, dir string) Repo {
	t.Helper()
	r, err := Find(dir)
	if err != nil {
		t.Fatalf("Find(%s): %v", dir, err)
	}
	return r
}

func TestFindPlainRepoAndSubdir(t *testing.T) {
	repo := initRepo(t)
	sub := filepath.Join(repo, "a", "b")
	if err := os.MkdirAll(sub, 0o755); err != nil {
		t.Fatal(err)
	}
	want := Repo{Root: repo, GitDir: filepath.Join(repo, ".git"), CommonDir: filepath.Join(repo, ".git")}
	for _, start := range []string{repo, sub} {
		if got := mustFind(t, start); got != want {
			t.Errorf("Find(%s) = %+v, want %+v", start, got, want)
		}
	}
}

// A linked worktree has its own root and gitDir but shares the common dir.
func TestFindLinkedWorktree(t *testing.T) {
	repo := initRepo(t)
	wt := filepath.Join(filepath.Dir(repo), filepath.Base(repo)+"-wt")
	git(t, repo, "worktree", "add", "-q", wt, "-b", "feature")
	t.Cleanup(func() { os.RemoveAll(wt) })

	want := Repo{
		Root:      wt,
		GitDir:    filepath.Join(repo, ".git", "worktrees", filepath.Base(wt)),
		CommonDir: filepath.Join(repo, ".git"),
	}
	if got := mustFind(t, filepath.Join(wt)); got != want {
		t.Errorf("got %+v, want %+v", got, want)
	}
}

func TestFindOutsideRepo(t *testing.T) {
	if _, err := Find(t.TempDir()); err != ErrNotRepo {
		t.Errorf("err = %v, want ErrNotRepo", err)
	}
}

func mustGitState(t *testing.T, dir string) State {
	t.Helper()
	st, err := mustFind(t, dir).ReadState()
	if err != nil {
		t.Fatalf("ReadState(%s): %v", dir, err)
	}
	return st
}

func TestReadGitStateLooseAndPackedRefs(t *testing.T) {
	repo := initRepo(t)
	want := State{Head: git(t, repo, "rev-parse", "HEAD"), Branch: "main"}
	if got := mustGitState(t, repo); got != want {
		t.Errorf("loose ref: got %+v, want %+v", got, want)
	}

	git(t, repo, "pack-refs", "--all") // moves refs/heads/main into packed-refs
	if _, err := os.Stat(filepath.Join(repo, ".git", "refs", "heads", "main")); err == nil {
		t.Fatal("pack-refs left a loose ref; test isn't exercising packed-refs")
	}
	if got := mustGitState(t, repo); got != want {
		t.Errorf("packed ref: got %+v, want %+v", got, want)
	}
}

func TestReadGitStateDetached(t *testing.T) {
	repo := initRepo(t)
	sha := git(t, repo, "rev-parse", "HEAD")
	git(t, repo, "checkout", "-q", "--detach")
	if got := mustGitState(t, repo); got != (State{Head: sha}) {
		t.Errorf("got %+v, want detached at %s", got, sha)
	}
}

func TestReadGitStateUnbornBranch(t *testing.T) {
	dir := t.TempDir()
	git(t, dir, "init", "-q", "-b", "trunk")
	if got := mustGitState(t, dir); got != (State{Branch: "trunk"}) {
		t.Errorf("got %+v, want unborn trunk", got)
	}
}

// A linked worktree reports its own HEAD, resolved through the shared refs.
func TestReadGitStateLinkedWorktree(t *testing.T) {
	repo := initRepo(t)
	wt := filepath.Join(filepath.Dir(repo), filepath.Base(repo)+"-wt")
	git(t, repo, "worktree", "add", "-q", wt, "-b", "feature")
	t.Cleanup(func() { os.RemoveAll(wt) })
	git(t, wt, "commit", "-q", "--allow-empty", "-m", "on feature")

	want := State{Head: git(t, wt, "rev-parse", "HEAD"), Branch: "feature"}
	if got := mustGitState(t, wt); got != want {
		t.Errorf("worktree: got %+v, want %+v", got, want)
	}
	if got := mustGitState(t, repo); got.Branch != "main" || got.Head == want.Head {
		t.Errorf("main checkout: got %+v, want main at its own commit", got)
	}
}

func TestReadGitStateReftable(t *testing.T) {
	dir := t.TempDir()
	if out, err := exec.Command("git", "-C", dir, "init", "-q", "--ref-format=reftable", "-b", "main").CombinedOutput(); err != nil {
		t.Skipf("git without reftable support: %s", out)
	}
	git(t, dir, "commit", "-q", "--allow-empty", "-m", "init")
	want := State{Head: git(t, dir, "rev-parse", "HEAD"), Branch: "main"}
	if got := mustGitState(t, dir); got != want {
		t.Errorf("got %+v, want %+v", got, want)
	}
}
