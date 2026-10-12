package cmd

import (
	"bytes"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/swarmia/swarmia-cli/internal/agent"
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
	for _, ev := range []string{"UserPromptSubmit", "PreToolUse", "PostToolUse", "PostToolUseFailure", "SessionStart", "SessionEnd", "Stop"} {
		if !contains(commandsFor(t, doc, ev), "swarmia-cli log-hook") {
			t.Errorf("%s missing log-hook; got %v", ev, commandsFor(t, doc, ev))
		}
	}
	// Exactly one hook per event: log-hook only.
	if se := commandsFor(t, doc, "SessionEnd"); len(se) != 1 {
		t.Errorf("SessionEnd = %v, want just log-hook", se)
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
	if ref, _ := claudeRef(repo); ref != "swarmia-cli" {
		t.Errorf("without local binary, ref = %q, want bare swarmia-cli", ref)
	}
	// With an executable at the repo root: prefer the repo-local reference.
	local := filepath.Join(repo, binaryName)
	if err := os.WriteFile(local, []byte("#!/bin/sh\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	if ref, src := claudeRef(repo); ref != "${CLAUDE_PROJECT_DIR}/swarmia-cli" {
		t.Errorf("with local binary, ref = %q (%s), want ${CLAUDE_PROJECT_DIR}/swarmia-cli", ref, src)
	}
}

func TestDetectBinaryRefNodeModules(t *testing.T) {
	repo := t.TempDir()
	binDir := filepath.Join(repo, "node_modules", ".bin")
	if err := os.MkdirAll(binDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(binDir, binaryName), []byte("#!/bin/sh\n"), 0o755); err != nil {
		t.Fatal(err)
	}

	// With no repo-root binary, an npm dependency in node_modules/.bin is used.
	if ref, _ := claudeRef(repo); ref != "${CLAUDE_PROJECT_DIR}/node_modules/.bin/swarmia-cli" {
		t.Errorf("ref = %q, want the node_modules/.bin reference", ref)
	}

	// The package's own launcher is preferred over the .bin shim.
	pkgBin := filepath.Join(repo, "node_modules", "@swarmia", "cli", "bin")
	if err := os.MkdirAll(pkgBin, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(pkgBin, binaryName), []byte("#!/bin/sh\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	if ref, _ := claudeRef(repo); ref != "${CLAUDE_PROJECT_DIR}/node_modules/@swarmia/cli/bin/swarmia-cli" {
		t.Errorf("ref = %q, want the package launcher", ref)
	}

	// A repo-root binary still takes precedence over node_modules.
	if err := os.WriteFile(filepath.Join(repo, binaryName), []byte("#!/bin/sh\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	if ref, _ := claudeRef(repo); ref != "${CLAUDE_PROJECT_DIR}/swarmia-cli" {
		t.Errorf("with a repo-root binary present, ref = %q, want the repo-root reference", ref)
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

// The user's own SessionEnd hook is kept, and our log-hook is appended after it.
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
	want := []string{"notify-done", "swarmia-cli log-hook"}
	if !reflect.DeepEqual(se, want) {
		t.Errorf("SessionEnd = %v, want %v", se, want)
	}
}

// A partial Swarmia install (log-hook missing from some events) is completed by
// adding exactly the missing hooks, with no duplication of the ones present.
func TestInstallCompletesPartialSwarmiaInstall(t *testing.T) {
	repo := t.TempDir()
	writeSettings(t, repo, map[string]any{
		"hooks": map[string]any{
			"UserPromptSubmit": []any{grp("", "swarmia-cli log-hook")},
			"PreToolUse":       []any{grp("*", "swarmia-cli log-hook")},
			"PostToolUse":      []any{grp("*", "swarmia-cli log-hook")},
			"SessionStart":     []any{grp("", "swarmia-cli log-hook")},
			"SessionEnd":       []any{grp("", "swarmia-cli log-hook")},
		},
	})

	var out bytes.Buffer
	if err := runInstall(&out, repo, installOpts{binaryRef: "swarmia-cli"}); err != nil {
		t.Fatal(err)
	}
	if !bytes.Contains(out.Bytes(), []byte("added 2 hook")) {
		t.Errorf("expected exactly 2 hooks added (Stop, PostToolUseFailure), got: %s", out.String())
	}

	doc := readSettings(t, repo)
	for _, ev := range []string{"UserPromptSubmit", "PreToolUse", "PostToolUse", "PostToolUseFailure", "SessionStart", "SessionEnd", "Stop"} {
		if cmds := commandsFor(t, doc, ev); len(cmds) != 1 || cmds[0] != "swarmia-cli log-hook" {
			t.Errorf("%s = %v, want a single, un-duplicated log-hook", ev, cmds)
		}
	}
}

// Configs written by older versions wire `process-logs`, which no longer exists.
// Install removes it (however the binary is referenced) and leaves the user's
// hooks in the same group alone; a group holding only obsolete hooks disappears.
func TestInstallRemovesObsoleteProcessLogsHook(t *testing.T) {
	repo := t.TempDir()
	writeSettings(t, repo, map[string]any{
		"hooks": map[string]any{
			"SessionEnd": []any{
				map[string]any{"hooks": []any{
					map[string]any{"type": "command", "command": "notify-done"},
					map[string]any{"type": "command", "command": "${CLAUDE_PROJECT_DIR}/swarmia-cli log-hook"},
					map[string]any{"type": "command", "command": "${CLAUDE_PROJECT_DIR}/swarmia-cli process-logs"},
				}},
			},
			"Stop": []any{grp("", "swarmia-cli process-logs --push")},
		},
	})

	var out bytes.Buffer
	if err := runInstall(&out, repo, installOpts{binaryRef: "swarmia-cli"}); err != nil {
		t.Fatal(err)
	}
	if !bytes.Contains(out.Bytes(), []byte("removed 2 obsolete hook(s)")) {
		t.Errorf("expected 2 obsolete hooks removed, got: %s", out.String())
	}

	doc := readSettings(t, repo)
	se := commandsFor(t, doc, "SessionEnd")
	want := []string{"notify-done", "${CLAUDE_PROJECT_DIR}/swarmia-cli log-hook"}
	if !reflect.DeepEqual(se, want) {
		t.Errorf("SessionEnd = %v, want %v", se, want)
	}
	// Stop lost its process-logs-only group and got a fresh log-hook group.
	if st := commandsFor(t, doc, "Stop"); !reflect.DeepEqual(st, []string{"swarmia-cli log-hook"}) {
		t.Errorf("Stop = %v, want [swarmia-cli log-hook]", st)
	}

	// Re-running is a no-op.
	out.Reset()
	if err := runInstall(&out, repo, installOpts{binaryRef: "swarmia-cli"}); err != nil {
		t.Fatal(err)
	}
	if !bytes.Contains(out.Bytes(), []byte("already configured")) {
		t.Errorf("second install should be a no-op, got: %s", out.String())
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
	if !contains(commandsFor(t, doc, "SessionEnd"), "swarmia-cli log-hook") {
		t.Errorf("our hooks were not added alongside the existing config")
	}
}

// Installing into a committed settings file only adds lines: existing keys
// keep their order and formatting, and && isn't escaped, so the diff is just
// the hooks.
func TestInstallOnlyAddsLines(t *testing.T) {
	repo := t.TempDir()
	original := `{
  "permissions": {
    "allow": ["Bash(git*)", "Read"]
  },
  "hooks": {
    "PreToolUse": [
      {
        "matcher": "Bash",
        "hooks": [
          {
            "type": "command",
            "command": "cd \"$CLAUDE_PROJECT_DIR\" && ./guard.sh"
          }
        ]
      }
    ]
  },
  "env": {
    "FOO": "bar"
  }
}
`
	path := filepath.Join(repo, ".claude", "settings.json")
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(original), 0o644); err != nil {
		t.Fatal(err)
	}

	var out bytes.Buffer
	if err := runInstall(&out, repo, installOpts{binaryRef: "swarmia-cli"}); err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	got := strings.Split(string(data), "\n")
	i := 0 // the original's lines must appear in order, with only additions between
	for _, line := range got {
		if want := strings.Split(original, "\n"); i < len(want) && line == want[i] {
			i++
		}
	}
	if n := len(strings.Split(original, "\n")); i != n {
		t.Errorf("only %d of the original's %d lines kept in place:\n%s", i, n, data)
	}
	if bytes.Contains(data, []byte("\\u0026")) {
		t.Errorf("& was escaped:\n%s", data)
	}
	if !strings.Contains(string(data), "\"matcher\": \"*\",\n        \"hooks\": [") {
		t.Errorf("new hook groups should list matcher before hooks:\n%s", data)
	}
}

// Local scope writes the gitignored settings.local.json and leaves the shared
// settings.json alone.
func TestInstallLocalScope(t *testing.T) {
	repo := t.TempDir()
	writeSettings(t, repo, map[string]any{"model": "opus"})
	shared, _ := os.ReadFile(filepath.Join(repo, ".claude", "settings.json"))

	var out bytes.Buffer
	if err := runInstall(&out, repo, installOpts{binaryRef: "/opt/bin/swarmia-cli", scope: agent.ScopeLocal}); err != nil {
		t.Fatal(err)
	}
	if after, _ := os.ReadFile(filepath.Join(repo, ".claude", "settings.json")); !bytes.Equal(shared, after) {
		t.Errorf("shared settings.json changed:\n%s", after)
	}
	local, err := readJSONObject(filepath.Join(repo, ".claude", "settings.local.json"))
	if err != nil {
		t.Fatal(err)
	}
	if got := commandsFor(t, local, "PreToolUse"); !reflect.DeepEqual(got, []string{"/opt/bin/swarmia-cli log-hook"}) {
		t.Errorf("local PreToolUse = %v", got)
	}
}

// User scope writes Claude Code's user settings, works in a repo without a
// .claude directory, and marks its hooks to stand down in projects that have
// their own.
func TestInstallUserScope(t *testing.T) {
	configDir := t.TempDir()
	t.Setenv("CLAUDE_CONFIG_DIR", configDir)
	repo := t.TempDir() // no .claude here

	var out bytes.Buffer
	if err := runInstall(&out, repo, installOpts{binaryRef: "/opt/bin/swarmia-cli", scope: agent.ScopeUser}); err != nil {
		t.Fatal(err)
	}
	doc, err := readJSONObject(filepath.Join(configDir, "settings.json"))
	if err != nil {
		t.Fatal(err)
	}
	want := "/opt/bin/swarmia-cli log-hook --skip-if-project-hooks"
	for _, ev := range []string{"UserPromptSubmit", "PreToolUse", "PostToolUse", "PostToolUseFailure", "SessionStart", "SessionEnd", "Stop"} {
		if got := commandsFor(t, doc, ev); !reflect.DeepEqual(got, []string{want}) {
			t.Errorf("%s = %v, want [%s]", ev, got, want)
		}
	}
	if _, err := os.Stat(filepath.Join(repo, ".claude")); err == nil {
		t.Error("user-scope install created a .claude dir in the repo")
	}
}

// Without --binary, personal scopes point at this binary by absolute path.
func TestExecutableRefIsAbsolute(t *testing.T) {
	ref := executableRef(t.TempDir(), agent.ScopeLocal).cmd
	if !filepath.IsAbs(strings.Trim(ref, "'")) {
		t.Errorf("executableRef = %q, want an absolute path", ref)
	}
}

// Run from the repository's npm dependency, personal installs point at the
// package's launcher, not the platform binary's versioned path, so upgrading
// the dependency doesn't break them.
func TestRefForExecutableFromNpmDependency(t *testing.T) {
	repo, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	exe := filepath.Join(repo, "node_modules", ".pnpm", "@swarmia+cli-darwin-arm64@0.1.0",
		"node_modules", "@swarmia", "cli-darwin-arm64", "bin", binaryName)
	elsewhere := filepath.Join(t.TempDir(), "node_modules", "@swarmia", "cli-darwin-arm64", "bin", binaryName)

	// Without the launcher (say, a dependency of a workspace package only),
	// there's nothing better than the binary itself.
	if got := refForExecutable(exe, repo, agent.ScopeLocal); got.cmd != exe {
		t.Errorf("without a launcher: %+v, want the binary", got)
	}

	launcher := filepath.Join(repo, "node_modules", "@swarmia", "cli", "bin", binaryName)
	if err := os.MkdirAll(filepath.Dir(launcher), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(launcher, []byte("#!/bin/sh\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	if got := refForExecutable(exe, repo, agent.ScopeLocal); got.rel != "node_modules/@swarmia/cli/bin/swarmia-cli" {
		t.Errorf("local scope: %+v, want the launcher relative to the repository", got)
	}
	if got := refForExecutable(exe, repo, agent.ScopeUser); got.cmd != launcher {
		t.Errorf("user scope: %+v, want the launcher's absolute path", got)
	}
	// Another repository's or a global npm install is used as is.
	if got := refForExecutable(elsewhere, repo, agent.ScopeLocal); got.cmd != elsewhere {
		t.Errorf("binary outside the repository: %+v, want the binary", got)
	}
}

// Installing into one of a project's settings files while the other already
// logs events warns about double logging (but still installs).
func TestInstallWarnsAboutDuplicateProjectHooks(t *testing.T) {
	repo := t.TempDir()
	writeSettings(t, repo, map[string]any{"hooks": map[string]any{
		"Stop": []any{grp("", "${CLAUDE_PROJECT_DIR}/swarmia-cli log-hook")},
	}})

	var out bytes.Buffer
	if err := runInstall(&out, repo, installOpts{binaryRef: "/opt/bin/swarmia-cli", scope: agent.ScopeLocal}); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out.String(), "Warning: .claude/settings.json already runs swarmia-cli log-hook") {
		t.Errorf("no duplicate warning; got:\n%s", out.String())
	}
	if _, err := os.Stat(filepath.Join(repo, ".claude", "settings.local.json")); err != nil {
		t.Errorf("local settings not written: %v", err)
	}

	// The reverse direction warns too; a clean project doesn't.
	out.Reset()
	if err := runInstall(&out, repo, installOpts{binaryRef: "swarmia-cli", scope: agent.ScopeProject}); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out.String(), "Warning: .claude/settings.local.json already runs") {
		t.Errorf("no reverse warning; got:\n%s", out.String())
	}
	clean := t.TempDir()
	os.Mkdir(filepath.Join(clean, ".claude"), 0o755)
	out.Reset()
	if err := runInstall(&out, clean, installOpts{binaryRef: "swarmia-cli", scope: agent.ScopeLocal}); err != nil {
		t.Fatal(err)
	}
	if strings.Contains(out.String(), "Warning") {
		t.Errorf("unexpected warning for a clean project:\n%s", out.String())
	}
}

// claudeRef is how a Claude Code project hook would invoke swarmia-cli.
func claudeRef(repo string) (ref, source string) {
	r := detectBinaryRef(repo)
	if r.rel != "" {
		return "${CLAUDE_PROJECT_DIR}/" + r.rel, r.source
	}
	return r.cmd, r.source
}

// A project hook runs the repository's swarmia-cli with its arguments, stdin
// and exit code, and does nothing, quietly, until there is one (a fresh clone
// or worktree has no node_modules yet). The repository path may hold spaces.
func TestClaudeProjectHookCommand(t *testing.T) {
	repo := filepath.Join(t.TempDir(), "my repo")
	rel := "node_modules/@swarmia/cli/bin/swarmia-cli"
	command := claudeCodeAgent.hookCommand(binaryRef{rel: rel}, agent.ScopeProject)
	run := func() (string, error) {
		cmd := exec.Command("sh", "-c", command)
		cmd.Env = append(os.Environ(), "CLAUDE_PROJECT_DIR="+repo)
		cmd.Stdin = strings.NewReader("payload")
		out, err := cmd.CombinedOutput()
		return string(out), err
	}

	if out, err := run(); err != nil || out != "" {
		t.Errorf("without the binary: output %q, err %v; want nothing, exit 0", out, err)
	}

	bin := filepath.Join(repo, rel)
	if err := os.MkdirAll(filepath.Dir(bin), 0o755); err != nil {
		t.Fatal(err)
	}
	script := "#!/bin/sh\necho \"args: $*\"; cat; exit 3\n"
	if err := os.WriteFile(bin, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	out, err := run()
	if want := "args: log-hook\npayload"; out != want {
		t.Errorf("output = %q, want %q", out, want)
	}
	if exit, ok := err.(*exec.ExitError); !ok || exit.ExitCode() != 3 {
		t.Errorf("err = %v, want exit status 3", err)
	}
}

// Project hooks written by earlier versions, which failed on every event
// before node_modules was installed, are rewritten to the current command.
func TestInstallUpdatesLegacyProjectHooks(t *testing.T) {
	repo := t.TempDir()
	if err := os.WriteFile(filepath.Join(repo, binaryName), []byte("#!/bin/sh\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	legacy := "${CLAUDE_PROJECT_DIR}/swarmia-cli log-hook"
	writeSettings(t, repo, map[string]any{"hooks": map[string]any{
		"Stop":       []any{grp("", legacy)},
		"PreToolUse": []any{grp("*", legacy), grp("Bash", "${CLAUDE_PROJECT_DIR}/other-tool")},
	}})

	var out bytes.Buffer
	if err := runInstall(&out, repo, installOpts{}); err != nil {
		t.Fatal(err)
	}
	want := claudeCodeAgent.hookCommand(binaryRef{rel: binaryName}, agent.ScopeProject)
	doc := readSettings(t, repo)
	for _, ev := range []string{"Stop", "PreToolUse", "SessionStart"} {
		if got := commandsFor(t, doc, ev); !contains(got, want) || contains(got, legacy) {
			t.Errorf("%s = %v, want %q in place of the legacy command", ev, got, want)
		}
	}
	if got := commandsFor(t, doc, "PreToolUse"); !contains(got, "${CLAUDE_PROJECT_DIR}/other-tool") {
		t.Errorf("PreToolUse lost an unrelated hook: %v", got)
	}
	if !strings.Contains(out.String(), "updated 2 hook(s)") {
		t.Errorf("output doesn't report the updates:\n%s", out.String())
	}
}
