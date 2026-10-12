package snapshot

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/swarmia/swarmia-cli/internal/gitrepo"
)

type fixture struct {
	t     *testing.T
	repo  gitrepo.Repo
	store Store
}

func newFixture(t *testing.T) *fixture {
	dir, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	f := &fixture{t: t, store: Store{Dir: filepath.Join(t.TempDir(), "snapshots")}}
	f.git(dir, "init", "-q", "-b", "main")
	if f.repo, err = gitrepo.Find(dir); err != nil {
		t.Fatal(err)
	}
	return f
}

func (f *fixture) git(dir string, args ...string) {
	f.t.Helper()
	cmd := exec.Command("git", append([]string{"-C", dir}, args...)...)
	cmd.Env = append(os.Environ(), "GIT_AUTHOR_NAME=t", "GIT_AUTHOR_EMAIL=t@example.com",
		"GIT_COMMITTER_NAME=t", "GIT_COMMITTER_EMAIL=t@example.com",
		"GIT_CONFIG_GLOBAL=/dev/null", "GIT_CONFIG_NOSYSTEM=1")
	if out, err := cmd.CombinedOutput(); err != nil {
		f.t.Fatalf("git %v: %v\n%s", args, err, out)
	}
}

// gitMayFail runs git where failure is expected, e.g. a rebase that stops on
// a conflict, with env added to the fixture's.
func (f *fixture) gitMayFail(env []string, args ...string) {
	f.t.Helper()
	cmd := exec.Command("git", append([]string{"-C", f.repo.Root}, args...)...)
	cmd.Env = append(os.Environ(), "GIT_AUTHOR_NAME=t", "GIT_AUTHOR_EMAIL=t@example.com",
		"GIT_COMMITTER_NAME=t", "GIT_COMMITTER_EMAIL=t@example.com",
		"GIT_CONFIG_GLOBAL=/dev/null", "GIT_CONFIG_NOSYSTEM=1", "GIT_EDITOR=true")
	cmd.Env = append(cmd.Env, env...)
	_ = cmd.Run()
}

func (f *fixture) write(path, content string) {
	f.t.Helper()
	full := filepath.Join(f.repo.Root, path)
	if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
		f.t.Fatal(err)
	}
	if err := os.WriteFile(full, []byte(content), 0o644); err != nil {
		f.t.Fatal(err)
	}
}

func (f *fixture) commit(files map[string]string) {
	f.t.Helper()
	for p, c := range files {
		f.write(p, c)
	}
	f.git(f.repo.Root, "add", "-A")
	f.git(f.repo.Root, "commit", "-q", "-m", "c")
}

// run snapshots, applies the "command", and returns the authored lines per
// path (non-blank added lines only).
func (f *fixture) run(command func()) map[string][]string {
	f.t.Helper()
	pre, err := f.store.Take(context.Background(), f.repo)
	if err != nil {
		f.t.Fatal(err)
	}
	if err := f.store.Save("toolu_1", pre); err != nil {
		f.t.Fatal(err)
	}
	time.Sleep(10 * time.Millisecond) // distinct mtimes on coarse filesystems
	command()
	loaded, ok := f.store.Load("toolu_1")
	if !ok {
		f.t.Fatal("snapshot not found after Save")
	}
	changes, err := f.store.Changes(context.Background(), f.repo, loaded)
	if err != nil {
		f.t.Fatal(err)
	}
	got := map[string][]string{}
	for _, c := range changes {
		for _, h := range c.Hunks {
			for _, l := range h.Lines {
				if l.Added && strings.TrimSpace(l.Text) != "" {
					got[c.Path] = append(got[c.Path], l.Text)
				}
			}
		}
	}
	return got
}

func check(t *testing.T, got, want map[string][]string) {
	t.Helper()
	if len(want) == 0 && len(got) == 0 {
		return
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("authored = %q\nwant      %q", got, want)
	}
}

func TestCommandCreatesFile(t *testing.T) {
	f := newFixture(t)
	f.commit(map[string]string{"README": "hi\n"})
	got := f.run(func() { f.write("gen/api.go", "package gen\n\nfunc Generated() {}\n") })
	check(t, got, map[string][]string{"gen/api.go": {"package gen", "func Generated() {}"}})
}

func TestCommandAppendsToCleanFile(t *testing.T) {
	f := newFixture(t)
	f.commit(map[string]string{"app.go": "package app\n\nfunc A() {}\n"})
	got := f.run(func() { f.write("app.go", "package app\n\nfunc A() {}\n\nfunc B() { return }\n") })
	check(t, got, map[string][]string{"app.go": {"func B() { return }"}})
}

// The human's uncommitted edits from before the command aren't the command's.
func TestCommandEditsAlreadyDirtyFile(t *testing.T) {
	f := newFixture(t)
	f.commit(map[string]string{"app.go": "package app\n"})
	f.write("app.go", "package app\n\nfunc Human() {}\n")
	got := f.run(func() { f.write("app.go", "package app\n\nfunc Human() {}\n\nfunc Agent() {}\n") })
	check(t, got, map[string][]string{"app.go": {"func Agent() {}"}})
}

func TestFormatterRunAuthorsNothing(t *testing.T) {
	f := newFixture(t)
	f.commit(map[string]string{"x.js": "const x = {a:1,b:2}\nfoo( x )\n"})
	got := f.run(func() { f.write("x.js", "const x = { a: 1, b: 2 };\nfoo(x);\n") })
	check(t, got, nil)
}

func TestGitStashAuthorsNothing(t *testing.T) {
	f := newFixture(t)
	f.commit(map[string]string{"app.go": "package app\n\nfunc A() {}\n"})
	f.write("app.go", "package app\n") // human removed A
	got := f.run(func() { f.git(f.repo.Root, "stash", "-q") })
	check(t, got, nil)
}

// Committing inside the command moves HEAD but changes no content.
func TestGitCommitAuthorsNothing(t *testing.T) {
	f := newFixture(t)
	f.commit(map[string]string{"app.go": "package app\n"})
	f.write("app.go", "package app\n\nfunc Human() {}\n")
	got := f.run(func() {
		f.git(f.repo.Root, "add", "-A")
		f.git(f.repo.Root, "commit", "-q", "-m", "wip")
	})
	check(t, got, nil)
}

func TestUntouchedDirtyAndBinaryFilesAreSkipped(t *testing.T) {
	f := newFixture(t)
	f.commit(map[string]string{"a.go": "package a\n"})
	f.write("a.go", "package a\n\nfunc Dirty() {}\n")
	got := f.run(func() { f.write("img.bin", "PNG\x00\x01binary\n") })
	check(t, got, nil)
}

// A second snapshot reuses stored blobs for files that didn't change.
func TestTakeReusesBlobsForUnchangedFiles(t *testing.T) {
	f := newFixture(t)
	f.commit(map[string]string{"a.go": "package a\n"})
	f.write("a.go", "package a\n\nfunc Dirty() {}\n")
	first, err := f.store.Take(context.Background(), f.repo)
	if err != nil {
		t.Fatal(err)
	}
	blob := f.store.blobPath(first.Files["a.go"].Blob)
	old := time.Now().Add(-time.Hour)
	if err := os.Chtimes(blob, old, old); err != nil {
		t.Fatal(err)
	}
	second, _ := f.store.Take(context.Background(), f.repo)
	if second.Files["a.go"].Blob != first.Files["a.go"].Blob {
		t.Fatal("blob changed for an unchanged file")
	}
	if fi, _ := os.Stat(blob); !fi.ModTime().Equal(old) {
		t.Error("blob was rewritten instead of reused")
	}
}

func TestPruneRemovesStalePendingSnapshots(t *testing.T) {
	f := newFixture(t)
	if err := f.store.Save("stale", Snapshot{}); err != nil {
		t.Fatal(err)
	}
	if err := f.store.Save("fresh", Snapshot{}); err != nil {
		t.Fatal(err)
	}
	old := time.Now().Add(-48 * time.Hour)
	if err := os.Chtimes(f.store.pendingPath("stale"), old, old); err != nil {
		t.Fatal(err)
	}
	f.store.Prune(time.Now())
	if _, ok := f.store.Load("stale"); ok {
		t.Error("stale snapshot survived pruning")
	}
	if _, ok := f.store.Load("fresh"); !ok {
		t.Error("fresh snapshot was pruned")
	}
}

// Out of time, both sides give up with ErrBudget rather than record partial
// (or, with a base missing, inflated) changes.
func TestSnapshotsStopAtTheBudget(t *testing.T) {
	f := newFixture(t)
	f.commit(map[string]string{"a.go": "package a\n"})
	f.write("a.go", "package a\n\nfunc A() {}\n")
	pre, err := f.store.Take(context.Background(), f.repo)
	if err != nil {
		t.Fatal(err)
	}
	done, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := f.store.Take(done, f.repo); !errors.Is(err, ErrBudget) {
		t.Errorf("Take err = %v, want ErrBudget", err)
	}
	if _, err := f.store.Changes(done, f.repo, pre); !errors.Is(err, ErrBudget) {
		t.Errorf("Changes err = %v, want ErrBudget", err)
	}
}

// branches sets up main and feature diverging from a common base: feature adds
// a function to app.go, and both change conflict.txt. HEAD ends on feature.
func (f *fixture) branches() {
	f.t.Helper()
	f.commit(map[string]string{"app.go": "package app\n", "conflict.txt": "base\n"})
	f.git(f.repo.Root, "checkout", "-q", "-b", "feature")
	f.commit(map[string]string{"app.go": "package app\n\nfunc Feature() { return computeFeature() }\n", "conflict.txt": "feature\n"})
	f.git(f.repo.Root, "checkout", "-q", "main")
	f.commit(map[string]string{"conflict.txt": "main\n", "other.go": "package app\n"})
	f.git(f.repo.Root, "checkout", "-q", "feature")
}

// A rebase stopped on a conflict leaves the commit it's replaying staged
// but uncommitted: those lines, and the conflict markers, are git's.
func TestStoppedRebaseAuthorsNothing(t *testing.T) {
	f := newFixture(t)
	f.branches()
	got := f.run(func() { f.gitMayFail(nil, "rebase", "main") })
	if _, busy := f.repo.InProgress(); !busy {
		t.Fatal("rebase didn't stop")
	}
	check(t, got, nil)
}

func TestStoppedCherryPickAuthorsNothing(t *testing.T) {
	f := newFixture(t)
	f.branches()
	f.git(f.repo.Root, "checkout", "-q", "main")
	got := f.run(func() { f.gitMayFail(nil, "cherry-pick", "feature") })
	check(t, got, nil)
}

func TestStoppedMergeAuthorsNothing(t *testing.T) {
	f := newFixture(t)
	f.branches()
	f.git(f.repo.Root, "checkout", "-q", "main")
	got := f.run(func() { f.gitMayFail(nil, "merge", "-q", "feature") })
	check(t, got, nil)
}

// While a rebase is stopped, the agent's own resolution still counts.
func TestEditDuringStoppedRebaseIsAuthored(t *testing.T) {
	f := newFixture(t)
	f.branches()
	f.gitMayFail(nil, "rebase", "main")
	got := f.run(func() { f.write("conflict.txt", "main\nfeature\nresolved by the agent\n") })
	check(t, got, map[string][]string{"conflict.txt": {"resolved by the agent"}})
}

// A rebase stopped at `break` names no commit; whatever the command left in
// the worktree can't be told apart from git's, so nothing is recorded.
func TestRebaseStoppedWithoutSourceFailsClosed(t *testing.T) {
	f := newFixture(t)
	f.branches()
	editor := filepath.Join(t.TempDir(), "break.sh") // puts a `break` first in the todo list
	if err := os.WriteFile(editor, []byte("#!/bin/sh\n{ echo break; cat \"$1\"; } > \"$1.new\" && mv \"$1.new\" \"$1\"\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	pre, err := f.store.Take(context.Background(), f.repo)
	if err != nil {
		t.Fatal(err)
	}
	f.gitMayFail([]string{"GIT_SEQUENCE_EDITOR=" + editor}, "rebase", "-i", "HEAD~1")
	if op, busy := f.repo.InProgress(); !busy || len(op.Sources) != 0 {
		t.Fatalf("want a rebase stopped at break, got busy=%v %+v", busy, op)
	}
	if _, err := f.store.Changes(context.Background(), f.repo, pre); !errors.Is(err, ErrUnknownSource) {
		t.Errorf("Changes err = %v, want ErrUnknownSource", err)
	}
}

// Popping a stash restores earlier changes, untracked files included.
func TestGitStashPopAuthorsNothing(t *testing.T) {
	f := newFixture(t)
	f.commit(map[string]string{"app.go": "package app\n"})
	f.write("app.go", "package app\n\nfunc Stashed() { return earlierWork() }\n")
	f.write("new.go", "package app\n\nfunc Untracked() { return earlierWork() }\n")
	f.git(f.repo.Root, "stash", "-q", "-u")
	got := f.run(func() { f.git(f.repo.Root, "stash", "pop", "-q") })
	check(t, got, nil)
}
