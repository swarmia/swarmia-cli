package cmd

import (
	"bytes"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/swarmia/swarmia-cli/internal/codex"
)

// codexRepo is a repository with a .codex directory and swarmia-cli as an npm
// dependency (a stub that prints its arguments and working directory).
func codexRepo(t *testing.T) string {
	t.Helper()
	repo := t.TempDir()
	bin := filepath.Join(repo, "node_modules", "@swarmia", "cli", "bin")
	for _, d := range []string{filepath.Join(repo, ".codex"), bin, filepath.Join(repo, "src", "deep")} {
		if err := os.MkdirAll(d, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	stub := "#!/bin/sh\necho \"$*\"\n"
	if err := os.WriteFile(filepath.Join(bin, binaryName), []byte(stub), 0o755); err != nil {
		t.Fatal(err)
	}
	return repo
}

func codexHooks(t *testing.T, path string) map[string]any {
	t.Helper()
	doc, err := readJSONObject(path)
	if err != nil {
		t.Fatal(err)
	}
	return doc
}

func TestInstallCodexProject(t *testing.T) {
	repo := codexRepo(t)
	var out bytes.Buffer
	if err := runInstall(&out, repo, installOpts{}); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(repo, ".codex", "hooks.json")
	doc := codexHooks(t, path)
	command := codexRootedCommand("node_modules/@swarmia/cli/bin/swarmia-cli", "log-hook --agent codex")
	for _, s := range codexHookSpecs {
		if got := commandsFor(t, doc, s.event); !reflect.DeepEqual(got, []string{command}) {
			t.Errorf("%s = %v", s.event, got)
		}
	}
	handlers, err := codex.ReadHandlers(path)
	if err != nil || len(handlers) != len(codexHookSpecs) {
		t.Fatalf("handlers = %v, %v", handlers, err)
	}
	for _, h := range handlers {
		if want := map[bool]uint64{true: 3, false: 30}[h.Event == "SessionEnd"]; h.Timeout != want || h.Matcher != nil {
			t.Errorf("%s: timeout %d, matcher %v", h.Event, h.Timeout, h.Matcher)
		}
	}
	if !strings.Contains(out.String(), "/hooks") || !strings.Contains(out.String(), "--trust") {
		t.Errorf("no trust instructions:\n%s", out.String())
	}
	if _, err := os.Stat(filepath.Join(os.Getenv("CODEX_HOME"), "config.toml")); err == nil {
		t.Error("wrote config.toml without --trust")
	}

	// The hook finds the launcher from anywhere in the repository, and does
	// nothing (successfully) where there is none.
	for dir, want := range map[string]string{
		repo:                               "log-hook --agent codex\n",
		filepath.Join(repo, "src", "deep"): "log-hook --agent codex\n",
		t.TempDir():                        "",
	} {
		cmd := exec.Command("sh", "-c", command)
		cmd.Dir = dir
		cmd.Env = append(os.Environ(), "PWD="+dir)
		got, err := cmd.CombinedOutput()
		if err != nil || string(got) != want {
			t.Errorf("in %s: %q, %v; want %q", dir, got, err, want)
		}
	}
}

func TestInstallCodexTrust(t *testing.T) {
	repo := codexRepo(t)
	var out bytes.Buffer
	if err := runInstall(&out, repo, installOpts{trust: true}); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out.String(), "Trusted 6 hook(s)") {
		t.Errorf("output:\n%s", out.String())
	}
	path := filepath.Join(repo, ".codex", "hooks.json")
	if left, err := untrustedCodexHooks(path); err != nil || len(left) != 0 {
		t.Errorf("untrusted after --trust: %v, %v", left, err)
	}

	// Reinstalling reports the trust; a changed command needs trusting again.
	out.Reset()
	if err := runInstall(&out, repo, installOpts{}); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out.String(), "Trusted in") {
		t.Errorf("output:\n%s", out.String())
	}
	data, _ := os.ReadFile(path)
	os.WriteFile(path, bytes.ReplaceAll(data, []byte("--agent codex"), []byte("--agent codex ")), 0o644)
	if left, _ := untrustedCodexHooks(path); len(left) != 6 {
		t.Errorf("after changing the commands, %d untrusted; want 6", len(left))
	}
}

func TestInstallCodexScopes(t *testing.T) {
	repo := codexRepo(t)
	var out bytes.Buffer
	if err := runInstall(&out, repo, installOpts{binaryRef: "/opt/bin/swarmia-cli", scope: scopeLocal}); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out.String(), "Codex: no local scope") {
		t.Errorf("output:\n%s", out.String())
	}

	// User scope: only when Codex's home exists.
	t.Setenv("CODEX_HOME", filepath.Join(t.TempDir(), "missing"))
	out.Reset()
	if err := runInstall(&out, repo, installOpts{binaryRef: "/opt/bin/swarmia-cli", scope: scopeUser}); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out.String(), "Codex: not detected") {
		t.Errorf("output:\n%s", out.String())
	}
	home := t.TempDir()
	t.Setenv("CODEX_HOME", home)
	if err := runInstall(&out, repo, installOpts{binaryRef: "/opt/bin/swarmia-cli", scope: scopeUser}); err != nil {
		t.Fatal(err)
	}
	doc := codexHooks(t, filepath.Join(home, "hooks.json"))
	want := "/opt/bin/swarmia-cli log-hook --agent codex --skip-if-project-hooks"
	if got := commandsFor(t, doc, "PostToolUse"); !reflect.DeepEqual(got, []string{want}) {
		t.Errorf("PostToolUse = %v", got)
	}

	// User hooks that stand down for project hooks are no duplicates.
	out.Reset()
	if err := runInstall(&out, repo, installOpts{}); err != nil {
		t.Fatal(err)
	}
	if strings.Contains(out.String(), "Warning") {
		t.Errorf("warned about stand-down user hooks:\n%s", out.String())
	}
}
