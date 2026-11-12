package cmd

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestPiPrivacyAndLifecycleContract(t *testing.T) {
	repo := newRepoAt(t, filepath.Join(tempDir(t), "app"))
	projectIn(t, repo)
	old := logAgent
	logAgent = "pi"
	t.Cleanup(func() { logAgent = old })

	transcript := filepath.Join(t.TempDir(), "pi.jsonl")
	writeFile(t, transcript, `{"type":"session","id":"s"}`+"\n"+`{"type":"message","id":"m","message":{"role":"assistant","model":"pi-test","usage":{"input":4,"output":3},"content":[{"type":"text","text":"TRANSCRIPT_PRIVATE"}]}}`+"\n")
	base := map[string]any{"cwd": repo, "transcript_path": transcript}
	send := func(name string, extras map[string]any) {
		event := map[string]any{"hook_event_name": name}
		for k, v := range base {
			event[k] = v
		}
		for k, v := range extras {
			event[k] = v
		}
		hook(t, event)
	}
	send("SessionStart", map[string]any{"model": "pi-test"})
	send("UserPromptSubmit", map[string]any{"prompt": "PRIVATE_PROMPT"})
	file := filepath.Join(repo, "source.go")
	source := "package sample\nvar PrivateMarker = \"SOURCE_PRIVATE\"\n"
	send("PostToolUse", map[string]any{"tool_name": "write", "tool_input": map[string]any{"file_path": file, "content": source}, "original_file": "", "after_file": source, "tool_response": "TOOL_PRIVATE"})
	writeFile(t, file, source)
	git(t, repo, "add", "source.go")
	git(t, repo, "commit", "-q", "-m", "add sample")
	pushed(t, repo)
	send("Stop", map[string]any{"last_assistant_message": "Done.\n```go\nSOURCE_PRIVATE\n```"})
	send("SessionFlush", nil)
	data := runReportFor(t, true)
	if strings.Contains(data, "SOURCE_PRIVATE") || strings.Contains(data, "TOOL_PRIVATE") || strings.Contains(data, "TRANSCRIPT_PRIVATE") {
		t.Fatalf("private tool/source content leaked: %s", data)
	}
	var payloads []map[string]any
	if err := json.Unmarshal([]byte(data), &payloads); err != nil || len(payloads) != 1 {
		t.Fatalf("payloads %s: %v", data, err)
	}
	if payloads[0]["ended"] != false {
		t.Fatal("reload flush marked session ended")
	}
	if !strings.Contains(data, "PRIVATE_PROMPT") || !strings.Contains(data, "pi-test") {
		t.Fatal("prompt or usage missing")
	}
	t.Setenv("SWARMIA_PROMPTS", "0")
	withheld := runReportFor(t, true)
	if strings.Contains(withheld, "PRIVATE_PROMPT") || strings.Contains(withheld, "Done.") || !strings.Contains(withheld, `"prompts_withheld": true`) {
		t.Fatalf("opt-out failed: %s", withheld)
	}
	path, err := eventsPath("")
	if err != nil {
		t.Fatal(err)
	}
	if fi, err := os.Stat(path); err != nil || fi.Mode().Perm()&0o077 != 0 {
		t.Fatalf("event log permissions: %v %v", fi, err)
	}
	sessions, err := os.ReadDir(filepath.Join(filepath.Dir(path), "sessions"))
	if err != nil || len(sessions) != 1 {
		t.Fatalf("sessions: %v %v", sessions, err)
	}
	if fi, err := sessions[0].Info(); err != nil || fi.Mode().Perm()&0o077 != 0 {
		t.Fatalf("session permissions: %v %v", fi, err)
	}
}
