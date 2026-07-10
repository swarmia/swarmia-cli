package cmd

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
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

// --- merging with existing user configurations ------------------------------

// writeSettings seeds a repo's .claude/settings.json from a doc, as if the user
// already had Claude Code configured.
func writeSettings(t *testing.T, repoDir string, doc map[string]any) {
	t.Helper()
	claude := filepath.Join(repoDir, ".claude")
	if err := os.MkdirAll(claude, 0o755); err != nil {
		t.Fatal(err)
	}
	data, err := json.MarshalIndent(doc, "", "  ")
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(claude, "settings.json"), data, 0o644); err != nil {
		t.Fatal(err)
	}
}

// grp builds a hook matcher-group. An empty matcher omits the key.
func grp(matcher, command string) map[string]any {
	g := map[string]any{"hooks": []any{map[string]any{"type": "command", "command": command}}}
	if matcher != "" {
		g["matcher"] = matcher
	}
	return g
}

func groupHasCmd(group map[string]any, command string) bool {
	for _, h := range asSlice(group, "hooks") {
		hm, _ := h.(map[string]any)
		if c, _ := hm["command"].(string); c == command {
			return true
		}
	}
	return false
}

// The user already runs their own command on the same event+matcher we target;
// ours is appended to that group and theirs is kept (no new duplicate group).
func TestInstallMergesIntoSameMatcherGroup(t *testing.T) {
	repo := t.TempDir()
	writeSettings(t, repo, map[string]any{
		"hooks": map[string]any{
			"PreToolUse": []any{grp("*", "my-linter")},
		},
	})

	var out bytes.Buffer
	if err := runInstall(&out, repo, installOpts{binaryRef: "swarmia-cli"}); err != nil {
		t.Fatal(err)
	}

	doc := readSettings(t, repo)
	cmds := commandsFor(t, doc, "PreToolUse")
	if !contains(cmds, "my-linter") {
		t.Errorf("user command dropped: %v", cmds)
	}
	if !contains(cmds, "swarmia-cli log-hook") {
		t.Errorf("our log-hook not added: %v", cmds)
	}
	if n := len(doc["hooks"].(map[string]any)["PreToolUse"].([]any)); n != 1 {
		t.Errorf("PreToolUse groups = %d, want 1 (merged into existing * group)", n)
	}
}

// The user has their own hook under a different matcher; ours goes in its own
// group and both coexist.
func TestInstallCoexistsWithDifferentMatcher(t *testing.T) {
	repo := t.TempDir()
	writeSettings(t, repo, map[string]any{
		"hooks": map[string]any{
			"PreToolUse": []any{grp("Bash", "audit-bash")},
		},
	})

	var out bytes.Buffer
	if err := runInstall(&out, repo, installOpts{binaryRef: "swarmia-cli"}); err != nil {
		t.Fatal(err)
	}

	groups := readSettings(t, repo)["hooks"].(map[string]any)["PreToolUse"].([]any)
	if len(groups) != 2 {
		t.Fatalf("PreToolUse groups = %d, want 2 (Bash + *)", len(groups))
	}
	var haveBash, haveStar bool
	for _, g := range groups {
		gm := g.(map[string]any)
		switch gm["matcher"] {
		case "Bash":
			haveBash = groupHasCmd(gm, "audit-bash")
		case "*":
			haveStar = groupHasCmd(gm, "swarmia-cli log-hook")
		}
	}
	if !haveBash {
		t.Errorf("user's Bash group not preserved")
	}
	if !haveStar {
		t.Errorf("our * group with log-hook not added")
	}
}

// The user's own SessionEnd hook is kept, and our two commands are appended in
// order (log-hook then process-logs).
func TestInstallMergesUserSessionEndHook(t *testing.T) {
	repo := t.TempDir()
	writeSettings(t, repo, map[string]any{
		"hooks": map[string]any{
			"SessionEnd": []any{grp("", "notify-done")},
		},
	})

	var out bytes.Buffer
	if err := runInstall(&out, repo, installOpts{binaryRef: "swarmia-cli"}); err != nil {
		t.Fatal(err)
	}

	se := commandsFor(t, readSettings(t, repo), "SessionEnd")
	want := []string{"notify-done", "swarmia-cli log-hook", "swarmia-cli process-logs"}
	if !reflect.DeepEqual(se, want) {
		t.Errorf("SessionEnd = %v, want %v", se, want)
	}
}

// An older/partial Swarmia install (log-hook everywhere, but process-logs missing
// from SessionEnd) is completed by adding exactly the missing hook, with no
// duplication of the ones already present.
func TestInstallCompletesPartialSwarmiaInstall(t *testing.T) {
	repo := t.TempDir()
	writeSettings(t, repo, map[string]any{
		"hooks": map[string]any{
			"UserPromptSubmit": []any{grp("", "swarmia-cli log-hook")},
			"PreToolUse":       []any{grp("*", "swarmia-cli log-hook")},
			"PostToolUse":      []any{grp("*", "swarmia-cli log-hook")},
			"SessionStart":     []any{grp("", "swarmia-cli log-hook")},
			"SessionEnd":       []any{grp("", "swarmia-cli log-hook")},
			"Stop":             []any{grp("", "swarmia-cli log-hook")},
		},
	})

	var out bytes.Buffer
	if err := runInstall(&out, repo, installOpts{binaryRef: "swarmia-cli"}); err != nil {
		t.Fatal(err)
	}
	if !bytes.Contains(out.Bytes(), []byte("added 1 hook")) {
		t.Errorf("expected exactly 1 hook added (process-logs), got: %s", out.String())
	}

	doc := readSettings(t, repo)
	if se := commandsFor(t, doc, "SessionEnd"); len(se) != 2 ||
		se[0] != "swarmia-cli log-hook" || se[1] != "swarmia-cli process-logs" {
		t.Errorf("SessionEnd = %v, want [log-hook, process-logs]", se)
	}
	for _, ev := range []string{"UserPromptSubmit", "PreToolUse", "PostToolUse", "SessionStart", "Stop"} {
		if cmds := commandsFor(t, doc, ev); len(cmds) != 1 || cmds[0] != "swarmia-cli log-hook" {
			t.Errorf("%s = %v, want a single, un-duplicated log-hook", ev, cmds)
		}
	}
}

// Unrelated top-level settings and hooks for events we don't manage survive an
// install untouched.
func TestInstallPreservesUnmanagedHooksAndTopLevelKeys(t *testing.T) {
	repo := t.TempDir()
	writeSettings(t, repo, map[string]any{
		"model":       "opus",
		"permissions": map[string]any{"allow": []any{"Bash(git*)"}},
		"env":         map[string]any{"FOO": "bar"},
		"hooks": map[string]any{
			"Notification": []any{grp("", "ping-me")},
			"PreCompact":   []any{grp("manual", "snapshot")},
		},
	})

	var out bytes.Buffer
	if err := runInstall(&out, repo, installOpts{binaryRef: "swarmia-cli"}); err != nil {
		t.Fatal(err)
	}

	doc := readSettings(t, repo)
	if doc["model"] != "opus" {
		t.Errorf("top-level 'model' lost: %v", doc["model"])
	}
	if perms, _ := doc["permissions"].(map[string]any); perms == nil {
		t.Errorf("permissions lost")
	} else if allow, _ := perms["allow"].([]any); len(allow) != 1 || allow[0] != "Bash(git*)" {
		t.Errorf("permissions altered: %v", doc["permissions"])
	}
	if env, _ := doc["env"].(map[string]any); env == nil || env["FOO"] != "bar" {
		t.Errorf("env lost: %v", doc["env"])
	}
	if !contains(commandsFor(t, doc, "Notification"), "ping-me") {
		t.Errorf("unmanaged Notification hook lost")
	}
	if !contains(commandsFor(t, doc, "PreCompact"), "snapshot") {
		t.Errorf("unmanaged PreCompact hook lost")
	}
	if !contains(commandsFor(t, doc, "SessionEnd"), "swarmia-cli process-logs") {
		t.Errorf("our hooks were not added alongside the existing config")
	}
}
