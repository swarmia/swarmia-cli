package cmd

import (
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/swarmia/swarmia-cli/internal/events"
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

// Worktrees of one clone share a state directory.
func TestRepoStateDirSharedByWorktrees(t *testing.T) {
	repo := initRepo(t)
	wt := filepath.Join(filepath.Dir(repo), filepath.Base(repo)+"-wt")
	git(t, repo, "worktree", "add", "-q", wt, "-b", "feature")
	t.Cleanup(func() { os.RemoveAll(wt) })

	t.Setenv("SWARMIA_STATE_DIR", t.TempDir())
	a, _ := repoStateDir(repo)
	b, _ := repoStateDir(wt)
	if a != b {
		t.Errorf("worktree state dir %s differs from main repo's %s", b, a)
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

func TestRunLogHookRecordsGitState(t *testing.T) {
	repo := initRepo(t)
	t.Setenv("CLAUDE_PROJECT_DIR", repo)
	path := filepath.Join(t.TempDir(), "events.jsonl")
	t.Setenv("SWARMIA_LOG_FILE", path)

	if err := runLogHook(strings.NewReader(`{"hook_event_name":"PostToolUse"}`)); err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var rec events.Record
	if err := json.Unmarshal(data, &rec); err != nil {
		t.Fatal(err)
	}
	want := events.Git{Root: repo, Head: git(t, repo, "rev-parse", "HEAD"), Branch: "main"}
	if rec.Git == nil || *rec.Git != want {
		t.Errorf("git = %+v, want %+v", rec.Git, want)
	}
}

// The email sent with sessions, and used to recognise the developer's
// commits, is the one their commits carry: $GIT_AUTHOR_EMAIL over user.email.
// Without either it's empty, never git's guess from the user and host names.
func TestAuthorEmail(t *testing.T) {
	repo := initRepo(t)
	t.Setenv("GIT_CONFIG_GLOBAL", "/dev/null")
	t.Setenv("GIT_CONFIG_NOSYSTEM", "1")
	t.Setenv("GIT_AUTHOR_EMAIL", "")
	os.Unsetenv("GIT_AUTHOR_EMAIL")
	t.Setenv("EMAIL", "")
	os.Unsetenv("EMAIL")

	if got := authorEmail(repo); got != "" {
		t.Errorf("unconfigured: %q, want none (not a guess)", got)
	}
	git(t, repo, "config", "user.email", "dev@example.com")
	if got := authorEmail(repo); got != "dev@example.com" {
		t.Errorf("user.email without user.name: %q, want dev@example.com", got)
	}
	git(t, repo, "config", "user.name", "Dev <Name>")
	if got := authorEmail(repo); got != "dev@example.com" {
		t.Errorf("user.email: %q, want dev@example.com", got)
	}
	t.Setenv("GIT_AUTHOR_EMAIL", "work@example.com")
	if got := authorEmail(repo); got != "work@example.com" {
		t.Errorf("GIT_AUTHOR_EMAIL: %q, want work@example.com", got)
	}
}
