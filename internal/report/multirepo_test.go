package report

import (
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/swarmia/swarmia-cli/internal/events"
)

// writeEvent logs a hook event of session sess-1 into recs, with no git field
// (as log-hook writes outside a repository) unless gitRoot is set.
func writeEvent(t *testing.T, recs *[]events.Record, at time.Time, gitRoot string, event map[string]any) {
	t.Helper()
	event["session_id"] = "sess-1"
	raw, err := json.Marshal(event)
	if err != nil {
		t.Fatal(err)
	}
	rec := events.Record{LoggedAt: at.Format(time.RFC3339), Event: raw}
	if gitRoot != "" {
		rec.Git = &events.Git{Root: gitRoot}
	}
	*recs = append(*recs, rec)
}

// writeTool is a Claude Code Write of path with content, into a new file.
func writeTool(path, content string) map[string]any {
	return map[string]any{
		"hook_event_name": "PostToolUse", "tool_name": "Write",
		"tool_input": map[string]any{"file_path": path, "content": content},
	}
}

// commitFiles summarizes attributed commits as (root, sha, path, lines).
type commitFile struct {
	Root, SHA, Path string
	Lines           int
}

func commitFiles(s *Session) []commitFile {
	var out []commitFile
	for _, c := range s.Commits() {
		for _, f := range c.Files {
			out = append(out, commitFile{c.Root, c.SHA, f.Path, f.Total()})
		}
	}
	return out
}

func all(Commit) bool { return true }

// A session started in one repository that also edits a sibling (here through
// a symlink to it): each edit is matched to commits in its own repository.
func TestBuildAttributesEditsInEachRepository(t *testing.T) {
	app, lib := newFixture(t), newFixture(t)
	app.commit(t0.Add(-30*time.Minute), "me@example.com", "base", map[string]string{"app.go": "package app\n"})
	lib.commit(t0.Add(-30*time.Minute), "me@example.com", "base", map[string]string{"lib.go": "package lib\n"})
	link := filepath.Join(t.TempDir(), "lib")
	if err := os.Symlink(lib.root, link); err != nil {
		t.Fatal(err)
	}

	appCode := lines(3, "func app%d() string { return \"from the app\" }")
	libCode := lines(4, "func lib%d() string { return \"from the library\" }")
	app.agentWrite(t0.Add(10*time.Second), "app.go", "package app\n"+appCode, "package app\n")
	app.log(t0.Add(20*time.Second), map[string]any{
		"hook_event_name": "PostToolUse", "tool_name": "Write",
		"tool_input":    map[string]any{"file_path": filepath.Join(link, "lib.go"), "content": "package lib\n" + libCode},
		"tool_response": map[string]any{"originalFile": "package lib\n"},
	})
	appSHA := app.commit(t0.Add(time.Minute), "me@example.com", "app", map[string]string{"app.go": "package app\n" + appCode})
	libSHA := lib.commit(t0.Add(2*time.Minute), "me@example.com", "lib", map[string]string{"lib.go": "package lib\n" + libCode})
	// A teammate's commit in the library: filtered out by author there too.
	lib.commit(t0.Add(3*time.Minute), "other@example.com", "teammate", map[string]string{"lib.go": "package lib\n" + libCode + "var x = 1\n"})

	sessions, err := Build(app.recs, Options{FallbackRoot: app.root, AuthorEmail: emailIs("me@example.com")})
	if err != nil {
		t.Fatal(err)
	}
	s := sessions[0]
	want := []commitFile{{app.root, appSHA, "app.go", 3}, {lib.root, libSHA, "lib.go", 4}}
	if got := commitFiles(s); !reflect.DeepEqual(got, want) {
		t.Errorf("commits = %+v, want %+v", got, want)
	}
	if s.AILinesIn(app.root) != 3 || s.AILinesIn(lib.root) != 4 {
		t.Errorf("AI lines by root = %v", s.AILinesByRoot)
	}
	if n := s.Survived(all); n != 7 {
		t.Errorf("survived = %d, want 7", n)
	}
}

// A session started in a directory holding several repositories, which isn't
// one itself, is attributed in each repository it edited.
func TestBuildAttributesSessionOutsideRepository(t *testing.T) {
	parent := t.TempDir()
	a, b := newFixtureAt(t, filepath.Join(parent, "a")), newFixtureAt(t, filepath.Join(parent, "b"))
	codeA, codeB := lines(2, "var a%d = \"written in a\""), lines(2, "var b%d = \"written in b\"")
	var recs []events.Record
	for i, f := range []*fixture{a, b} {
		code := []string{codeA, codeB}[i]
		writeEvent(t, &recs, t0.Add(time.Duration(i)*time.Second), "", writeTool(filepath.Join(parent, filepath.Base(f.root), "x.go"), code))
		f.commit(t0.Add(time.Minute), "me@example.com", "x", map[string]string{"x.go": code})
	}

	sessions, err := Build(recs, Options{})
	if err != nil {
		t.Fatal(err)
	}
	s := sessions[0]
	if len(s.Commits()) != 2 || s.Commits()[0].Root != a.root || s.Commits()[1].Root != b.root {
		t.Fatalf("commits = %+v, want one in each repository", s.Commits())
	}
	if s.AILines() != 4 || s.Survived(all) != 4 {
		t.Errorf("AI lines = %d, survived = %d, want 4 and 4", s.AILines(), s.Survived(all))
	}
}

// Locating files: through symlinks, into directories that don't exist (any
// more), and into the innermost repository; files in no repository aren't
// located.
func TestLocator(t *testing.T) {
	outer := newFixture(t)
	inner := newFixtureAt(t, filepath.Join(outer.root, "vendor", "inner"))
	plain := t.TempDir()
	link := filepath.Join(t.TempDir(), "outer")
	if err := os.Symlink(outer.root, link); err != nil {
		t.Fatal(err)
	}
	locate := newLocator().locate
	for _, tc := range []struct {
		path, root, rel string
		ok              bool
	}{
		{filepath.Join(outer.root, "a.go"), outer.root, "a.go", true},
		{filepath.Join(link, "pkg", "b.go"), outer.root, "pkg/b.go", true},
		{filepath.Join(link, "gone", "deeper", "c.go"), outer.root, "gone/deeper/c.go", true},
		{filepath.Join(inner.root, "d.go"), inner.root, "d.go", true},
		{filepath.Join(link, "vendor", "inner", "x", "e.go"), inner.root, "x/e.go", true},
		{filepath.Join(plain, "f.go"), "", "", false},
	} {
		root, rel, ok := locate(tc.path)
		if ok != tc.ok || ok && (root != tc.root || rel != tc.rel) {
			t.Errorf("locate(%s) = %q, %q, %v; want %q, %q, %v", tc.path, root, rel, ok, tc.root, tc.rel, tc.ok)
		}
	}
}

// An edit outside every repository isn't recorded as an edit, but the tool
// call still is.
func TestFoldSkipsEditsOutsideRepositories(t *testing.T) {
	path := filepath.Join(t.TempDir(), "notes.md")
	var recs []events.Record
	writeEvent(t, &recs, t0, "", writeTool(path, "some notes\n"))
	sessions := map[string]*Session{}
	Fold(sessions, recs, "")
	s := sessions["sess-1"]
	if len(s.Edits) != 0 || s.AILines() != 0 || len(s.Tools) != 1 || s.Tools[0].File != path {
		t.Errorf("edits = %+v, AI lines = %d, tools = %+v", s.Edits, s.AILines(), s.Tools)
	}
}

// A Codex patch run from a directory holding two repositories edits both;
// each file lands in its own repository.
func TestFoldPlacesCodexPatchFilesInTheirRepositories(t *testing.T) {
	parent := t.TempDir()
	a, b := newFixtureAt(t, filepath.Join(parent, "a")), newFixtureAt(t, filepath.Join(parent, "b"))
	patch := "*** Begin Patch\n*** Add File: a/x.go\n+var x = 1\n*** Add File: b/y.go\n+var y = 2\n+var z = 3\n*** End Patch"
	raw, _ := json.Marshal(map[string]any{"session_id": "s", "hook_event_name": "PostToolUse", "tool_name": "apply_patch",
		"cwd": parent, "tool_input": map[string]any{"command": patch}})
	sessions := map[string]*Session{}
	Fold(sessions, []events.Record{{LoggedAt: t0.Format(time.RFC3339), Agent: "codex", Event: raw}}, "")
	s := sessions["s"]
	if len(s.Edits) != 2 || s.Edits[0].Root != a.root || s.Edits[0].Path != "x.go" || s.Edits[1].Root != b.root || s.Edits[1].Path != "y.go" {
		t.Fatalf("edits = %+v", s.Edits)
	}
	if s.AILinesIn(a.root) != 1 || s.AILinesIn(b.root) != 2 {
		t.Errorf("AI lines = %v", s.AILinesByRoot)
	}
	if len(s.Tools) != 1 || s.Tools[0].File != filepath.Join(a.root, "x.go") {
		t.Errorf("tools = %+v", s.Tools)
	}
}

// Files of the same path in two repositories are separate files: their AI
// lines are counted, and matched, apart. In b the file already holds the lines
// the agent wrote in a, which aren't the agent's there.
func TestSamePathInTwoRepositories(t *testing.T) {
	a, b := newFixture(t), newFixture(t)
	codeA, codeB := lines(2, "var shared%d = \"version a\""), lines(3, "var shared%d = \"version b\"")
	var recs []events.Record
	writeEvent(t, &recs, t0, a.root, writeTool(filepath.Join(a.root, "main.go"), codeA))
	appendB := writeTool(filepath.Join(b.root, "main.go"), codeA+codeB)
	appendB["tool_response"] = map[string]any{"originalFile": codeA}
	writeEvent(t, &recs, t0.Add(time.Second), a.root, appendB)
	shaA := a.commit(t0.Add(time.Minute), "me@example.com", "a", map[string]string{"main.go": codeA})
	// b commits a's version: none of b's agent lines are in it.
	b.commit(t0.Add(time.Minute), "me@example.com", "b", map[string]string{"main.go": codeA})

	sessions, err := Build(recs, Options{FallbackRoot: a.root})
	if err != nil {
		t.Fatal(err)
	}
	s := sessions[0]
	if s.AILinesIn(a.root) != 2 || s.AILinesIn(b.root) != 3 {
		t.Errorf("AI lines = %v", s.AILinesByRoot)
	}
	if got, want := commitFiles(s), []commitFile{{a.root, shaA, "main.go", 2}}; !reflect.DeepEqual(got, want) {
		t.Errorf("commits = %+v, want %+v", got, want)
	}
}

// Edits in two worktrees of one clone are matched against its commits once:
// a commit holding both files is listed once.
func TestBuildMatchesWorktreesOfOneRepositoryOnce(t *testing.T) {
	main := newFixture(t)
	main.commit(t0.Add(-time.Hour), "me@example.com", "base", map[string]string{"README": "hi\n"})
	wt := filepath.Join(t.TempDir(), "wt")
	main.git(t0.Add(-time.Hour), "me@example.com", "worktree", "add", "-q", "-b", "feature", wt)
	wt, _ = filepath.EvalSymlinks(wt)
	one, two := lines(2, "var one%d = \"first file\""), lines(2, "var two%d = \"second file\"")

	var recs []events.Record
	writeEvent(t, &recs, t0, main.root, writeTool(filepath.Join(main.root, "one.go"), one))
	writeEvent(t, &recs, t0.Add(time.Second), main.root, writeTool(filepath.Join(wt, "two.go"), two))
	sha := main.commit(t0.Add(time.Minute), "me@example.com", "both", map[string]string{"one.go": one, "two.go": two})

	sessions, err := Build(recs, Options{FallbackRoot: main.root})
	if err != nil {
		t.Fatal(err)
	}
	s := sessions[0]
	if len(s.Commits()) != 1 || s.Commits()[0].SHA != sha || len(s.Commits()[0].Files) != 2 {
		t.Fatalf("commits = %+v, want %s once, with both files", s.Commits(), sha[:7])
	}
	if s.Edits[1].Root != wt || s.Survived(all) != 4 {
		t.Errorf("edit roots %q, %q; survived %d", s.Edits[0].Root, s.Edits[1].Root, s.Survived(all))
	}
}

// A session that edited only in one worktree of a clone gets its commits
// labeled with that worktree, not with another session's of the same clone.
func TestCommitsKeepTheSessionsOwnWorktree(t *testing.T) {
	main := newFixture(t)
	main.commit(t0.Add(-time.Hour), "me@example.com", "base", map[string]string{"README": "hi\n"})
	wt := filepath.Join(t.TempDir(), "wt")
	main.git(t0.Add(-time.Hour), "me@example.com", "worktree", "add", "-q", "-b", "feature", wt)
	wt, _ = filepath.EvalSymlinks(wt)
	code := lines(2, "var inWorktree%d = \"second session\"")

	var recs []events.Record
	raw, _ := json.Marshal(map[string]any{"session_id": "sess-2", "hook_event_name": "PostToolUse", "tool_name": "Write",
		"tool_input": map[string]any{"file_path": filepath.Join(wt, "two.go"), "content": code}})
	recs = append(recs, events.Record{LoggedAt: t0.Format(time.RFC3339), Event: raw, Git: &events.Git{Root: wt}})
	// The other session is newer, so it comes first.
	writeEvent(t, &recs, t0.Add(time.Second), main.root, writeTool(filepath.Join(main.root, "one.go"), lines(2, "var inMain%d = 1")))
	sha := (&fixture{t: t, root: wt}).commit(t0.Add(time.Minute), "me@example.com", "two", map[string]string{"two.go": code})

	sessions, err := Build(recs, Options{FallbackRoot: main.root})
	if err != nil {
		t.Fatal(err)
	}
	for _, s := range sessions {
		if s.ID != "sess-2" {
			continue
		}
		if got, want := commitFiles(s), []commitFile{{wt, sha, "two.go", 2}}; !reflect.DeepEqual(got, want) {
			t.Errorf("commits = %+v, want %+v", got, want)
		}
		return
	}
	t.Fatal("no sess-2")
}

// A session's attribution is only recomputed when its own repositories'
// commits change, not when another session starts editing elsewhere.
func TestProcessRecomputesOnlyForOwnRepositories(t *testing.T) {
	a, b := newFixture(t), newFixture(t)
	state := t.TempDir()
	logPath := filepath.Join(state, "events.jsonl")
	a.commit(t0.Add(-time.Hour), "me@example.com", "base", map[string]string{"README": "hi\n"})
	b.commit(t0.Add(-time.Hour), "me@example.com", "base", map[string]string{"README": "hi\n"})
	code := lines(2, "var fromA%d = \"agent\"")
	a.agentWrite(t0, "a.go", code, nil)
	writeLog(t, logPath, a.recs)
	res := a.process(t, state, t0.Add(time.Hour))
	computed := res.Sessions[0].Attribution.ComputedAt

	var recs []events.Record
	raw, _ := json.Marshal(map[string]any{"session_id": "sess-2", "hook_event_name": "PostToolUse", "tool_name": "Write",
		"tool_input": map[string]any{"file_path": filepath.Join(b.root, "b.go"), "content": "var b = 1\n"}})
	recs = append(recs, events.Record{LoggedAt: t0.Add(time.Hour).Format(time.RFC3339), Event: raw})
	writeLog(t, logPath, recs)
	b.commit(t0.Add(2*time.Hour), "me@example.com", "b", map[string]string{"b.go": "var b = 1\n"})
	res = a.process(t, state, t0.Add(2*time.Hour))
	for _, s := range res.Sessions {
		if s.ID == "sess-1" && !s.Attribution.ComputedAt.Equal(computed) {
			t.Errorf("sess-1 recomputed after another session edited another repository")
		}
		if s.ID == "sess-2" && len(s.Commits()) != 1 {
			t.Errorf("sess-2 commits = %+v, want 1", s.Commits())
		}
	}
}

// A repository that can no longer be read (its worktree was deleted) keeps
// the commits it had, while the session's other repositories are matched
// again.
func TestAttributeKeepsCommitsOfUnreadableRepository(t *testing.T) {
	a := newFixture(t)
	b := newFixtureAt(t, filepath.Join(t.TempDir(), "b"))
	codeA, codeB := lines(2, "var a%d = \"agent in a\""), lines(2, "var b%d = \"agent in b\"")
	var recs []events.Record
	writeEvent(t, &recs, t0, a.root, writeTool(filepath.Join(a.root, "a.go"), codeA))
	writeEvent(t, &recs, t0.Add(time.Second), a.root, writeTool(filepath.Join(b.root, "b.go"), codeB))
	shaB := b.commit(t0.Add(time.Minute), "me@example.com", "b", map[string]string{"b.go": codeB})

	sessions, err := Build(recs, Options{FallbackRoot: a.root})
	if err != nil {
		t.Fatal(err)
	}
	s := sessions[0]
	if got := commitFiles(s); len(got) != 1 || got[0].SHA != shaB {
		t.Fatalf("commits = %+v, want %s", got, shaB[:7])
	}

	if err := os.RemoveAll(b.root); err != nil {
		t.Fatal(err)
	}
	shaA := a.commit(t0.Add(2*time.Minute), "me@example.com", "a", map[string]string{"a.go": codeA})
	if err := Attribute(sessions, Options{FallbackRoot: a.root}, t0.Add(time.Hour)); err != nil {
		t.Fatal(err)
	}
	want := []commitFile{{b.root, shaB, "b.go", 2}, {a.root, shaA, "a.go", 2}}
	if got := commitFiles(s); !reflect.DeepEqual(got, want) {
		t.Errorf("commits = %+v, want %+v", got, want)
	}
}

// Sessions stored by an older swarmia-cli (format 2) had everything in the
// project's worktree: they are read into it, and saved in the new format.
func TestProcessUpgradesSessionsFromFormat2(t *testing.T) {
	f := newFixture(t)
	state := t.TempDir()
	code := lines(3, "var old%d = \"from an older binary\"")
	sha := f.commit(t0.Add(time.Minute), "me@example.com", "old", map[string]string{"old.go": code})
	hunk := []string{}
	for _, l := range splitLines(code) {
		hunk = append(hunk, "+"+l)
	}
	stored := map[string]any{
		"format": 2, "id": "old-session", "agent": "claude-code",
		"started_at": t0, "ended_at": t0,
		"tools": []map[string]any{
			{"at": t0, "name": "Bash", "command": "make"},
			{"at": t0, "name": "Edit", "file": filepath.Join(f.root, "old.go")},
			{"at": t0, "name": "Read", "file": "/elsewhere/notes.md"},
		},
		"edits":            []map[string]any{{"at": t0, "path": "old.go", "hunks": [][]string{hunk}}},
		"ai_lines_by_path": map[string]int{"old.go": 3},
	}
	data, _ := json.Marshal(stored)
	if err := os.MkdirAll(filepath.Join(state, "sessions"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(state, "sessions", "old-session.json"), data, 0o600); err != nil {
		t.Fatal(err)
	}

	res := f.process(t, state, t0.Add(time.Hour))
	s := res.Sessions[0]
	if s.Root != f.root || s.Edits[0].Root != f.root || s.AILinesIn(f.root) != 3 || s.AILinesByPath != nil {
		t.Errorf("root = %q, edits = %+v, AI lines = %v / %v", s.Root, s.Edits, s.AILinesByRoot, s.AILinesByPath)
	}
	if s.Tools[0].Root != f.root || s.Tools[1].Root != f.root || s.Tools[2].Root != "" {
		t.Errorf("tools = %+v; want the project's root but for the file elsewhere", s.Tools)
	}
	if got, want := commitFiles(s), []commitFile{{f.root, sha, "old.go", 3}}; !reflect.DeepEqual(got, want) {
		t.Errorf("commits = %+v, want %+v", got, want)
	}
	saved, err := os.ReadFile(filepath.Join(state, "sessions", "old-session.json"))
	if err != nil {
		t.Fatal(err)
	}
	var back Session
	if err := json.Unmarshal(saved, &back); err != nil || back.Format != SessionFormat || back.Edits[0].Root != f.root {
		t.Errorf("saved as format %d with edit root %q (%v)", back.Format, back.Edits[0].Root, err)
	}
}

func splitLines(s string) []string {
	return strings.Split(strings.TrimSuffix(s, "\n"), "\n")
}

// A session records the worktree it was started in, and each tool call the
// worktree it worked in: its file's, else its working directory's (from the
// event), else the project's.
func TestFoldRecordsWhereToolsWorked(t *testing.T) {
	a, b := newFixture(t), newFixture(t)
	elsewhere := t.TempDir()
	var recs []events.Record
	tool := func(name string, extra map[string]any) map[string]any {
		ev := map[string]any{"hook_event_name": "PostToolUse", "tool_name": name}
		for k, v := range extra {
			ev[k] = v
		}
		return ev
	}
	writeEvent(t, &recs, t0, a.root, map[string]any{"hook_event_name": "SessionStart"})
	writeEvent(t, &recs, t0, a.root, tool("Bash", map[string]any{"cwd": filepath.Join(b.root, "pkg"), "tool_input": map[string]any{"command": "go test"}}))
	writeEvent(t, &recs, t0, a.root, tool("Read", map[string]any{"cwd": a.root, "tool_input": map[string]any{"file_path": filepath.Join(b.root, "x.go")}}))
	writeEvent(t, &recs, t0, a.root, tool("Read", map[string]any{"cwd": a.root, "tool_input": map[string]any{"file_path": filepath.Join(elsewhere, "y.md")}}))
	writeEvent(t, &recs, t0, a.root, tool("Bash", map[string]any{"cwd": elsewhere, "tool_input": map[string]any{"command": "ls"}}))
	writeEvent(t, &recs, t0, a.root, tool("Grep", nil)) // no file, no cwd
	sessions := map[string]*Session{}
	Fold(sessions, recs, "")
	s := sessions["sess-1"]
	var roots []string
	for _, tl := range s.Tools {
		roots = append(roots, tl.Root)
	}
	if want := []string{b.root, b.root, "", "", a.root}; s.Root != a.root || !reflect.DeepEqual(roots, want) {
		t.Errorf("session root %q, tool roots %q; want %q, %q", s.Root, roots, a.root, want)
	}
}

// On a case-insensitive filesystem, edits through a path in another case are
// in the worktree as the filesystem spells it: one file, one commit.
func TestFoldSpellsWorktreesOnce(t *testing.T) {
	parent, _ := filepath.EvalSymlinks(t.TempDir())
	aimo := newFixtureAt(t, filepath.Join(parent, "aimo"))
	if _, err := os.Stat(filepath.Join(parent, "AIMO")); err != nil {
		t.Skip("the filesystem is case-sensitive")
	}
	joke := "Why did Aimo get kicked out of the Slack channel?\n"
	var recs []events.Record
	writeEvent(t, &recs, t0, "", writeTool(filepath.Join(parent, "aimo", "README.md"), joke))
	writeEvent(t, &recs, t0.Add(time.Second), "", writeTool(filepath.Join(parent, "Aimo", "README.md"), joke+joke))
	sha := aimo.commit(t0.Add(time.Minute), "me@example.com", "joke", map[string]string{"README.md": joke + joke})

	sessions, err := Build(recs, Options{})
	if err != nil {
		t.Fatal(err)
	}
	s := sessions[0]
	if len(s.AILinesByRoot) != 1 || s.Edits[1].Root != aimo.root {
		t.Errorf("AI lines = %v, edit roots %q, %q; want aimo alone", s.AILinesByRoot, s.Edits[0].Root, s.Edits[1].Root)
	}
	if got := commitFiles(s); len(got) != 1 || got[0].SHA != sha || got[0].Root != aimo.root {
		t.Errorf("commits = %+v, want %s once", got, sha[:7])
	}
}
