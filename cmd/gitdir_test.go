package cmd

import (
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

func mustGitState(t *testing.T, dir string) gitState {
	t.Helper()
	st, err := readGitState(dir)
	if err != nil {
		t.Fatalf("readGitState(%s): %v", dir, err)
	}
	return st
}

func TestReadGitStateLooseAndPackedRefs(t *testing.T) {
	repo := initRepo(t)
	want := gitState{Head: git(t, repo, "rev-parse", "HEAD"), Branch: "main"}
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
	if got := mustGitState(t, repo); got != (gitState{Head: sha}) {
		t.Errorf("got %+v, want detached at %s", got, sha)
	}
}

func TestReadGitStateUnbornBranch(t *testing.T) {
	dir := t.TempDir()
	git(t, dir, "init", "-q", "-b", "trunk")
	if got := mustGitState(t, dir); got != (gitState{Branch: "trunk"}) {
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

	want := gitState{Head: git(t, wt, "rev-parse", "HEAD"), Branch: "feature"}
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
	want := gitState{Head: git(t, dir, "rev-parse", "HEAD"), Branch: "main"}
	if got := mustGitState(t, dir); got != want {
		t.Errorf("got %+v, want %+v", got, want)
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
	var rec hookRecord
	if err := json.Unmarshal(data, &rec); err != nil {
		t.Fatal(err)
	}
	want := gitState{Head: git(t, repo, "rev-parse", "HEAD"), Branch: "main"}
	if rec.Git == nil || *rec.Git != want {
		t.Errorf("git = %+v, want %+v", rec.Git, want)
	}
}
