package usage

import (
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

// assistant renders a transcript line for one content block of a response.
func assistant(msgID, reqID, model string, in, out, cacheRead, cache1h int64) string {
	return fmt.Sprintf(`{"type":"assistant","requestId":%q,"message":{"id":%q,"model":%q,"content":[{"type":"text","text":"secret code here"}],"usage":{"input_tokens":%d,"output_tokens":%d,"cache_read_input_tokens":%d,"cache_creation_input_tokens":%d,"cache_creation":{"ephemeral_5m_input_tokens":0,"ephemeral_1h_input_tokens":%d},"server_tool_use":{"web_search_requests":0,"web_fetch_requests":1},"speed":"standard"}}}`,
		reqID, msgID, model, in, out, cacheRead, cache1h, cache1h)
}

func write(t *testing.T, path string, lines ...string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(strings.Join(lines, "\n")+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
}

func appendTo(t *testing.T, path, text string) {
	t.Helper()
	f, err := os.OpenFile(path, os.O_APPEND|os.O_WRONLY, 0o644)
	if err != nil {
		t.Fatal(err)
	}
	f.WriteString(text)
	f.Close()
}

func TestTotalsDeduplicatesAndSplitsSubagents(t *testing.T) {
	dir := t.TempDir()
	main := filepath.Join(dir, "sess.jsonl")
	write(t, main,
		`{"type":"user","message":{"content":"hello"}}`,
		assistant("m1", "r1", "claude-opus-5-5", 2, 100, 1000, 50),
		assistant("m1", "r1", "claude-opus-5-5", 2, 100, 1000, 50), // same response, next block
		assistant("m1", "r1", "claude-opus-5-5", 2, 100, 1000, 50),
		`{"type":"attachment","attachment":{"usage":"not a usage object"}}`,
		assistant("m2", "r2", "claude-opus-5-5", 3, 200, 2000, 0),
		assistant("m3", "r3", "<synthetic>", 0, 0, 0, 0),
		assistant("m4", "r4", "claude-fable-5-1", 10, 20, 0, 0),
	)
	write(t, filepath.Join(dir, "sess", "subagents", "agent-a1.jsonl"),
		assistant("s1", "rs1", "claude-fable-5-1", 5, 7, 0, 0),
	)

	var tr Tracker
	if err := tr.Update(main); err != nil {
		t.Fatal(err)
	}
	want := []Totals{
		{Model: "claude-fable-5-1", Speed: "standard", Requests: 1, InputTokens: 10, OutputTokens: 20, WebFetchRequests: 1},
		{Model: "claude-opus-5-5", Speed: "standard", Requests: 2, InputTokens: 5, OutputTokens: 300, CacheReadInputTokens: 3000, CacheCreation1hInputTokens: 50, WebFetchRequests: 2},
		{Model: "claude-fable-5-1", Speed: "standard", Subagent: true, Requests: 1, InputTokens: 5, OutputTokens: 7, WebFetchRequests: 1},
	}
	if got := tr.Totals(); !reflect.DeepEqual(got, want) {
		t.Errorf("Totals =\n%+v\nwant\n%+v", got, want)
	}
}

// Appended lines are read incrementally; a partial last line waits, and a
// response whose blocks straddle two reads still counts once.
func TestUpdateIsIncremental(t *testing.T) {
	path := filepath.Join(t.TempDir(), "sess.jsonl")
	write(t, path, assistant("m1", "r1", "m", 1, 1, 0, 0))
	var tr Tracker
	tr.Update(path)

	partial := assistant("m2", "r2", "m", 1, 1, 0, 0)
	appendTo(t, path, assistant("m1", "r1", "m", 1, 1, 0, 0)+"\n"+partial[:20])
	tr.Update(path)
	if got := tr.Totals()[0].Requests; got != 1 {
		t.Fatalf("after partial append: %d requests, want 1", got)
	}
	appendTo(t, path, partial[20:]+"\n")
	tr.Update(path)
	if got := tr.Totals()[0].Requests; got != 2 {
		t.Errorf("after completing the line: %d requests, want 2", got)
	}
	tr.Update(path) // nothing new
	if got := tr.Totals()[0].Requests; got != 2 {
		t.Errorf("re-read counted twice: %d", got)
	}
}

// A transcript that shrank was rewritten: its counts start over instead of
// adding to the old ones.
func TestRewrittenTranscriptStartsOver(t *testing.T) {
	path := filepath.Join(t.TempDir(), "sess.jsonl")
	write(t, path, assistant("m1", "r1", "m", 1, 1, 0, 0), assistant("m2", "r2", "m", 1, 1, 0, 0))
	var tr Tracker
	tr.Update(path)
	write(t, path, assistant("m9", "r9", "m", 1, 1, 0, 0))
	tr.Update(path)
	if got := tr.Totals()[0].Requests; got != 1 {
		t.Errorf("requests = %d, want 1 after rewrite", got)
	}
}

// Without the 5m/1h breakdown, cache writes count as the default (5 minute)
// duration.
func TestCacheCreationWithoutBreakdown(t *testing.T) {
	path := filepath.Join(t.TempDir(), "sess.jsonl")
	write(t, path, `{"type":"assistant","requestId":"r","message":{"id":"m","model":"x","usage":{"input_tokens":1,"output_tokens":1,"cache_creation_input_tokens":42}}}`)
	var tr Tracker
	tr.Update(path)
	if got := tr.Totals()[0]; got.CacheCreation5mInputTokens != 42 || got.CacheCreation1hInputTokens != 0 || got.Speed != "standard" {
		t.Errorf("totals = %+v", got)
	}
}

func TestIgnoresMissingAndUnexpectedPaths(t *testing.T) {
	var tr Tracker
	for _, p := range []string{"", "relative.jsonl", "/etc/passwd", filepath.Join(t.TempDir(), "missing.jsonl")} {
		if err := tr.Update(p); err != nil {
			t.Errorf("Update(%q) = %v", p, err)
		}
	}
	if len(tr.Totals()) != 0 {
		t.Errorf("totals from nothing: %+v", tr.Totals())
	}
}
