package cmd

import (
	"encoding/json"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"testing"
	"time"
)

// contractTables reads the field tables of docs/ingest-api.md: for each of a
// session payload's object types, its fields and whether each is required
// (its type doesn't say optional).
func contractTables(t *testing.T) map[string]map[string]bool {
	t.Helper()
	doc, err := os.ReadFile(filepath.Join("..", "docs", "ingest-api.md"))
	if err != nil {
		t.Fatal(err)
	}
	heading := regexp.MustCompile(`^#{3,4} (\w+)`)
	row := regexp.MustCompile("^\\| `([a-z_0-9]+)` \\| ([^|]+)\\|")
	tables := map[string]map[string]bool{}
	section := ""
	for _, line := range strings.Split(string(doc), "\n") {
		if m := heading.FindStringSubmatch(line); m != nil {
			section = m[1]
		} else if m := row.FindStringSubmatch(line); m != nil && section != "" {
			if tables[section] == nil {
				tables[section] = map[string]bool{}
			}
			tables[section][m[1]] = !strings.Contains(m[2], "optional")
		}
	}
	for _, s := range []string{"SessionPayload", "Repository", "Usage", "Prompt", "Tool", "CommitAttribution", "LineRange"} {
		if len(tables[s]) == 0 {
			t.Fatalf("docs/ingest-api.md has no field table for %s", s)
		}
	}
	return tables
}

// What `report --json` prints is what docs/ingest-api.md describes: every
// field it sends is documented, every required one is there, every
// documented one shows up for a session that has it, and the values follow
// the rules the descriptions state. The session spans two repositories, so
// the multi-repository rules are exercised too.
func TestReportJSONFollowsTheContract(t *testing.T) {
	tables := contractTables(t)
	parent := tempDir(t)
	app, lib := newRepoAt(t, filepath.Join(parent, "app")), newRepoAt(t, filepath.Join(parent, "lib"))
	projectIn(t, parent)

	transcript := filepath.Join(t.TempDir(), "s.jsonl")
	writeFile(t, transcript, `{"type":"assistant","requestId":"r1","message":{"id":"m1","model":"claude-test","usage":{"input_tokens":3,"output_tokens":5,"cache_read_input_tokens":7}}}`+"\n")
	hook(t, map[string]any{"hook_event_name": "SessionStart", "model": "claude-test", "transcript_path": transcript, "cwd": parent})
	hook(t, map[string]any{"hook_event_name": "UserPromptSubmit", "prompt": "add the helper, the key is ghp_" + strings.Repeat("a1B2", 9), "cwd": parent})
	hook(t, map[string]any{"hook_event_name": "PostToolUse", "tool_name": "Read", "cwd": parent,
		"tool_input": map[string]any{"file_path": filepath.Join(app, "README.md")}})
	agentWritesAndCommits(t, app, filepath.Join(app, "helper.go"), "package app\n\nfunc Helper() string { return \"from the agent\" }\n")
	shellCommand(t, parent, "cd lib && ./gen.sh > gen.go", func() {
		writeFile(t, filepath.Join(lib, "gen.go"), "package lib\n\nfunc Generated() string { return \"from the generator\" }\n")
	})
	git(t, lib, "add", ".")
	git(t, lib, "commit", "-q", "-m", "generate")
	hook(t, map[string]any{"hook_event_name": "PostToolUseFailure", "tool_name": "Bash", "tool_use_id": "toolu_test", "cwd": app,
		"tool_input": map[string]any{"command": "go test ./..."}, "error": "Exit code 1\nFAIL"})
	hook(t, map[string]any{"hook_event_name": "SessionEnd", "cwd": parent})
	pushed(t, app)
	pushed(t, lib)

	var payloads []map[string]any
	if err := json.Unmarshal([]byte(runReportFor(t, true)), &payloads); err != nil {
		t.Fatal(err)
	}
	if len(payloads) != 1 {
		t.Fatalf("got %d payloads, want 1", len(payloads))
	}
	p := payloads[0]

	// Every field sent is documented; every required one is there. seen
	// collects what showed up, per table.
	seen := map[string]map[string]bool{}
	var check func(section, where string, obj map[string]any)
	check = func(section, where string, obj map[string]any) {
		spec := tables[section]
		if seen[section] == nil {
			seen[section] = map[string]bool{}
		}
		for k := range obj {
			seen[section][k] = true
			if _, ok := spec[k]; !ok {
				t.Errorf("%s: field %q isn't documented in %s", where, k, section)
			}
		}
		for k, required := range spec {
			if _, ok := obj[k]; required && !ok {
				t.Errorf("%s: required field %q (%s) is missing", where, k, section)
			}
		}
	}
	objects := func(v any) []map[string]any {
		var out []map[string]any
		for _, x := range v.([]any) {
			out = append(out, x.(map[string]any))
		}
		return out
	}
	check("SessionPayload", "session", p)
	for _, r := range objects(p["repos"]) {
		check("Repository", "repos[]", r)
	}
	for _, x := range objects(p["prompts"]) {
		check("Prompt", "prompts[]", x)
	}
	for _, x := range objects(p["tools"]) {
		check("Tool", "tools[]", x)
	}
	for _, c := range objects(p["commits"]) {
		check("CommitAttribution", "commits[]", c)
		for _, l := range objects(c["lines"]) {
			check("LineRange", "commits[].lines[]", l)
		}
	}
	for _, u := range objects(p["usage"]) {
		check("Usage", "usage[]", u)
	}

	// Every documented field shows up, but for those this session can't have.
	cantHave := map[string]string{
		"Usage.subagent":                "no subagents",
		"Usage.reasoning_output_tokens": "Codex only",
		"Usage.web_search_requests":     "no web searches",
		"Usage.web_fetch_requests":      "no web fetches",
	}
	for section, spec := range tables {
		if seen[section] == nil {
			continue // a table that isn't part of a session payload (the request body, results)
		}
		for k := range spec {
			if _, why := cantHave[section+"."+k]; !why && !seen[section][k] {
				t.Errorf("documented field %s.%s never appeared; is the doc ahead of the code?", section, k)
			}
		}
	}

	// The rules the descriptions state.
	var slugs []string
	for _, r := range objects(p["repos"]) {
		slugs = append(slugs, r["repo"].(string))
	}
	slices.Sort(slugs)
	if want := []string{"github.com/acme/app", "github.com/acme/lib"}; !slices.Equal(slugs, want) {
		t.Errorf("repos = %q, want %q", slugs, want)
	}
	for _, k := range []string{"started_at", "ended_at"} {
		if s, _ := p[k].(string); !strings.HasSuffix(s, "Z") {
			t.Errorf("%s = %v, want RFC 3339 UTC", k, p[k])
		} else if _, err := time.Parse(time.RFC3339, s); err != nil {
			t.Errorf("%s: %v", k, err)
		}
	}
	for _, x := range objects(p["tools"]) {
		repo, hasRepo := x["repo"].(string)
		if hasRepo && !slices.Contains(slugs, repo) {
			t.Errorf("tool repo %q isn't in repos", repo)
		}
		if file, ok := x["file"].(string); ok && (!hasRepo || strings.HasPrefix(file, "/") || strings.Contains(file, "..")) {
			t.Errorf("tool file %q: want it relative to the tool's repo", file)
		}
		if _, ok := x["exit_code"]; ok && x["status"] != "failed" && x["status"] != "interrupted" {
			t.Errorf("exit_code on a %v call", x["status"])
		}
	}
	shaRE := regexp.MustCompile(`^([0-9a-f]{40}|[0-9a-f]{64})$`)
	for _, c := range objects(p["commits"]) {
		if !slices.Contains(slugs, c["repo"].(string)) || !shaRE.MatchString(c["sha"].(string)) {
			t.Errorf("commit %v: repo not in repos, or not a full sha", c)
		}
		prev := 0.0
		for _, l := range objects(c["lines"]) {
			start, end := l["start"].(float64), l["end"].(float64)
			if start < 1 || end < start || start <= prev || !slices.Contains([]string{"exact", "normalized", "modified"}, l["class"].(string)) {
				t.Errorf("line range %v: want ascending, non-overlapping, a known class", l)
			}
			prev = end
		}
	}
	data, _ := json.Marshal(p)
	for _, leak := range []string{parent, "ghp_"} {
		if strings.Contains(string(data), leak) {
			t.Errorf("payload contains %q", leak)
		}
	}
}
