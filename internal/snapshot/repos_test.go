package snapshot

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/swarmia/swarmia-cli/internal/gitrepo"
)

// repoAt makes a repository with one commit in dir, returning its root.
func (f *fixture) repoAt(dir string) string {
	f.t.Helper()
	if err := os.MkdirAll(dir, 0o755); err != nil {
		f.t.Fatal(err)
	}
	f.git(dir, "init", "-q", "-b", "main")
	f.git(dir, "commit", "-q", "--allow-empty", "-m", "init")
	root, err := filepath.EvalSymlinks(dir)
	if err != nil {
		f.t.Fatal(err)
	}
	return root
}

func roots(repos []gitrepo.Repo) []string {
	var out []string
	for _, r := range repos {
		out = append(out, r.Root)
	}
	return out
}

// Scope: the working directory's worktree, then those the command names
// (relative or absolute, existing or about to be created, an option's value;
// not options or variables themselves), then the working set; each once.
func TestScope(t *testing.T) {
	f := newFixture(t)
	parent, _ := filepath.EvalSymlinks(t.TempDir())
	r := map[string]string{}
	for _, name := range []string{"a", "b", "c", "d", "e", "prefixed"} {
		r[name] = f.repoAt(filepath.Join(parent, name))
	}
	gone := filepath.Join(parent, "gone")

	command := "cd ../b && make -C ../c test; cat " + filepath.Join(r["a"], "x.go") + " > ../d/new/out.txt; echo $HOME ~/x README; npm --prefix=../prefixed run build"
	got := roots(Scope(command, r["a"], []string{r["e"], r["b"], gone}))
	if want := []string{r["a"], r["b"], r["c"], r["d"], r["prefixed"], r["e"]}; !reflect.DeepEqual(got, want) {
		t.Errorf("scope = %q\nwant      %q", got, want)
	}

	// Run from a directory that isn't a repository: only what it names.
	if got := roots(Scope("cd b && go test ./...", parent, nil)); !reflect.DeepEqual(got, []string{r["b"]}) {
		t.Errorf("from the parent: %q, want b only", got)
	}
	// Without a working directory, only the working set.
	if got := roots(Scope("make", "", []string{r["e"]})); !reflect.DeepEqual(got, []string{r["e"]}) {
		t.Errorf("without cwd: %q, want e only", got)
	}
}

// At most MaxRepos worktrees, those named first winning over the working set.
func TestScopeIsCapped(t *testing.T) {
	f := newFixture(t)
	parent, _ := filepath.EvalSymlinks(t.TempDir())
	var names, all []string
	for i := range MaxRepos + 2 {
		name := string(rune('a' + i))
		names = append(names, name)
		all = append(all, f.repoAt(filepath.Join(parent, name)))
	}
	got := roots(Scope("ls "+strings.Join(names, " "), parent, []string{f.repo.Root}))
	if !reflect.DeepEqual(got, all[:MaxRepos]) {
		t.Errorf("scope = %q, want the first %d named", got, MaxRepos)
	}
}

// Before and After work on every worktree at once; one that fails (removed
// meanwhile) is reported without losing the others' changes.
func TestBeforeAndAfterAcrossWorktrees(t *testing.T) {
	f := newFixture(t)
	parent, _ := filepath.EvalSymlinks(t.TempDir())
	a, b, c := f.repoAt(filepath.Join(parent, "a")), f.repoAt(filepath.Join(parent, "b")), f.repoAt(filepath.Join(parent, "c"))
	write := func(path, content string) {
		if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	write(filepath.Join(a, "dirty.go"), "package a\n") // dirty before the command
	var repos []gitrepo.Repo
	for _, root := range []string{a, b, c} {
		repo, _ := gitrepo.Find(root)
		repos = append(repos, repo)
	}
	pre, err := f.store.Before(context.Background(), repos)
	if err != nil || len(pre) != 3 {
		t.Fatalf("before: %d worktrees, %v", len(pre), err)
	}
	if err := f.store.Save("toolu_1", pre); err != nil {
		t.Fatal(err)
	}
	time.Sleep(10 * time.Millisecond)
	write(filepath.Join(a, "dirty.go"), "package a\n\nfunc A() {}\n")
	write(filepath.Join(b, "new.go"), "package b\n\nfunc B() {}\n")
	if err := os.RemoveAll(c); err != nil {
		t.Fatal(err)
	}

	loaded, _ := f.store.Load("toolu_1")
	changes, err := f.store.After(context.Background(), loaded)
	if err == nil {
		t.Error("removed worktree reported no error")
	}
	got := map[string][]string{}
	for root, cs := range changes {
		for _, ch := range cs {
			got[root] = append(got[root], ch.Path)
		}
	}
	if want := map[string][]string{a: {"dirty.go"}, b: {"new.go"}}; !reflect.DeepEqual(got, want) {
		t.Errorf("changes = %v, want %v", got, want)
	}
}

// Out of time, Before reports ErrBudget and keeps nothing it couldn't finish.
func TestBeforeStopsAtTheBudget(t *testing.T) {
	f := newFixture(t)
	f.commit(map[string]string{"a.go": "package a\n"})
	done, cancel := context.WithCancel(context.Background())
	cancel()
	pre, err := f.store.Before(done, []gitrepo.Repo{f.repo})
	if !errors.Is(err, ErrBudget) || len(pre) != 0 {
		t.Errorf("pre = %v, err = %v; want nothing and ErrBudget", pre, err)
	}
}

// A session's working set keeps the most recent worktrees first, each once,
// and isn't rewritten when nothing changes.
func TestWorkingSet(t *testing.T) {
	s := Store{Dir: t.TempDir()}
	s.Touch("sess", "/a")
	s.Touch("sess", "/b", "/c")
	s.Touch("sess", "/a", "")
	if got, want := s.WorkingSet("sess"), []string{"/a", "/b", "/c"}; !reflect.DeepEqual(got, want) {
		t.Errorf("working set = %q, want %q", got, want)
	}
	if got := s.WorkingSet("other"); got != nil {
		t.Errorf("another session's = %q", got)
	}

	old := time.Now().Add(-time.Hour)
	os.Chtimes(s.workingSetPath("sess"), old, old)
	s.Touch("sess", "/a", "/b")
	if fi, _ := os.Stat(s.workingSetPath("sess")); !fi.ModTime().Equal(old) {
		t.Error("unchanged working set rewritten")
	}

	for i := range maxWorkingSet + 5 {
		s.Touch("sess", "/r"+string(rune('a'+i)))
	}
	if got := s.WorkingSet("sess"); len(got) != maxWorkingSet || got[0] != "/r"+string(rune('a'+maxWorkingSet+4)) {
		t.Errorf("working set = %q, want the %d most recent", got, maxWorkingSet)
	}
}

// Each worktree keeps its own latest snapshot, so blobs are reused per
// worktree; pruning keeps blobs any of them uses, and drops what no session
// or worktree has used for a week.
func TestPruneKeepsEveryWorktreesBlobs(t *testing.T) {
	f := newFixture(t)
	parent, _ := filepath.EvalSymlinks(t.TempDir())
	other := f.repoAt(filepath.Join(parent, "other"))
	f.write("a.go", "package a\n")
	if err := os.WriteFile(filepath.Join(other, "b.go"), []byte("package b\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	otherRepo, _ := gitrepo.Find(other)
	pre, err := f.store.Before(context.Background(), []gitrepo.Repo{f.repo, otherRepo})
	if err != nil {
		t.Fatal(err)
	}
	f.store.Touch("old-session", f.repo.Root)
	week := time.Now().Add(-8 * 24 * time.Hour)
	var blobs []string
	for _, snap := range pre {
		for _, e := range snap.Files {
			blobs = append(blobs, f.store.blobPath(e.Blob))
			os.Chtimes(f.store.blobPath(e.Blob), week, week)
		}
	}
	os.Chtimes(f.store.workingSetPath("old-session"), week, week)
	os.WriteFile(filepath.Join(f.store.Dir, "last.json"), []byte("{}"), 0o600) // an older binary's

	f.store.Prune(time.Now())
	for _, blob := range blobs {
		if _, err := os.Stat(blob); err != nil {
			t.Errorf("blob in use pruned: %v", err)
		}
	}
	if len(blobs) != 2 {
		t.Errorf("blobs = %d, want one per worktree", len(blobs))
	}
	if got := f.store.WorkingSet("old-session"); got != nil {
		t.Errorf("week-old working set kept: %q", got)
	}
	if _, err := os.Stat(filepath.Join(f.store.Dir, "last.json")); err == nil {
		t.Error("older binary's last.json kept")
	}
	entries, _ := os.ReadDir(filepath.Join(f.store.Dir, "last"))
	if !slices.ContainsFunc(entries, func(de os.DirEntry) bool { return strings.HasSuffix(de.Name(), ".json") }) || len(entries) != 2 {
		t.Errorf("last snapshots = %v, want one per worktree", entries)
	}
}

// On a case-insensitive filesystem a word naming a worktree in another case
// (here from a heredoc's prose) is that worktree, snapshotted once.
func TestScopeSpellsWorktreesOnce(t *testing.T) {
	f := newFixture(t)
	parent, _ := filepath.EvalSymlinks(t.TempDir())
	aimo := f.repoAt(filepath.Join(parent, "aimo"))
	if _, err := os.Stat(filepath.Join(parent, "AIMO")); err != nil {
		t.Skip("the filesystem is case-sensitive")
	}
	command := "python3 - <<'EOF'\nprint('Why did Aimo get kicked out of the Slack channel?')\nopen('aimo/README.md', 'a')\nEOF"
	if got := roots(Scope(command, parent, []string{aimo})); !reflect.DeepEqual(got, []string{aimo}) {
		t.Errorf("scope = %q, want aimo once", got)
	}
}
