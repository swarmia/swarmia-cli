package cmd

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

func TestGitDirsPlainRepoAndSubdir(t *testing.T) {
	repo := initRepo(t)
	sub := filepath.Join(repo, "a", "b")
	if err := os.MkdirAll(sub, 0o755); err != nil {
		t.Fatal(err)
	}
	for _, start := range []string{repo, sub} {
		gitDir, common, err := gitDirs(start)
		if err != nil {
			t.Fatalf("gitDirs(%s): %v", start, err)
		}
		want := filepath.Join(repo, ".git")
		if gitDir != want || common != want {
			t.Errorf("gitDirs(%s) = (%s, %s), want both %s", start, gitDir, common, want)
		}
	}
}

// A linked worktree has its own gitDir but shares the main repo's common dir,
// and therefore its state directory.
func TestGitDirsLinkedWorktreeSharesCommonDir(t *testing.T) {
	repo := initRepo(t)
	wt := filepath.Join(filepath.Dir(repo), filepath.Base(repo)+"-wt")
	git(t, repo, "worktree", "add", "-q", wt, "-b", "feature")
	t.Cleanup(func() { os.RemoveAll(wt) })

	gitDir, common, err := gitDirs(wt)
	if err != nil {
		t.Fatal(err)
	}
	if want := filepath.Join(repo, ".git"); common != want {
		t.Errorf("common = %s, want %s", common, want)
	}
	if want := filepath.Join(repo, ".git", "worktrees", filepath.Base(wt)); gitDir != want {
		t.Errorf("gitDir = %s, want %s", gitDir, want)
	}

	t.Setenv("SWARMIA_STATE_DIR", t.TempDir())
	a, _ := repoStateDir(repo)
	b, _ := repoStateDir(wt)
	if a != b {
		t.Errorf("worktree state dir %s differs from main repo's %s", b, a)
	}
}

func TestGitDirsOutsideRepo(t *testing.T) {
	if _, _, err := gitDirs(t.TempDir()); err != errNotGitRepo {
		t.Errorf("err = %v, want errNotGitRepo", err)
	}
}

func TestRepoStateDirIsStableAndDistinct(t *testing.T) {
	base := t.TempDir()
	t.Setenv("SWARMIA_STATE_DIR", base)
	r1, r2 := initRepo(t), initRepo(t)

	d1, err := ensureRepoStateDir(r1)
	if err != nil {
		t.Fatal(err)
	}
	again, _ := repoStateDir(filepath.Join(r1, "."))
	d2, _ := repoStateDir(r2)
	if d1 != again {
		t.Errorf("same repo gave %s then %s", d1, again)
	}
	if d1 == d2 {
		t.Errorf("different repos share state dir %s", d1)
	}
	if !strings.HasPrefix(d1, filepath.Join(base, "repos")+string(filepath.Separator)) {
		t.Errorf("state dir %s not under %s/repos", d1, base)
	}
	meta, err := os.ReadFile(filepath.Join(d1, "repo.json"))
	if err != nil || !strings.Contains(string(meta), filepath.Join(r1, ".git")) {
		t.Errorf("repo.json = %q, %v; want it to name %s", meta, err, r1)
	}
}

func TestEventsPathResolution(t *testing.T) {
	base := t.TempDir()
	repo := initRepo(t)
	t.Setenv("SWARMIA_STATE_DIR", base)
	t.Setenv("CLAUDE_PROJECT_DIR", repo)
	t.Setenv("SWARMIA_LOG_FILE", "")

	stateDir, _ := repoStateDir(repo)
	if got, _ := eventsPath(""); got != filepath.Join(stateDir, "events.jsonl") {
		t.Errorf("default = %s, want events.jsonl in %s", got, stateDir)
	}
	t.Setenv("SWARMIA_LOG_FILE", "/tmp/env.jsonl")
	if got, _ := eventsPath(""); got != "/tmp/env.jsonl" {
		t.Errorf("env override = %s", got)
	}
	if got, _ := eventsPath("/tmp/flag.jsonl"); got != "/tmp/flag.jsonl" {
		t.Errorf("flag override = %s", got)
	}
}
