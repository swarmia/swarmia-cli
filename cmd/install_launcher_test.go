package cmd

import (
	"bytes"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/swarmia/swarmia-cli/internal/agent"
	"github.com/swarmia/swarmia-cli/internal/launcher"
)

// serveSums serves SHA256SUMS for versions, as their GitHub releases do, and
// points launcher downloads at it.
func serveSums(t *testing.T, versions ...string) {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		for _, v := range versions {
			if r.URL.Path == "/v"+v+"/SHA256SUMS" {
				for _, p := range launcher.Platforms {
					w.Write([]byte(strings.Repeat("a", 64) + "  " + launcher.TarballName(v, p) + "\n"))
				}
				return
			}
		}
		http.NotFound(w, r)
	}))
	t.Cleanup(srv.Close)
	t.Setenv(launcher.DownloadURLEnv, srv.URL)
}

func launcherRepo(t *testing.T) string {
	t.Helper()
	repo := initRepo(t)
	for _, dir := range []string{".claude", ".codex"} {
		if err := os.Mkdir(filepath.Join(repo, dir), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	t.Setenv("CODEX_HOME", t.TempDir())
	return repo
}

// --launcher writes a launcher for the version, and points project hooks at
// it; rerun, it leaves it alone, and with another version upgrades it.
func TestInstallLauncher(t *testing.T) {
	serveSums(t, "1.2.3", "1.3.0")
	repo := launcherRepo(t)
	path := filepath.Join(repo, filepath.FromSlash(launcher.Path))

	var out bytes.Buffer
	if err := runInstall(&out, repo, installOpts{dryRun: true, launcher: true, launcherVersion: "v1.2.3"}); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(path); err == nil {
		t.Fatal("a dry run wrote the launcher")
	}
	if !strings.Contains(out.String(), "would write it, for swarmia-cli 1.2.3") {
		t.Errorf("dry run output:\n%s", out.String())
	}

	out.Reset()
	if err := runInstall(&out, repo, installOpts{launcher: true, launcherVersion: "v1.2.3"}); err != nil {
		t.Fatal(err)
	}
	script, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if got := launcher.PinnedVersion(script); got != "1.2.3" {
		t.Errorf("launcher runs %q, want 1.2.3", got)
	}
	if fi, err := os.Stat(path); err != nil || fi.Mode()&0o111 == 0 {
		t.Errorf("launcher isn't executable: %v", err)
	}
	if !strings.Contains(out.String(), "wrote it, for swarmia-cli 1.2.3; commit it") {
		t.Errorf("output:\n%s", out.String())
	}
	want := `[ ! -x "${CLAUDE_PROJECT_DIR}/.swarmia/swarmia-cli" ] || exec "${CLAUDE_PROJECT_DIR}/.swarmia/swarmia-cli" log-hook`
	if got := commandsFor(t, readSettings(t, repo), "SessionStart"); !contains(got, want) {
		t.Errorf("Claude Code hook = %v, want %s", got, want)
	}
	codexHooks, err := os.ReadFile(filepath.Join(repo, ".codex", "hooks.json"))
	if err != nil || !strings.Contains(string(codexHooks), `.swarmia/swarmia-cli\" log-hook --agent codex`) {
		t.Errorf("Codex hooks don't run the launcher (%v):\n%s", err, codexHooks)
	}

	out.Reset()
	if err := runInstall(&out, repo, installOpts{launcher: true, launcherVersion: "1.2.3"}); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out.String(), "already runs swarmia-cli 1.2.3") {
		t.Errorf("rerun output:\n%s", out.String())
	}

	out.Reset()
	if err := runInstall(&out, repo, installOpts{launcher: true, launcherVersion: "1.3.0"}); err != nil {
		t.Fatal(err)
	}
	if script, _ := os.ReadFile(path); launcher.PinnedVersion(script) != "1.3.0" {
		t.Errorf("launcher not upgraded:\n%s", script)
	}
	if !strings.Contains(out.String(), "switched it from swarmia-cli 1.2.3 to 1.3.0") {
		t.Errorf("upgrade output:\n%s", out.String())
	}

	// Without --launcher, project hooks find the launcher.
	if ref := detectBinaryRef(repo); ref.rel != launcher.Path {
		t.Errorf("detectBinaryRef = %+v, want the launcher", ref)
	}
}

func TestInstallLauncherErrors(t *testing.T) {
	serveSums(t, "1.2.3")
	repo := launcherRepo(t)
	for _, tc := range []struct {
		name string
		opts installOpts
		want string
	}{
		{"unreleased version", installOpts{launcher: true, launcherVersion: "9.9.9"}, "404"},
		{"development build", installOpts{launcher: true}, "development build"},
		{"local scope", installOpts{launcher: true, launcherVersion: "1.2.3", scope: agent.ScopeLocal}, "project hooks"},
		{"with --binary", installOpts{launcher: true, launcherVersion: "1.2.3", binaryRef: "x"}, "pick one"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			err := runInstall(&bytes.Buffer{}, repo, tc.opts)
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Errorf("err = %v, want one mentioning %q", err, tc.want)
			}
		})
	}
	if _, err := os.Stat(filepath.Join(repo, filepath.FromSlash(launcher.Path))); err == nil {
		t.Error("a failed install wrote the launcher")
	}
}

// Teammates only get the launcher if git doesn't ignore it.
func TestInstallLauncherWarnsWhenIgnored(t *testing.T) {
	serveSums(t, "1.2.3")
	repo := launcherRepo(t)
	if err := os.WriteFile(filepath.Join(repo, ".gitignore"), []byte(".swarmia/\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	var out bytes.Buffer
	if err := runInstall(&out, repo, installOpts{launcher: true, launcherVersion: "1.2.3"}); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out.String(), "Warning: git ignores .swarmia/swarmia-cli") {
		t.Errorf("no warning:\n%s", out.String())
	}
}

// Run from the repository's launcher, personal installs point at the
// launcher, not the version in its cache, so upgrading it upgrades them.
func TestRefForExecutableFromLauncher(t *testing.T) {
	cache := t.TempDir()
	t.Setenv("XDG_CACHE_HOME", cache)
	t.Setenv("PATH", "")
	repo, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	real, err := filepath.EvalSymlinks(cache)
	if err != nil {
		t.Fatal(err)
	}
	exe := filepath.Join(real, "swarmia", "cli", "1.2.3", binaryName)
	if err := os.MkdirAll(filepath.Dir(exe), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(exe, []byte("#!/bin/sh\n"), 0o755); err != nil {
		t.Fatal(err)
	}

	if got := refForExecutable(exe, repo, agent.ScopeLocal); got.cmd != exe {
		t.Errorf("without a launcher: %+v, want the binary", got)
	}
	path := filepath.Join(repo, filepath.FromSlash(launcher.Path))
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte("#!/bin/sh\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	if got := refForExecutable(exe, repo, agent.ScopeLocal); got.rel != launcher.Path {
		t.Errorf("local scope: %+v, want the launcher relative to the repository", got)
	}
	if got := refForExecutable(exe, repo, agent.ScopeUser); got.cmd != path {
		t.Errorf("user scope: %+v, want the launcher's absolute path", got)
	}
}
