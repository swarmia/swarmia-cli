package report

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/swarmia/swarmia-cli/internal/events"
	"github.com/swarmia/swarmia-cli/internal/fslock"
)

// writeLog writes records as a raw event log, the way log-hook does.
func writeLog(t *testing.T, path string, recs []events.Record) {
	t.Helper()
	f, err := os.OpenFile(path, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o600)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	for _, r := range recs {
		line, _ := json.Marshal(r)
		f.Write(append(line, '\n'))
	}
}

func (f *fixture) process(t *testing.T, stateDir string, now time.Time) ProcessResult {
	t.Helper()
	res, err := Process(ProcessOptions{
		Options:  Options{FallbackRoot: f.root, AuthorEmail: emailIs("me@example.com")},
		LogPath:  filepath.Join(stateDir, "events.jsonl"),
		StateDir: stateDir,
		Wait:     true,
		Now:      now,
	})
	if err != nil {
		t.Fatal(err)
	}
	return res
}

func TestProcessFoldsLogAndCachesAttribution(t *testing.T) {
	f := newFixture(t)
	state := t.TempDir()
	logPath := filepath.Join(state, "events.jsonl")
	f.commit(t0.Add(-time.Hour), "me@example.com", "base", map[string]string{"README": "hi\n"})

	code := lines(4, "func generated%d() string { return \"value from the agent\" }")
	f.agentWrite(t0, "gen.go", code, nil)
	// The same event logged twice (overlapping hooks) folds once.
	writeLog(t, logPath, append(f.recs, f.recs...))

	res := f.process(t, state, t0.Add(time.Hour))
	if res.Folded != 2 || len(res.Sessions) != 1 {
		t.Fatalf("folded %d records into %d sessions, want 2 into 1", res.Folded, len(res.Sessions))
	}
	if s := res.Sessions[0]; len(s.Edits) != 1 || s.AILines() != 4 || len(s.Commits()) != 0 {
		t.Errorf("edits=%d ailines=%d commits=%d, want 1, 4, 0", len(s.Edits), s.AILines(), len(s.Commits()))
	}
	if fi, err := os.Stat(logPath); err != nil || fi.Size() != 0 {
		t.Errorf("log not emptied after folding: %v, %v", fi, err)
	}

	// Nothing new: the stored attribution is reused as is.
	computed := res.Sessions[0].Attribution.ComputedAt
	res = f.process(t, state, t0.Add(2*time.Hour))
	if got := res.Sessions[0].Attribution.ComputedAt; !got.Equal(computed) {
		t.Errorf("attribution recomputed without new inputs (%v → %v)", computed, got)
	}

	// A new commit with the agent's code: recomputed, and attributed.
	sha := f.commit(t0.Add(3*time.Hour), "me@example.com", "add gen", map[string]string{"gen.go": code})
	res = f.process(t, state, t0.Add(3*time.Hour))
	if c := res.Sessions[0].Commits(); len(c) != 1 || c[0].SHA != sha || c[0].Survived() != 4 || c[0].Pushed {
		t.Errorf("commits = %+v, want %s with 4 lines, local", c, sha[:7])
	}

	// Pushing refreshes the flag without needing new inputs.
	remote := filepath.Join(t.TempDir(), "remote.git")
	f.git(t0, "me@example.com", "init", "-q", "--bare", remote)
	f.git(t0, "me@example.com", "remote", "add", "origin", remote)
	f.git(t0, "me@example.com", "push", "-q", "origin", "main")
	res = f.process(t, state, t0.Add(4*time.Hour))
	if c := res.Sessions[0].Commits(); len(c) != 1 || !c[0].Pushed {
		t.Errorf("after push: %+v, want pushed", c)
	}
}

// Amending a local commit replaces it: the old sha is unreachable, so the
// attribution moves to the new one.
func TestProcessFollowsRewrittenCommits(t *testing.T) {
	f := newFixture(t)
	state := t.TempDir()
	f.commit(t0.Add(-time.Hour), "me@example.com", "base", map[string]string{"README": "hi\n"})
	code := lines(3, "func rewritten%d() error { return errors.New(\"agent wrote this\") }")
	f.agentWrite(t0, "x.go", code, nil)
	writeLog(t, filepath.Join(state, "events.jsonl"), f.recs)

	before := f.commit(t0.Add(time.Minute), "me@example.com", "wip", map[string]string{"x.go": code})
	if c := f.process(t, state, t0.Add(time.Hour)).Sessions[0].Commits(); len(c) != 1 || c[0].SHA != before {
		t.Fatalf("before amend: %+v", c)
	}
	f.git(t0.Add(2*time.Minute), "me@example.com", "commit", "-q", "--amend", "-m", "better message")
	after := f.git(t0, "me@example.com", "rev-parse", "HEAD")
	c := f.process(t, state, t0.Add(time.Hour)).Sessions[0].Commits()
	if len(c) != 1 || c[0].SHA != after || c[0].Subject != "better message" {
		t.Errorf("after amend: %+v, want only %s", c, after[:7])
	}
}

// Edits folded in separate runs still count AI lines across the whole session.
func TestProcessFoldsIncrementally(t *testing.T) {
	f := newFixture(t)
	state := t.TempDir()
	logPath := filepath.Join(state, "events.jsonl")
	f.agentWrite(t0, "x.go", "func a() { return firstVersion() }\nfunc b() { return keep() }\n", nil)
	writeLog(t, logPath, f.recs)
	f.process(t, state, t0)

	f.recs = nil
	f.agentWrite(t0.Add(time.Second), "x.go", "func a() { return secondVersion() }\nfunc b() { return keep() }\n",
		"func a() { return firstVersion() }\nfunc b() { return keep() }\n")
	writeLog(t, logPath, f.recs)
	res := f.process(t, state, t0)
	if s := res.Sessions[0]; len(s.Edits) != 2 || s.AILines() != 2 {
		t.Errorf("edits=%d ailines=%d, want 2 and 2 (secondVersion + keep)", len(s.Edits), s.AILines())
	}
}

// retitle changes a stored commit's subject, so a test can tell a result
// reused from the store (still retitled) from one matched again.
func retitle(t *testing.T, stateDir, sha, subject string) {
	t.Helper()
	st := Store{Dir: filepath.Join(stateDir, "sessions")}
	sessions, _, err := st.List()
	if err != nil {
		t.Fatal(err)
	}
	for _, s := range sessions {
		for i, c := range s.Commits() {
			if c.SHA == sha {
				s.Attribution.Commits[i].Subject = subject
				if err := st.Save(s); err != nil {
					t.Fatal(err)
				}
				return
			}
		}
	}
	t.Fatalf("no stored commit %s", sha[:7])
}

// A new commit recomputes the session's attribution, but commits matched
// before with the same edits aren't matched again.
func TestProcessMatchesOnlyNewCommits(t *testing.T) {
	f := newFixture(t)
	state := t.TempDir()
	f.commit(t0.Add(-time.Hour), "me@example.com", "base", map[string]string{"README": "hi\n"})
	code := lines(4, "func generated%d() string { return \"value from the agent\" }")
	f.agentWrite(t0, "gen.go", code, nil)
	writeLog(t, filepath.Join(state, "events.jsonl"), f.recs)
	first := f.commit(t0.Add(time.Minute), "me@example.com", "add gen", map[string]string{"gen.go": code})
	res := f.process(t, state, t0.Add(time.Hour))
	computed := res.Sessions[0].Attribution.ComputedAt
	retitle(t, state, first, "from the store")

	second := f.commit(t0.Add(2*time.Minute), "me@example.com", "more", map[string]string{"gen.go": code + "func human() {}\n"})
	res = f.process(t, state, t0.Add(2*time.Hour))
	s := res.Sessions[0]
	if s.Attribution.ComputedAt.Equal(computed) {
		t.Fatal("attribution not recomputed after a new commit")
	}
	if c := s.Commits(); len(c) != 1 || c[0].SHA != first || c[0].Subject != "from the store" {
		t.Errorf("commits = %+v, want only %s, reused from the store", c, first[:7])
	}
	n := 0
	for _, shas := range s.Attribution.Matched {
		n += len(shas)
	}
	if n != 2 {
		t.Errorf("matched = %v, want both %s and %s", s.Attribution.Matched, first[:7], second[:7])
	}
}

// An edit folded after a commit was matched, but made before it (within the
// clock slack), changes what the commit is matched with: it is matched again.
func TestProcessRematchesCommitsNewEditsApplyTo(t *testing.T) {
	f := newFixture(t)
	state := t.TempDir()
	logPath := filepath.Join(state, "events.jsonl")
	f.commit(t0.Add(-time.Hour), "me@example.com", "base", map[string]string{"README": "hi\n"})
	one := lines(4, "func first%d() string { return \"written by the agent\" }")
	two := lines(4, "func second%d() string { return \"also by the agent\" }")
	f.agentWrite(t0, "x.go", one, nil)
	writeLog(t, logPath, f.recs)
	at := t0.Add(time.Minute)
	sha := f.commit(at, "me@example.com", "both", map[string]string{"x.go": one + two})
	if c := f.process(t, state, t0.Add(time.Hour)).Sessions[0].Commits(); len(c) != 1 || c[0].Survived() != 4 {
		t.Fatalf("before the late edit: %+v, want %s with 4 lines", c, sha[:7])
	}
	retitle(t, state, sha, "from the store")

	f.recs = nil
	f.agentWrite(at.Add(time.Second), "x.go", one+two, one)
	writeLog(t, logPath, f.recs)
	c := f.process(t, state, t0.Add(2*time.Hour)).Sessions[0].Commits()
	if len(c) != 1 || c[0].Survived() != 8 || c[0].Subject != "both" {
		t.Errorf("after the late edit: %+v, want %s with 8 lines, matched again", c, sha[:7])
	}
}

// A commit git failed to read isn't taken to hold none of the lines: it is
// matched again on the next run, once it can be read.
func TestProcessRematchesCommitsGitFailedToRead(t *testing.T) {
	f := newFixture(t)
	state := t.TempDir()
	f.git(t0, "me@example.com", "config", "gc.auto", "0") // keep objects loose, to hide one below
	f.commit(t0.Add(-time.Hour), "me@example.com", "base", map[string]string{"README": "hi\n"})
	code := lines(4, "func generated%d() string { return \"value from the agent\" }")
	f.agentWrite(t0, "gen.go", code, nil)
	writeLog(t, filepath.Join(state, "events.jsonl"), f.recs)
	sha := f.commit(t0.Add(time.Minute), "me@example.com", "add gen", map[string]string{"gen.go": code})

	// Hide the file's blob: git can't show it, though the commit still lists it.
	blob := f.git(t0, "me@example.com", "rev-parse", sha+":gen.go")
	object := filepath.Join(f.root, ".git", "objects", blob[:2], blob[2:])
	if _, err := os.Stat(object); err != nil {
		t.Skipf("blob %s isn't a loose object (packed by git config?): %v", blob[:7], err)
	}
	if err := os.Rename(object, object+".hidden"); err != nil {
		t.Fatal(err)
	}
	if c := f.process(t, state, t0.Add(time.Hour)).Sessions[0].Commits(); len(c) != 0 {
		t.Fatalf("with the blob hidden: %+v, want none", c)
	}
	if err := os.Rename(object+".hidden", object); err != nil {
		t.Fatal(err)
	}
	if c := f.process(t, state, t0.Add(2*time.Hour)).Sessions[0].Commits(); len(c) != 1 || c[0].SHA != sha || c[0].Survived() != 4 {
		t.Errorf("once readable: %+v, want %s with 4 lines", c, sha[:7])
	}
}

// A failed read only makes the sessions that read it retry: another session
// reading another file of the same commit is complete.
func TestProcessRetriesOnlySessionsWhoseReadFailed(t *testing.T) {
	f := newFixture(t)
	state := t.TempDir()
	f.git(t0, "me@example.com", "config", "gc.auto", "0") // keep objects loose, to hide one below
	f.commit(t0.Add(-time.Hour), "me@example.com", "base", map[string]string{"README": "hi\n"})
	codeA := lines(4, "func fromOne%d() string { return \"first session\" }")
	codeB := lines(4, "func fromTwo%d() string { return \"second session\" }")
	f.agentWrite(t0, "a.go", codeA, nil)
	raw, _ := json.Marshal(map[string]any{"session_id": "sess-2", "hook_event_name": "PostToolUse", "tool_name": "Write",
		"tool_input": map[string]any{"file_path": filepath.Join(f.root, "b.go"), "content": codeB}})
	f.recs = append(f.recs, events.Record{LoggedAt: t0.Format(time.RFC3339), Git: &events.Git{Root: f.root}, Event: raw})
	writeLog(t, filepath.Join(state, "events.jsonl"), f.recs)
	sha := f.commit(t0.Add(time.Minute), "me@example.com", "both", map[string]string{"a.go": codeA, "b.go": codeB})

	blob := f.git(t0, "me@example.com", "rev-parse", sha+":a.go")
	object := filepath.Join(f.root, ".git", "objects", blob[:2], blob[2:])
	if _, err := os.Stat(object); err != nil {
		t.Skipf("blob %s isn't a loose object (packed by git config?): %v", blob[:7], err)
	}
	if err := os.Rename(object, object+".hidden"); err != nil {
		t.Fatal(err)
	}
	defer os.Rename(object+".hidden", object)
	for _, s := range f.process(t, state, t0.Add(time.Hour)).Sessions {
		switch s.ID {
		case "sess-1":
			if s.Attribution.Key != "" {
				t.Errorf("sess-1, whose read failed, will not be retried")
			}
		case "sess-2":
			if s.Attribution.Key == "" || len(s.Commits()) != 1 {
				t.Errorf("sess-2: key %q, commits %+v; want it complete, with %s", s.Attribution.Key, s.Commits(), sha[:7])
			}
		}
	}
}

// A commit recorded as holding lines whose stored result is missing is
// matched again rather than dropped.
func TestProcessRematchesCommitsWithMissingResults(t *testing.T) {
	f := newFixture(t)
	state := t.TempDir()
	f.commit(t0.Add(-time.Hour), "me@example.com", "base", map[string]string{"README": "hi\n"})
	code := lines(4, "func generated%d() string { return \"value from the agent\" }")
	f.agentWrite(t0, "gen.go", code, nil)
	writeLog(t, filepath.Join(state, "events.jsonl"), f.recs)
	sha := f.commit(t0.Add(time.Minute), "me@example.com", "add gen", map[string]string{"gen.go": code})
	f.process(t, state, t0.Add(time.Hour))

	st := Store{Dir: filepath.Join(state, "sessions")}
	sessions, _, err := st.List()
	if err != nil || len(sessions) != 1 {
		t.Fatalf("stored sessions: %v, %v", sessions, err)
	}
	sessions[0].Attribution.Commits = nil
	sessions[0].Attribution.Key = "stale"
	if err := st.Save(sessions[0]); err != nil {
		t.Fatal(err)
	}
	if c := f.process(t, state, t0.Add(2*time.Hour)).Sessions[0].Commits(); len(c) != 1 || c[0].SHA != sha {
		t.Errorf("commits = %+v, want %s matched again", c, sha[:7])
	}
}

func TestProcessPrunesOldSessions(t *testing.T) {
	f := newFixture(t)
	state := t.TempDir()
	f.agentWrite(t0, "x.go", "func old() { return stale() }\n", nil)
	writeLog(t, filepath.Join(state, "events.jsonl"), f.recs)
	if res := f.process(t, state, t0); len(res.Sessions) != 1 {
		t.Fatalf("sessions = %d, want 1", len(res.Sessions))
	}
	res := f.process(t, state, t0.Add(Retention+time.Hour))
	if res.Pruned != 1 || len(res.Sessions) != 0 {
		t.Errorf("pruned=%d sessions=%d, want 1 and 0", res.Pruned, len(res.Sessions))
	}
	if left, _ := os.ReadDir(filepath.Join(state, "sessions")); len(left) != 0 {
		t.Errorf("session files left: %v", left)
	}
}

func TestProcessWithoutWaitReportsBusy(t *testing.T) {
	state := t.TempDir()
	lock, err := os.OpenFile(filepath.Join(state, "process.lock"), os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		t.Fatal(err)
	}
	defer lock.Close()
	unlock, err := fslock.Lock(lock)
	if err != nil {
		t.Fatal(err)
	}
	defer unlock()
	_, err = Process(ProcessOptions{LogPath: filepath.Join(state, "events.jsonl"), StateDir: state, Now: t0})
	if !errors.Is(err, ErrBusy) {
		t.Errorf("err = %v, want ErrBusy", err)
	}
}

// Token usage is read from the session's transcript (path from the hooks) and
// keeps up as the transcript grows, without counting anything twice.
func TestProcessTracksTranscriptUsage(t *testing.T) {
	f := newFixture(t)
	state := t.TempDir()
	transcript := filepath.Join(t.TempDir(), "sess-1.jsonl")
	line := func(id string, out int) string {
		return `{"type":"assistant","requestId":"r-` + id + `","message":{"id":"m-` + id + `","model":"claude-test","usage":{"input_tokens":1,"output_tokens":` + strconv.Itoa(out) + `,"speed":"standard"}}}` + "\n"
	}
	if err := os.WriteFile(transcript, []byte(line("1", 10)+line("1", 10)), 0o600); err != nil {
		t.Fatal(err)
	}
	f.log(t0, map[string]any{"hook_event_name": "SessionStart", "transcript_path": transcript})
	writeLog(t, filepath.Join(state, "events.jsonl"), f.recs)

	usage := func(res ProcessResult) (int64, int64) {
		tot := res.Sessions[0].Usage.Totals()
		if len(tot) != 1 {
			t.Fatalf("totals = %+v", tot)
		}
		return tot[0].Requests, tot[0].OutputTokens
	}
	if req, out := usage(f.process(t, state, t0)); req != 1 || out != 10 {
		t.Errorf("first run: %d requests, %d output tokens; want 1, 10", req, out)
	}
	fh, _ := os.OpenFile(transcript, os.O_APPEND|os.O_WRONLY, 0o600)
	fh.WriteString(line("2", 5))
	fh.Close()
	if req, out := usage(f.process(t, state, t0)); req != 2 || out != 15 {
		t.Errorf("after growth: %d requests, %d output tokens; want 2, 15", req, out)
	}
}

// A repository's state is shared by every swarmia-cli version in use (each
// worktree may have its own). This version must leave what a newer one wrote
// alone: records in a newer format stay in the log byte for byte, as do the
// records of sessions stored in a newer format, whose files it doesn't touch.
func TestProcessLeavesNewerFormatsAlone(t *testing.T) {
	f := newFixture(t)
	state := t.TempDir()
	logPath := filepath.Join(state, "events.jsonl")
	f.agentWrite(t0, "gen.go", "package gen\n", nil) // sess-1, this version's
	writeLog(t, logPath, f.recs)

	newerRecord := fmt.Sprintf(`{"format":%d,"logged_at":"2026-10-07T10:00:00Z","event":{"session_id":"sess-2"},"from_the_future":[1,2]}`, events.Format+1) + "\n"
	newerSession := fmt.Sprintf(`{"format":%d,"id":"sess-3","agent":"claude-code","from_the_future":true}`, SessionFormat+1)
	ofNewerSession := `{"format":1,"logged_at":"2026-10-07T10:00:00Z","event":{"session_id":"sess-3","hook_event_name":"Stop"}}` + "\n"
	sessions := filepath.Join(state, "sessions")
	if err := os.MkdirAll(sessions, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(sessions, "sess-3.json"), []byte(newerSession), 0o600); err != nil {
		t.Fatal(err)
	}
	lf, err := os.OpenFile(logPath, os.O_APPEND|os.O_WRONLY, 0o600)
	if err != nil {
		t.Fatal(err)
	}
	lf.WriteString(newerRecord + ofNewerSession)
	lf.Close()

	for run := range 2 { // the second run finds nothing it can fold
		res := f.process(t, state, t0.Add(time.Hour))
		if want := 1 - run; res.Folded != want || len(res.Sessions) != 1 || res.Sessions[0].ID != "sess-1" {
			t.Errorf("run %d: folded %d into %d session(s), want %d into sess-1 only", run, res.Folded, len(res.Sessions), want)
		}
		if got, _ := os.ReadFile(logPath); string(got) != newerRecord+ofNewerSession {
			t.Errorf("run %d: log = %q, want the newer records as they were", run, got)
		}
		if got, _ := os.ReadFile(filepath.Join(sessions, "sess-3.json")); string(got) != newerSession {
			t.Errorf("run %d: newer session rewritten: %s", run, got)
		}
	}
	if got, _ := os.ReadFile(filepath.Join(sessions, "sess-1.json")); !strings.Contains(string(got), fmt.Sprintf(`"format":%d`, SessionFormat)) {
		t.Errorf("saved session lacks its format: %.80s…", got)
	}
}

// Sessions stored before turns kept prompts alone; they become turns.
func TestListReadsPromptsAsTurns(t *testing.T) {
	st := Store{Dir: t.TempDir()}
	old := `{"format":3,"id":"old","agent":"claude-code","prompts":[{"at":"2026-10-07T10:00:00Z","text":"fix it"}]}`
	if err := os.WriteFile(filepath.Join(st.Dir, "old.json"), []byte(old), 0o600); err != nil {
		t.Fatal(err)
	}
	sessions, _, err := st.List()
	if err != nil || len(sessions) != 1 {
		t.Fatalf("List = %v, %v", sessions, err)
	}
	s := sessions[0]
	if len(s.Turns) != 1 || s.Turns[0].Text != "fix it" || s.Turns[0].EndedAt != nil || s.Prompts != nil {
		t.Errorf("turns = %+v, prompts = %+v", s.Turns, s.Prompts)
	}
}
