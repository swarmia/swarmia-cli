package usage

import (
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

func TestPiUsageIncrementalAndFork(t *testing.T) {
	dir := t.TempDir()
	parent := filepath.Join(dir, "parent.jsonl")
	fork := filepath.Join(dir, "fork.jsonl")
	one := `{"type":"message","id":"a","message":{"role":"assistant","model":"m1","content":[{"type":"text","text":"secret"}],"usage":{"input":10,"output":5,"cacheRead":20,"cacheWrite":3,"reasoning":2}}}`
	two := `{"type":"message","id":"b","message":{"role":"assistant","model":"m2","usage":{"input":7,"output":4,"cacheRead":0,"cacheWrite":0}}}`
	header, _ := json.Marshal(map[string]string{"type": "session", "parentSession": parent})
	write(t, parent, `{"type":"session","id":"parent"}`, one)
	write(t, fork, string(header), one)
	var tr Tracker
	if err := tr.Update(PiTranscript, fork); err != nil {
		t.Fatal(err)
	}
	if len(tr.Totals()) != 0 {
		t.Fatal("fork counted copied parent response")
	}
	appendTo(t, fork, two[:25])
	tr.Update(PiTranscript, fork)
	if len(tr.Totals()) != 0 {
		t.Fatal("counted incomplete entry")
	}
	appendTo(t, fork, two[25:]+"\n")
	tr.Update(PiTranscript, fork)
	tr.Update(PiTranscript, fork)
	want := []Totals{{Model: "m2", Speed: "standard", Requests: 1, InputTokens: 7, OutputTokens: 4}}
	if !reflect.DeepEqual(tr.Totals(), want) {
		t.Fatalf("got %+v", tr.Totals())
	}
	state, _ := json.Marshal(tr)
	if strings.Contains(string(state), "secret") {
		t.Fatal("stored transcript content")
	}
	var orig Tracker
	orig.Update(PiTranscript, parent)
	expected := []Totals{{Model: "m1", Speed: "standard", Requests: 1, InputTokens: 10, OutputTokens: 5, CacheReadInputTokens: 20, CacheCreation5mInputTokens: 3, ReasoningOutputTokens: 2}}
	if !reflect.DeepEqual(orig.Totals(), expected) {
		t.Fatalf("got %+v", orig.Totals())
	}
	appendTo(t, parent, two+"\n")
	orig.Update(PiTranscript, parent)
	if len(orig.Totals()) != 2 {
		t.Fatalf("model switch: %+v", orig.Totals())
	}
}

func TestPiAuxiliaryUsage(t *testing.T) {
	path := filepath.Join(t.TempDir(), "session.jsonl")
	write(t, path, `{"type":"session"}`,
		`{"type":"usage","kind":"cache_warm","model":"m","usage":{"input":2,"cacheRead":5,"output":1}}`,
		`{"type":"message","message":{"role":"toolResult","usage":{"input":100,"output":200}}}`,
		`{"type":"compaction","usage":{"input":300,"output":400}}`,
		`{"type":"branch_summary","usage":{"input":500,"output":600}}`,
		`{"type":"usage","kind":"extension-defined","model":"m","usage":{"input":900}}`)
	var tr Tracker
	if err := tr.Update(PiTranscript, path); err != nil {
		t.Fatal(err)
	}
	want := []Totals{{Model: "m", Speed: "standard", Requests: 1, InputTokens: 2, OutputTokens: 1, CacheReadInputTokens: 5}}
	if got := tr.Totals(); !reflect.DeepEqual(got, want) {
		t.Fatalf("got %+v, want %+v", got, want)
	}
}

func TestPiForkMissingParentDoesNotCountCopies(t *testing.T) {
	dir := t.TempDir()
	fork := filepath.Join(dir, "fork.jsonl")
	parent := filepath.Join(dir, "missing.jsonl")
	header, _ := json.Marshal(map[string]string{"type": "session", "parentSession": parent})
	line := `{"type":"message","id":"a","message":{"role":"assistant","model":"m","usage":{"input":5}}}`
	write(t, fork, string(header), line)
	var tr Tracker
	_ = tr.Update(PiTranscript, fork)
	if len(tr.Totals()) != 0 || tr.Files[fork].Offset != 0 {
		t.Fatal("counted or advanced without parent")
	}
	write(t, parent, `{"type":"session"}`, line)
	if err := tr.Update(PiTranscript, fork); err != nil {
		t.Fatal(err)
	}
	if len(tr.Totals()) != 0 {
		t.Fatal("counted inherited entry after parent restored")
	}
	_ = os.Remove(parent)
}
