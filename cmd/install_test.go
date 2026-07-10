package cmd

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
)

// readSettings parses a .claude/settings.json under repoDir for assertions.
func readSettings(t *testing.T, repoDir string) map[string]any {
	t.Helper()
	doc, err := readJSONObject(filepath.Join(repoDir, ".claude", "settings.json"))
	if err != nil {
		t.Fatalf("read settings: %v", err)
	}
	return doc
}

// commandsFor returns every hook command string registered under an event.
func commandsFor(t *testing.T, doc map[string]any, event string) []string {
	t.Helper()
	hooks, _ := doc["hooks"].(map[string]any)
	arr, _ := hooks[event].([]any)
	var cmds []string
	for _, g := range arr {
		gm, _ := g.(map[string]any)
		for _, h := range asSlice(gm, "hooks") {
			hm, _ := h.(map[string]any)
			if c, ok := hm["command"].(string); ok {
				cmds = append(cmds, c)
			}
		}
	}
	return cmds
}

func contains(ss []string, want string) bool {
	for _, s := range ss {
		if s == want {
			return true
		}
	}
	return false
}

func TestInstallFreshCreatesConfig(t *testing.T) {
	repo := t.TempDir()
	if err := os.Mkdir(filepath.Join(repo, ".claude"), 0o755); err != nil {
		t.Fatal(err)
	}

	var out bytes.Buffer
	if err := runInstall(&out, repo, installOpts{binaryRef: "swarmia-cli"}); err != nil {
		t.Fatalf("install: %v", err)
	}

	doc := readSettings(t, repo)
	// Every managed event carries the log-hook command.
	for _, ev := range []string{"UserPromptSubmit", "PreToolUse", "PostToolUse", "SessionStart", "SessionEnd", "Stop"} {
		if !contains(commandsFor(t, doc, ev), "swarmia-cli log-hook") {
			t.Errorf("%s missing log-hook; got %v", ev, commandsFor(t, doc, ev))
		}
	}
	// SessionEnd also ships the logs, and log-hook comes before process-logs.
	se := commandsFor(t, doc, "SessionEnd")
	if len(se) != 2 || se[0] != "swarmia-cli log-hook" || se[1] != "swarmia-cli process-logs" {
		t.Errorf("SessionEnd = %v, want [log-hook, process-logs]", se)
	}
	// PreToolUse uses the "*" matcher.
	hooks := doc["hooks"].(map[string]any)
	grp := hooks["PreToolUse"].([]any)[0].(map[string]any)
	if grp["matcher"] != "*" {
		t.Errorf("PreToolUse matcher = %v, want *", grp["matcher"])
	}
}

func TestInstallIdempotent(t *testing.T) {
	repo := t.TempDir()
	if err := os.Mkdir(filepath.Join(repo, ".claude"), 0o755); err != nil {
		t.Fatal(err)
	}
	var out bytes.Buffer
	if err := runInstall(&out, repo, installOpts{binaryRef: "swarmia-cli"}); err != nil {
		t.Fatalf("install 1: %v", err)
	}
	first, err := os.ReadFile(filepath.Join(repo, ".claude", "settings.json"))
	if err != nil {
		t.Fatal(err)
	}

	out.Reset()
	if err := runInstall(&out, repo, installOpts{binaryRef: "swarmia-cli"}); err != nil {
		t.Fatalf("install 2: %v", err)
	}
	second, err := os.ReadFile(filepath.Join(repo, ".claude", "settings.json"))
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(first, second) {
		t.Errorf("second install changed the file:\n%s", second)
	}
	if !bytes.Contains(out.Bytes(), []byte("already configured")) {
		t.Errorf("expected 'already configured', got: %s", out.String())
	}
}

// A different --binary must not create duplicate entries: detection is
// path-agnostic (matches any swarmia-cli command for the subcommand).
func TestInstallDifferentBinaryNoDuplicate(t *testing.T) {
	repo := t.TempDir()
	if err := os.Mkdir(filepath.Join(repo, ".claude"), 0o755); err != nil {
		t.Fatal(err)
	}
	var out bytes.Buffer
	if err := runInstall(&out, repo, installOpts{binaryRef: "${CLAUDE_PROJECT_DIR}/swarmia-cli"}); err != nil {
		t.Fatal(err)
	}
	out.Reset()
	if err := runInstall(&out, repo, installOpts{binaryRef: "swarmia-cli"}); err != nil {
		t.Fatal(err)
	}
	doc := readSettings(t, repo)
	if got := commandsFor(t, doc, "PreToolUse"); len(got) != 1 {
		t.Errorf("PreToolUse has %d commands, want 1 (no duplicate): %v", len(got), got)
	}
	if !bytes.Contains(out.Bytes(), []byte("already configured")) {
		t.Errorf("expected 'already configured', got: %s", out.String())
	}
}

// Unrelated settings and unrelated hooks must survive an install.
func TestInstallPreservesExisting(t *testing.T) {
	repo := t.TempDir()
	claude := filepath.Join(repo, ".claude")
	if err := os.Mkdir(claude, 0o755); err != nil {
		t.Fatal(err)
	}
	existing := map[string]any{
		"model": "opus",
		"hooks": map[string]any{
			"PreToolUse": []any{
				map[string]any{
					"matcher": "Bash",
					"hooks":   []any{map[string]any{"type": "command", "command": "/usr/local/bin/audit"}},
				},
			},
		},
	}
	data, _ := json.MarshalIndent(existing, "", "  ")
	if err := os.WriteFile(filepath.Join(claude, "settings.json"), data, 0o644); err != nil {
		t.Fatal(err)
	}

	var out bytes.Buffer
	if err := runInstall(&out, repo, installOpts{binaryRef: "swarmia-cli"}); err != nil {
		t.Fatalf("install: %v", err)
	}

	doc := readSettings(t, repo)
	if doc["model"] != "opus" {
		t.Errorf("unrelated key 'model' lost: %v", doc["model"])
	}
	cmds := commandsFor(t, doc, "PreToolUse")
	if !contains(cmds, "/usr/local/bin/audit") {
		t.Errorf("unrelated Bash hook lost: %v", cmds)
	}
	if !contains(cmds, "swarmia-cli log-hook") {
		t.Errorf("swarmia log-hook not added: %v", cmds)
	}
}

func TestInstallSkipsWhenNoClaudeDir(t *testing.T) {
	repo := t.TempDir() // no .claude
	var out bytes.Buffer
	if err := runInstall(&out, repo, installOpts{binaryRef: "swarmia-cli"}); err != nil {
		t.Fatalf("install: %v", err)
	}
	if _, err := os.Stat(filepath.Join(repo, ".claude", "settings.json")); !os.IsNotExist(err) {
		t.Errorf("settings.json should not have been created")
	}
	if !bytes.Contains(out.Bytes(), []byte("No supported coding agents detected")) {
		t.Errorf("expected no-agents message, got: %s", out.String())
	}
}

func TestInstallDryRunWritesNothing(t *testing.T) {
	repo := t.TempDir()
	if err := os.Mkdir(filepath.Join(repo, ".claude"), 0o755); err != nil {
		t.Fatal(err)
	}
	var out bytes.Buffer
	if err := runInstall(&out, repo, installOpts{binaryRef: "swarmia-cli", dryRun: true}); err != nil {
		t.Fatalf("install: %v", err)
	}
	if _, err := os.Stat(filepath.Join(repo, ".claude", "settings.json")); !os.IsNotExist(err) {
		t.Errorf("dry-run must not write settings.json")
	}
	if !bytes.Contains(out.Bytes(), []byte("would add")) {
		t.Errorf("expected 'would add', got: %s", out.String())
	}
}

func TestDetectBinaryRefPrefersLocal(t *testing.T) {
	repo := t.TempDir()
	// No local binary: falls through to PATH assumption (bare name).
	if ref, _ := detectBinaryRef(repo); ref != "swarmia-cli" {
		t.Errorf("without local binary, ref = %q, want bare swarmia-cli", ref)
	}
	// With an executable at the repo root: prefer the repo-local reference.
	local := filepath.Join(repo, binaryName)
	if err := os.WriteFile(local, []byte("#!/bin/sh\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	if ref, src := detectBinaryRef(repo); ref != "${CLAUDE_PROJECT_DIR}/swarmia-cli" {
		t.Errorf("with local binary, ref = %q (%s), want ${CLAUDE_PROJECT_DIR}/swarmia-cli", ref, src)
	}
}
