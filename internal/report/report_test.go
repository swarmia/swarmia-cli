package report

import (
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/swarmia/swarmia-cli/internal/attribution"
	"github.com/swarmia/swarmia-cli/internal/events"
)

var t0 = time.Date(2026, 10, 1, 12, 0, 0, 0, time.UTC)

// fixture is a repository plus a log of one agent session in it.
type fixture struct {
	t    *testing.T
	root string
	recs []events.Record
}

func newFixture(t *testing.T) *fixture {
	dir, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	f := &fixture{t: t, root: dir}
	f.git(t0.Add(-time.Hour), "me@example.com", "init", "-q", "-b", "main")
	return f
}

// git runs git with author/committer identity and dates pinned to at.
func (f *fixture) git(at time.Time, email string, args ...string) string {
	f.t.Helper()
	cmd := exec.Command("git", append([]string{"-C", f.root}, args...)...)
	date := at.Format(time.RFC3339)
	cmd.Env = append(os.Environ(),
		"GIT_AUTHOR_NAME=dev", "GIT_AUTHOR_EMAIL="+email, "GIT_AUTHOR_DATE="+date,
		"GIT_COMMITTER_NAME=dev", "GIT_COMMITTER_EMAIL="+email, "GIT_COMMITTER_DATE="+date,
		"GIT_CONFIG_GLOBAL=/dev/null", "GIT_CONFIG_NOSYSTEM=1",
	)
	out, err := cmd.CombinedOutput()
	if err != nil {
		f.t.Fatalf("git %v: %v\n%s", args, err, out)
	}
	return strings.TrimSpace(string(out))
}

func (f *fixture) write(path, content string) {
	f.t.Helper()
	full := filepath.Join(f.root, path)
	if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
		f.t.Fatal(err)
	}
	if err := os.WriteFile(full, []byte(content), 0o644); err != nil {
		f.t.Fatal(err)
	}
}

func (f *fixture) commit(at time.Time, email, msg string, files map[string]string) string {
	f.t.Helper()
	for p, c := range files {
		f.write(p, c)
		f.git(at, email, "add", p)
	}
	f.git(at, email, "commit", "-q", "-m", msg)
	return f.git(at, email, "rev-parse", "HEAD")
}

// log appends a hook event to the session log.
func (f *fixture) log(at time.Time, event map[string]any) {
	f.t.Helper()
	event["session_id"] = "sess-1"
	raw, err := json.Marshal(event)
	if err != nil {
		f.t.Fatal(err)
	}
	f.recs = append(f.recs, events.Record{
		LoggedAt: at.Format(time.RFC3339),
		Git:      &events.Git{Root: f.root},
		Event:    raw,
	})
}

func (f *fixture) agentWrite(at time.Time, path, content string, original any) {
	f.log(at, map[string]any{
		"hook_event_name": "PostToolUse", "tool_name": "Write",
		"tool_input":    map[string]any{"file_path": filepath.Join(f.root, path), "content": content},
		"tool_response": map[string]any{"originalFile": original},
	})
}

func lines(n int, format string) string {
	var b strings.Builder
	for i := 1; i <= n; i++ {
		fmt.Fprintf(&b, format+"\n", i)
	}
	return b.String()
}

func classesOf(file File) map[attribution.Class]int {
	return map[attribution.Class]int{
		attribution.Exact:      file.Count(attribution.Exact),
		attribution.Normalized: file.Count(attribution.Normalized),
		attribution.Modified:   file.Count(attribution.Modified),
	}
}

func TestBuildAttributesSessionToCommits(t *testing.T) {
	f := newFixture(t)
	base := lines(10, "var existing%d = compute(%d)")
	f.commit(t0.Add(-30*time.Minute), "me@example.com", "base", map[string]string{"app.go": base})
	// An older commit by me, before the session: must not be considered.
	f.commit(t0.Add(-10*time.Minute), "me@example.com", "before session", map[string]string{"old.go": "package old\n"})

	f.log(t0, map[string]any{"hook_event_name": "SessionStart", "model": "claude-test"})
	f.log(t0.Add(time.Second), map[string]any{"hook_event_name": "UserPromptSubmit", "prompt": "add helpers"})

	// The agent appends five functions to app.go...
	agentCode := lines(5, "func helper%d() string { return \"helper result\" }")
	f.agentWrite(t0.Add(10*time.Second), "app.go", base+agentCode, base)
	// ...creates new.go...
	newFile := "package app\n\nfunc NewThing(name string) *Thing {\n\treturn &Thing{Name: name, Created: time.Now()}\n}\n"
	f.agentWrite(t0.Add(20*time.Second), "new.go", newFile, nil)
	// ...and writes wip.go, which is never committed.
	f.agentWrite(t0.Add(30*time.Second), "wip.go", lines(3, "func unfinished%d() { panic(\"todo: finish this\") }"), nil)

	// I commit app.go with one helper tweaked, plus new.go reformatted; then
	// push it.
	tweaked := strings.Replace(agentCode, `func helper3() string { return "helper result" }`, `func helper3() string { return "helper result, tweaked" }`, 1)
	reformatted := strings.Replace(newFile, "\treturn", "    return", 1)
	first := f.commit(t0.Add(time.Minute), "me@example.com", "Add helpers", map[string]string{
		"app.go": base + tweaked,
		"new.go": reformatted,
	})
	remote := filepath.Join(t.TempDir(), "remote.git")
	f.git(t0, "me@example.com", "init", "-q", "--bare", remote)
	f.git(t0, "me@example.com", "remote", "add", "origin", remote)
	f.git(t0, "me@example.com", "push", "-q", "origin", "main")

	// A teammate's commit touching app.go: filtered out by author.
	f.commit(t0.Add(2*time.Minute), "other@example.com", "teammate", map[string]string{"app.go": base + tweaked + "var teammate = 1\n"})
	// My later local commit re-touching app.go only adds a line of my own; the
	// agent's lines it carries along were introduced earlier, not here.
	second := f.commit(t0.Add(3*time.Minute), "me@example.com", "Mine", map[string]string{"app.go": base + tweaked + "var teammate = 1\nvar mine = 2\n"})

	sessions, err := Build(f.recs, Options{RepoDir: f.root, FallbackRoot: f.root, AuthorEmail: "me@example.com"})
	if err != nil {
		t.Fatal(err)
	}
	if len(sessions) != 1 {
		t.Fatalf("got %d sessions, want 1", len(sessions))
	}
	s := sessions[0]
	if s.Model != "claude-test" || len(s.Prompts) != 1 || len(s.Edits) != 3 {
		t.Errorf("session metadata: model=%q prompts=%d edits=%d", s.Model, len(s.Prompts), len(s.Edits))
	}
	if want := 5 + 4 + 3; s.AILines() != want { // new.go has 4 non-blank lines
		t.Errorf("AILines = %d, want %d", s.AILines(), want)
	}

	if len(s.Commits()) != 1 {
		t.Fatalf("commits = %+v; want only %s (not %s, the teammate's, or pre-session ones)", s.Commits(), first[:7], second[:7])
	}
	c := s.Commits()[0]
	if c.SHA != first || !c.Pushed {
		t.Errorf("commit %s pushed=%v, want %s pushed", c.SHA[:7], c.Pushed, first[:7])
	}
	got := map[string]map[attribution.Class]int{}
	for _, file := range c.Files {
		got[file.Path] = classesOf(file)
	}
	want := map[string]map[attribution.Class]int{
		"app.go": {attribution.Exact: 4, attribution.Normalized: 0, attribution.Modified: 1},
		"new.go": {attribution.Exact: 3, attribution.Normalized: 1, attribution.Modified: 0},
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("attribution = %v, want %v", got, want)
	}
	// The agent's app.go lines are 11–15 in the commit.
	if lines := c.Files[0].Lines; lines[0].Start != 11 || lines[len(lines)-1].End != 15 {
		t.Errorf("app.go lines = %v, want 11..15", lines)
	}
	if c.Survived() != 4+4 {
		t.Errorf("Survived = %d, want 8", c.Survived())
	}

	// Without the author filter, the later commits are scanned but still get
	// nothing: they didn't introduce the agent's lines.
	all, err := Build(f.recs, Options{RepoDir: f.root, FallbackRoot: f.root})
	if err != nil {
		t.Fatal(err)
	}
	if n := len(all[0].Commits()); n != 1 {
		t.Errorf("without author filter: %d commits, want 1", n)
	}
}

// Lines the agent wrote and then replaced itself don't count toward AILines.
func TestAILinesExcludesSelfReplacedLines(t *testing.T) {
	f := newFixture(t)
	f.agentWrite(t0, "x.go", "func a() { return firstVersion() }\nfunc b() { return keep() }\n", nil)
	f.agentWrite(t0.Add(time.Second), "x.go", "func a() { return secondVersion() }\nfunc b() { return keep() }\n",
		"func a() { return firstVersion() }\nfunc b() { return keep() }\n")
	sessions, err := Build(f.recs, Options{RepoDir: f.root, FallbackRoot: f.root})
	if err != nil {
		t.Fatal(err)
	}
	if got := sessions[0].AILines(); got != 2 {
		t.Errorf("AILines = %d, want 2 (secondVersion + keep)", got)
	}
}

func TestRanges(t *testing.T) {
	in := []attribution.LineMatch{
		{Line: 1, Class: attribution.Exact}, {Line: 2, Class: attribution.Exact},
		{Line: 3, Class: attribution.Modified}, {Line: 5, Class: attribution.Modified},
	}
	want := []Range{{1, 2, attribution.Exact}, {3, 3, attribution.Modified}, {5, 5, attribution.Modified}}
	if got := Ranges(in); !reflect.DeepEqual(got, want) {
		t.Errorf("Ranges = %v, want %v", got, want)
	}
}

// Lines a shell command wrote (recorded as changes on the record that ended
// it) are attributed like the agent's own edits.
func TestBuildAttributesShellCommandChanges(t *testing.T) {
	f := newFixture(t)
	f.commit(t0.Add(-time.Hour), "me@example.com", "base", map[string]string{"README": "hi\n"})
	raw, _ := json.Marshal(map[string]any{
		"hook_event_name": "PostToolUse", "session_id": "sess-1", "tool_name": "Bash",
		"tool_input": map[string]any{"command": "./codegen.sh"},
	})
	f.recs = append(f.recs, events.Record{
		LoggedAt: t0.Format(time.RFC3339),
		Git:      &events.Git{Root: f.root},
		Event:    raw,
		Changes: []events.FileChange{{Path: "gen/api.go", Hunks: [][]string{
			{"+package gen", "+", "+func Generated() string { return \"generated value\" }"},
		}}},
	})
	sha := f.commit(t0.Add(time.Minute), "me@example.com", "codegen", map[string]string{
		"gen/api.go": "package gen\n\nfunc Generated() string { return \"generated value\" }\n",
	})

	sessions, err := Build(f.recs, Options{RepoDir: f.root, FallbackRoot: f.root})
	if err != nil {
		t.Fatal(err)
	}
	s := sessions[0]
	if len(s.Edits) != 1 || s.ShellEdits() != 1 || s.AILines() != 2 {
		t.Errorf("edits=%d shell=%d ailines=%d, want 1, 1, 2", len(s.Edits), s.ShellEdits(), s.AILines())
	}
	if len(s.Commits()) != 1 || s.Commits()[0].SHA != sha || s.Commits()[0].Survived() != 2 {
		t.Errorf("commits = %+v, want %s with 2 lines", s.Commits(), sha[:7])
	}
}

// The reflog makes a commit made here under a different email the user's,
// while a fetched commit by someone else with the very same content isn't.
func TestOwnCommitsUseTheReflog(t *testing.T) {
	f := newFixture(t)
	f.commit(t0.Add(-time.Hour), "me@example.com", "base", map[string]string{"README": "hi\n"})
	code := lines(3, "func reflogged%d() string { return \"written by the agent\" }")
	f.agentWrite(t0, "mine.go", code, nil)
	f.agentWrite(t0, "theirs.go", code, nil)

	// I commit mine.go with my work email in this repo's config.
	local := f.commit(t0.Add(time.Minute), "me@work.example.com", "with another email", map[string]string{"mine.go": code})

	// A teammate commits theirs.go in their own clone; I fetch it.
	dir, _ := filepath.EvalSymlinks(t.TempDir())
	theirs := &fixture{t: t, root: filepath.Join(dir, "theirs")}
	f.git(t0, "me@example.com", "clone", "-q", f.root, theirs.root)
	fetched := theirs.commit(t0.Add(2*time.Minute), "teammate@example.com", "teammate", map[string]string{"theirs.go": code})
	f.git(t0, "me@example.com", "fetch", "-q", theirs.root, "main:refs/remotes/teammate/main")

	sessions, err := Build(f.recs, Options{RepoDir: f.root, FallbackRoot: f.root, AuthorEmail: "me@example.com"})
	if err != nil {
		t.Fatal(err)
	}
	var shas []string
	for _, c := range sessions[0].Commits() {
		shas = append(shas, c.SHA)
	}
	if len(shas) != 1 || shas[0] != local {
		t.Errorf("commits = %v, want only the local one %s (not fetched %s)", shas, local[:7], fetched[:7])
	}
}

// A commit rebased after a later session's edit holds the pre-edit lines;
// the later session (which only renamed them) must not be credited with them.
func TestRebasedCommitIsNotCreditedToALaterEditor(t *testing.T) {
	f := newFixture(t)
	f.commit(t0.Add(-time.Hour), "me@example.com", "base", map[string]string{"README": "hi\n"})
	original := strings.Join([]string{
		`import { describe, expect, it } from "vitest";`,
		`import { isSecureURL } from "../src";`,
		``,
		`describe("isSecureURL", () => {`,
		`  it("accepts https and wss URLs", () => {`,
		`    expect(isSecureURL("https://example.com")).toBe(true);`,
		`    expect(isSecureURL("wss://example.com/socket")).toBe(true);`,
		`  });`,
		`  it("rejects other protocols", () => {`,
		`    expect(isSecureURL("http://example.com")).toBe(false);`,
		`    expect(isSecureURL("ftp://files.example.com")).toBe(false);`,
		`  });`,
		`});`,
	}, "\n") + "\n"
	renamed := strings.ReplaceAll(original, "isSecureURL", "hasSecureProtocol")

	// Session 1 writes it; the developer commits it.
	f.agentWrite(t0, "utils.ts", original, nil)
	f.commit(t0.Add(time.Minute), "me@example.com", "Add isSecureURL", map[string]string{"utils.ts": original})

	// Session 2 renames it (edits after that commit).
	raw, _ := json.Marshal(map[string]any{
		"hook_event_name": "PostToolUse", "session_id": "sess-2", "tool_name": "Write",
		"tool_input":    map[string]any{"file_path": filepath.Join(f.root, "utils.ts"), "content": renamed},
		"tool_response": map[string]any{"originalFile": original},
	})
	f.recs = append(f.recs, events.Record{LoggedAt: t0.Add(2 * time.Minute).Format(time.RFC3339), Git: &events.Git{Root: f.root}, Event: raw})

	// The developer rebases the first commit onto a teammate's work, which
	// gives it a committer date after session 2's edit.
	f.git(t0.Add(3*time.Minute), "me@example.com", "checkout", "-q", "-b", "teammate", "HEAD~1")
	f.commit(t0.Add(3*time.Minute), "teammate@example.com", "teammate", map[string]string{"team.txt": "x\n"})
	f.git(t0.Add(4*time.Minute), "me@example.com", "checkout", "-q", "main")
	f.git(t0.Add(4*time.Minute), "me@example.com", "rebase", "-q", "teammate")
	rebased := f.git(t0, "me@example.com", "rev-parse", "HEAD")

	sessions, err := Build(f.recs, Options{RepoDir: f.root, FallbackRoot: f.root, AuthorEmail: "me@example.com"})
	if err != nil {
		t.Fatal(err)
	}
	for _, s := range sessions {
		for _, c := range s.Commits() {
			if s.ID == "sess-2" && c.SHA == rebased {
				t.Errorf("session 2 credited with %v in the rebased commit, which predates its rename", c.Files)
			}
		}
	}
}
