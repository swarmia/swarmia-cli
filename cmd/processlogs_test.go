package cmd

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// initRepo creates a throwaway git repo in a temp dir. process-logs injects its
// own committer identity, so no user.name/email config is needed.
func initRepo(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	if _, err := runGit(dir, nil, "init", "-q"); err != nil {
		t.Fatalf("git init: %v", err)
	}
	return dir
}

// eventLine builds a log envelope like log-hook writes, tagged with a session id
// and a marker so we can assert exact content and ordering. Output is
// deterministic (encoding/json sorts map keys).
func eventLine(session, marker string) string {
	b, err := json.Marshal(map[string]any{
		"logged_at": "2026-07-10T00:00:00Z",
		"event": map[string]any{
			"session_id":      session,
			"hook_event_name": "PreToolUse",
			"marker":          marker,
		},
	})
	if err != nil {
		panic(err)
	}
	return string(b)
}

// appendEvents appends lines to the log file exactly as log-hook would.
func appendEvents(t *testing.T, path string, lines ...string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	f, err := os.OpenFile(path, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o644)
	if err != nil {
		t.Fatalf("open log: %v", err)
	}
	defer f.Close()
	for _, l := range lines {
		if _, err := f.WriteString(l + "\n"); err != nil {
			t.Fatalf("write log: %v", err)
		}
	}
}

// refBlob returns the events.jsonl content stored in a session ref.
func refBlob(t *testing.T, repoDir, key string) string {
	t.Helper()
	out, err := runGit(repoDir, nil, "cat-file", "-p", refPrefix+key+":"+eventsFileName)
	if err != nil {
		t.Fatalf("cat-file %s: %v", key, err)
	}
	return string(out)
}

// commitCount returns the number of commits reachable from a session ref.
func commitCount(t *testing.T, repoDir, key string) string {
	t.Helper()
	out, err := runGit(repoDir, nil, "rev-list", "--count", refPrefix+key)
	if err != nil {
		t.Fatalf("rev-list %s: %v", key, err)
	}
	return strings.TrimSpace(string(out))
}

func revParse(t *testing.T, repoDir, rev string) string {
	t.Helper()
	out, err := runGit(repoDir, nil, "rev-parse", rev)
	if err != nil {
		t.Fatalf("rev-parse %s: %v", rev, err)
	}
	return strings.TrimSpace(string(out))
}

func TestProcessLogsStoresPerSession(t *testing.T) {
	repo := initRepo(t)
	logPath := filepath.Join(repo, ".swarmia", "events.jsonl")

	// Two sessions, interleaved in the file.
	appendEvents(t, logPath,
		eventLine("aaaa", "a1"),
		eventLine("bbbb", "b1"),
		eventLine("aaaa", "a2"),
	)

	res, err := processLogs(repo, logPath, processOpts{})
	if err != nil {
		t.Fatalf("processLogs: %v", err)
	}
	if res.Sessions != 2 || res.Events != 3 {
		t.Fatalf("result = %+v, want 2 sessions / 3 events", res)
	}

	// Each ref holds exactly its session's lines, in order.
	if got, want := refBlob(t, repo, "aaaa"), eventLine("aaaa", "a1")+"\n"+eventLine("aaaa", "a2")+"\n"; got != want {
		t.Errorf("ref aaaa = %q, want %q", got, want)
	}
	if got, want := refBlob(t, repo, "bbbb"), eventLine("bbbb", "b1")+"\n"; got != want {
		t.Errorf("ref bbbb = %q, want %q", got, want)
	}

	// Local file is emptied (rotation).
	if data, _ := os.ReadFile(logPath); len(strings.TrimSpace(string(data))) != 0 {
		t.Errorf("log file not emptied: %q", data)
	}

	// Nothing was staged or committed: every status entry is untracked (??).
	out, err := runGit(repo, nil, "status", "--porcelain")
	if err != nil {
		t.Fatalf("git status: %v", err)
	}
	for _, line := range strings.Split(strings.TrimSpace(string(out)), "\n") {
		if line != "" && !strings.HasPrefix(line, "??") {
			t.Errorf("working tree/index touched: %q", line)
		}
	}
	// And HEAD is still unborn — no commit landed on the branch.
	if _, err := runGit(repo, nil, "rev-parse", "--verify", "--quiet", "HEAD"); err == nil {
		t.Errorf("HEAD advanced; process-logs must not commit to the branch")
	}
}

// TestProcessLogsAccumulatesAcrossRuns is the key case: a session that keeps going
// after its logs were shipped must keep updating the same ref.
func TestProcessLogsAccumulatesAcrossRuns(t *testing.T) {
	repo := initRepo(t)
	logPath := filepath.Join(repo, ".swarmia", "events.jsonl")

	// First shipment: two events for session S.
	appendEvents(t, logPath, eventLine("S", "e1"), eventLine("S", "e2"))
	if _, err := processLogs(repo, logPath, processOpts{}); err != nil {
		t.Fatalf("run 1: %v", err)
	}
	if got, want := refBlob(t, repo, "S"), eventLine("S", "e1")+"\n"+eventLine("S", "e2")+"\n"; got != want {
		t.Fatalf("after run 1 ref S = %q, want %q", got, want)
	}
	if got := commitCount(t, repo, "S"); got != "1" {
		t.Fatalf("after run 1 commit count = %s, want 1", got)
	}
	c1 := revParse(t, repo, refPrefix+"S")

	// Session continues: two more events land in the (now empty) file, then ship again.
	appendEvents(t, logPath, eventLine("S", "e3"), eventLine("S", "e4"))
	if _, err := processLogs(repo, logPath, processOpts{}); err != nil {
		t.Fatalf("run 2: %v", err)
	}

	// The ref's file now holds all four events, in order.
	want := strings.Join([]string{
		eventLine("S", "e1"), eventLine("S", "e2"),
		eventLine("S", "e3"), eventLine("S", "e4"),
	}, "\n") + "\n"
	if got := refBlob(t, repo, "S"); got != want {
		t.Errorf("after run 2 ref S = %q, want %q", got, want)
	}
	// Two commits, chained: C2's parent is C1.
	if got := commitCount(t, repo, "S"); got != "2" {
		t.Errorf("after run 2 commit count = %s, want 2", got)
	}
	if got := revParse(t, repo, refPrefix+"S^"); got != c1 {
		t.Errorf("run 2 commit parent = %s, want C1 %s", got, c1)
	}
}

// TestProcessLogsRollbackOnStoreFailure verifies that when the git store fails, the
// claimed batch is put back so nothing is lost and the next run can retry.
func TestProcessLogsRollbackOnStoreFailure(t *testing.T) {
	// A plain temp dir that is NOT a git repo, so every git plumbing call fails.
	notRepo := t.TempDir()
	logPath := filepath.Join(notRepo, ".swarmia", "events.jsonl")
	appendEvents(t, logPath, eventLine("S", "e1"))

	_, err := processLogs(notRepo, logPath, processOpts{})
	if err == nil {
		t.Fatal("expected an error storing into a non-repo, got nil")
	}

	// The batch must be back in the file, not lost.
	data, readErr := os.ReadFile(logPath)
	if readErr != nil {
		t.Fatalf("read log: %v", readErr)
	}
	if got, want := string(data), eventLine("S", "e1")+"\n"; got != want {
		t.Errorf("log after rollback = %q, want %q", got, want)
	}
}
