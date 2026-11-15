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

// copilotRepo is a repository set up for Copilot (.github/hooks), with
// swarmia-cli as an npm dependency (a stub that prints its arguments).
func copilotRepo(t *testing.T) string {
	t.Helper()
	repo := t.TempDir()
	bin := filepath.Join(repo, "node_modules", "@swarmia", "cli", "bin")
	for _, d := range []string{filepath.Join(repo, ".github", "hooks"), bin, filepath.Join(repo, "src", "deep")} {
		if err := os.MkdirAll(d, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.WriteFile(filepath.Join(bin, binaryName), []byte("#!/bin/sh\necho \"$*\"\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	return repo
}

// copilotCommands lists the bash commands of an event's hook entries.
func copilotCommands(t *testing.T, doc map[string]any, event string) []string {
	t.Helper()
	hooks, _ := doc["hooks"].(map[string]any)
	entries, _ := hooks[event].([]any)
	var out []string
	for _, e := range entries {
		em, _ := e.(map[string]any)
		if _, grouped := em["hooks"]; grouped || em["type"] != "command" {
			t.Errorf("%s: not a flat command entry: %v", event, em)
		}
		out = append(out, hookCommand(em))
	}
	return out
}

func TestInstallCopilotProject(t *testing.T) {
	repo := copilotRepo(t)
	var out bytes.Buffer
	if err := runInstall(&out, repo, installOpts{}); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(repo, ".github", "hooks", "swarmia.json")
	doc, err := readJSONObject(path)
	if err != nil {
		t.Fatal(err)
	}
	if v, _ := doc["version"].(json.Number); v.String() != "1" {
		t.Errorf("version = %v", doc["version"])
	}
	command := copilotRootedCommand("node_modules/@swarmia/cli/bin/swarmia-cli", "log-hook --agent copilot")
	for _, s := range copilotHookSpecs {
		if got := copilotCommands(t, doc, s.event); !reflect.DeepEqual(got, []string{command}) {
			t.Errorf("%s = %v", s.event, got)
		}
	}
	if !strings.Contains(out.String(), "GitHub Copilot (.github/hooks/swarmia.json): added 7 hook(s)") {
		t.Errorf("output:\n%s", out.String())
	}

	// Idempotent.
	out.Reset()
	if err := runInstall(&out, repo, installOpts{}); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out.String(), "GitHub Copilot (.github/hooks/swarmia.json): already configured") {
		t.Errorf("second install:\n%s", out.String())
	}

	// The hook runs the launcher from the project directory Copilot passes,
	// or from anywhere in the repository without it, and does nothing
	// (successfully) where there is none: Copilot denies a tool call whose
	// PreToolUse hook fails.
	for _, tc := range []struct {
		dir, projectDir, want string
	}{
		{repo, repo, "log-hook --agent copilot\n"},
		{filepath.Join(repo, "src", "deep"), "", "log-hook --agent copilot\n"},
		{t.TempDir(), "", ""},
	} {
		cmd := exec.Command("sh", "-c", command)
		cmd.Dir = tc.dir
		cmd.Env = append(os.Environ(), "PWD="+tc.dir, "COPILOT_PROJECT_DIR="+tc.projectDir)
		got, err := cmd.CombinedOutput()
		if err != nil || string(got) != tc.want {
			t.Errorf("in %s: %q, %v; want %q", tc.dir, got, err, tc.want)
		}
	}
}

func TestInstallCopilotDetection(t *testing.T) {
	repo := t.TempDir()
	if err := os.MkdirAll(filepath.Join(repo, ".github", "workflows"), 0o755); err != nil {
		t.Fatal(err)
	}
	var out bytes.Buffer
	if err := runInstall(&out, repo, installOpts{}); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out.String(), "GitHub Copilot: not detected") {
		t.Errorf("a .github directory alone is no Copilot setup:\n%s", out.String())
	}
	if err := os.WriteFile(filepath.Join(repo, ".github", "copilot-instructions.md"), []byte("Be brief.\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	out.Reset()
	if err := runInstall(&out, repo, installOpts{dryRun: true}); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out.String(), "GitHub Copilot (.github/hooks/swarmia.json): would add 7 hook(s)") {
		t.Errorf("copilot-instructions.md should show Copilot:\n%s", out.String())
	}
	if _, err := os.Stat(filepath.Join(repo, ".github", "hooks")); err == nil {
		t.Error("a dry run wrote hooks")
	}
}

// Local scope adds a hooks block to the personal settings file, keeping the
// rest, without a version (settings have none); user scope a hooks file of
// its own.
func TestInstallCopilotScopes(t *testing.T) {
	repo := copilotRepo(t)
	local := filepath.Join(repo, ".github", "copilot", "settings.local.json")
	if err := os.MkdirAll(filepath.Dir(local), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(local, []byte("{\n  \"model\": \"claude-sonnet-5.5\"\n}\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	var out bytes.Buffer
	if err := runInstall(&out, repo, installOpts{scope: agent.ScopeLocal, binaryRef: "/opt/bin/swarmia-cli"}); err != nil {
		t.Fatal(err)
	}
	doc, err := readJSONObject(local)
	if err != nil {
		t.Fatal(err)
	}
	if doc["model"] != "claude-sonnet-5.5" || doc["version"] != nil {
		t.Errorf("local settings = %v", doc)
	}
	want := "[ ! -x /opt/bin/swarmia-cli ] || exec /opt/bin/swarmia-cli log-hook --agent copilot"
	if got := copilotCommands(t, doc, "PreToolUse"); !reflect.DeepEqual(got, []string{want}) {
		t.Errorf("PreToolUse = %v", got)
	}
	if !strings.Contains(out.String(), "isn't ignored by git") {
		t.Errorf("no gitignore note:\n%s", out.String())
	}

	out.Reset()
	if err := runInstall(&out, repo, installOpts{scope: agent.ScopeUser, binaryRef: "swarmia-cli"}); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out.String(), "GitHub Copilot: not detected (no ~/.copilot)") {
		t.Errorf("user scope without ~/.copilot:\n%s", out.String())
	}
	home := os.Getenv("COPILOT_HOME")
	if err := os.MkdirAll(home, 0o755); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(home) })
	if err := runInstall(&out, repo, installOpts{scope: agent.ScopeUser, binaryRef: "swarmia-cli"}); err != nil {
		t.Fatal(err)
	}
	doc, err = readJSONObject(filepath.Join(home, "hooks", "swarmia.json"))
	if err != nil {
		t.Fatal(err)
	}
	want = "command -v swarmia-cli >/dev/null 2>&1 || exit 0; exec swarmia-cli log-hook --agent copilot --skip-if-project-hooks"
	if got := copilotCommands(t, doc, "Stop"); !reflect.DeepEqual(got, []string{want}) {
		t.Errorf("Stop = %v", got)
	}
}

// A hook running swarmia-cli from outside the repository does nothing,
// successfully, if it isn't there.
func TestFailSafeCommand(t *testing.T) {
	for cmd, want := range map[string]string{
		"/opt/bin/swarmia-cli":   "[ ! -x /opt/bin/swarmia-cli ] || exec /opt/bin/swarmia-cli log-hook",
		"'/my bin/swarmia-cli'":  "[ ! -x '/my bin/swarmia-cli' ] || exec '/my bin/swarmia-cli' log-hook",
		"swarmia-cli":            "command -v swarmia-cli >/dev/null 2>&1 || exit 0; exec swarmia-cli log-hook",
		"npx -y @swarmia/cli":    "npx -y @swarmia/cli log-hook || true",
		"/nonexistent/swarmia-x": "[ ! -x /nonexistent/swarmia-x ] || exec /nonexistent/swarmia-x log-hook",
	} {
		if got := failSafeCommand(cmd, "log-hook"); got != want {
			t.Errorf("%s: %q, want %q", cmd, got, want)
		}
	}
	for _, cmd := range []string{"/nonexistent/swarmia-x", "swarmia-cli-not-installed"} {
		if out, err := exec.Command("sh", "-c", failSafeCommand(cmd, "log-hook")).CombinedOutput(); err != nil || len(out) > 0 {
			t.Errorf("%s: %q, %v; want nothing, successfully", cmd, out, err)
		}
	}
}
