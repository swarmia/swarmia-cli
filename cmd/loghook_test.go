package cmd

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/swarmia/swarmia-cli/internal/events"
)

// lockFile must exclude a second opener of the same file until released; that
// is what keeps concurrent log-hook invocations (parallel tool calls, subagents)
// from interleaving writes.
func TestLockFileExcludesSecondOpener(t *testing.T) {
	path := filepath.Join(t.TempDir(), "events.jsonl")
	open := func() *os.File {
		f, err := os.OpenFile(path, os.O_CREATE|os.O_WRONLY, 0o644)
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { f.Close() })
		return f
	}

	unlock, err := lockFile(open())
	if err != nil {
		t.Fatal(err)
	}

	acquired := make(chan struct{})
	go func() {
		unlock2, err := lockFile(open())
		if err != nil {
			t.Error(err)
			return
		}
		unlock2()
		close(acquired)
	}()

	select {
	case <-acquired:
		t.Fatal("second lock acquired while the first was held")
	case <-time.After(100 * time.Millisecond):
	}
	unlock()
	select {
	case <-acquired:
	case <-time.After(5 * time.Second):
		t.Fatal("second lock not acquired after release")
	}
}

func TestRunLogHookWrapsPayload(t *testing.T) {
	path := filepath.Join(t.TempDir(), "events.jsonl")
	t.Setenv("SWARMIA_LOG_FILE", path)
	t.Setenv("CLAUDE_PROJECT_DIR", t.TempDir()) // not a repo: no git field

	if err := runLogHook(strings.NewReader(`{"hook_event_name":"Stop","session_id":"s1"}`)); err != nil {
		t.Fatal(err)
	}
	if err := runLogHook(strings.NewReader("   \n")); err != nil { // blank input: no line
		t.Fatal(err)
	}

	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if n := bytes.Count(data, []byte("\n")); n != 1 {
		t.Fatalf("got %d lines, want 1:\n%s", n, data)
	}
	var rec struct {
		LoggedAt string         `json:"logged_at"`
		Event    map[string]any `json:"event"`
	}
	if err := json.Unmarshal(data, &rec); err != nil {
		t.Fatal(err)
	}
	if bytes.Contains(data, []byte(`"git"`)) {
		t.Errorf("git field written outside a repo: %s", data)
	}
	if rec.LoggedAt == "" || rec.Event["session_id"] != "s1" {
		t.Errorf("unexpected record: %s", data)
	}
}

// A user-scope hook stands down when the project wires log-hook itself, so the
// event isn't logged twice; elsewhere it logs as usual.
func TestLogHookSkipIfProjectHooks(t *testing.T) {
	logSkipIfProjectHooks = true
	t.Cleanup(func() { logSkipIfProjectHooks = false })
	path := filepath.Join(t.TempDir(), "events.jsonl")
	t.Setenv("SWARMIA_LOG_FILE", path)

	withHooks := t.TempDir()
	writeSettings(t, withHooks, map[string]any{"hooks": map[string]any{
		"Stop": []any{grp("", "${CLAUDE_PROJECT_DIR}/swarmia-cli log-hook")},
	}})
	t.Setenv("CLAUDE_PROJECT_DIR", withHooks)
	if err := runLogHook(strings.NewReader(`{"hook_event_name":"Stop"}`)); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(path); err == nil {
		t.Fatal("logged although the project has its own log-hook")
	}

	t.Setenv("CLAUDE_PROJECT_DIR", t.TempDir())
	if err := runLogHook(strings.NewReader(`{"hook_event_name":"Stop"}`)); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(path); err != nil {
		t.Errorf("didn't log in a project without its own hooks: %v", err)
	}
}

// A Bash tool call is bracketed: PreToolUse snapshots the worktree and the
// finishing event (here a failure) records what the command authored.
func TestLogHookRecordsShellCommandChanges(t *testing.T) {
	repo := initRepo(t)
	t.Setenv("CLAUDE_PROJECT_DIR", repo)
	path := filepath.Join(t.TempDir(), "events.jsonl")
	t.Setenv("SWARMIA_LOG_FILE", path)

	bash := func(event string) string {
		return `{"hook_event_name":"` + event + `","session_id":"s","tool_name":"Bash","tool_use_id":"toolu_42","tool_input":{"command":"./gen.sh"}}`
	}
	if err := runLogHook(strings.NewReader(bash("PreToolUse"))); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(repo, "gen.go"), []byte("package gen\n\nfunc Generated() {}\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := runLogHook(strings.NewReader(bash("PostToolUseFailure"))); err != nil {
		t.Fatal(err)
	}

	recs, err := events.ReadFile(path)
	if err != nil || len(recs) != 2 {
		t.Fatalf("got %d records, %v", len(recs), err)
	}
	if recs[0].Changes != nil {
		t.Errorf("PreToolUse record has changes: %+v", recs[0].Changes)
	}
	want := []events.FileChange{{Path: "gen.go", Hunks: [][]string{{"+package gen", "+", "+func Generated() {}"}}}}
	if !reflect.DeepEqual(recs[1].Changes, want) {
		t.Errorf("changes = %+v, want %+v", recs[1].Changes, want)
	}
}
