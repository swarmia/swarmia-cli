//go:build e2e

// Package e2e runs a real coding agent against a real repository with
// swarmia-cli's hooks installed, and checks what swarmia-cli captured,
// attributed and would send. See docs/e2e.md. Run with `make e2e`.
package e2e

import (
	"bytes"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

const (
	fixtureURL  = "https://github.com/unjs/ufo.git"
	fixtureSHA  = "f06c800d0c59f2a4a1b9ba65eb6cb61a84419be6" // 2026-04-29
	fixturePNPM = "pnpm@10.33.2"                             // the fixture's packageManager

	devName  = "E2E Developer"
	devEmail = "dev@e2e.example.com"
)

func envOr(key, def string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return def
}

// env is one isolated end-to-end run: its own HOME, agent config, git config
// and swarmia-cli state, so nothing touches the developer's own.
type env struct {
	t         *testing.T
	root      string // everything lives under here
	home      string
	repo      string // the developer's clone of the fixture
	workspace string // where the agent runs and its hooks are installed: repo, or a directory holding it
	remotes   string // bare repositories standing in for GitHub (see addRemote)
	team      string // a teammate's clone
	swarmia   string // swarmia-cli binary
	agent     agent
	agentBin  string // the agent's binary
	version   string // the agent's version actually installed
	artifacts string // where to keep logs (empty: don't)
	auth      auth
	base      []string // environment for every command
	secrets   []string // credential values that must never reach the artifacts
	leaked    bool     // a secret was found (and scrubbed) in an artifact
	// settingSources is Claude Code's --setting-sources: "project" (only the
	// workspace's hooks), or "user,project" to also run user-scope ones.
	settingSources string
}

func newEnv(t *testing.T, ag agent) *env {
	t.Helper()
	root := t.TempDir()
	if r, err := filepath.EvalSymlinks(root); err == nil {
		root = r
	}
	e := &env{t: t, root: root, home: filepath.Join(root, "home"), repo: filepath.Join(root, "repo"),
		remotes: filepath.Join(root, "remotes"), team: filepath.Join(root, "team"),
		agent: ag, artifacts: os.Getenv("E2E_ARTIFACTS"), auth: ag.detectAuth(), settingSources: "project"}
	e.workspace = e.repo
	for _, d := range []string{e.home, filepath.Join(root, "claude-config"), filepath.Join(root, "codex-home"), filepath.Join(root, "state"), filepath.Join(root, "bin")} {
		must(t, os.MkdirAll(d, 0o755))
	}
	if e.artifacts != "" {
		must(t, os.MkdirAll(e.artifacts, 0o755))
	}
	for _, k := range secretVars {
		e.addSecret(os.Getenv(k))
	}
	t.Cleanup(func() {
		if e.leaked {
			t.Error("a credential reached the artifacts (scrubbed before upload); find out how")
		}
	})
	sshStandIn := filepath.Join(root, "bin", "e2e-ssh")
	must(t, os.WriteFile(sshStandIn, []byte(sshStandInScript), 0o755))
	gitconfig := filepath.Join(root, "gitconfig")
	must(t, os.WriteFile(gitconfig, []byte(fmt.Sprintf(
		"[user]\n\tname = %s\n\temail = %s\n[init]\n\tdefaultBranch = main\n[advice]\n\tdetachedHead = false\n", devName, devEmail)), 0o644))

	// Build the environment from scratch: only what the tools need, plus the
	// chosen credentials. Nothing else from the caller's environment (their
	// Claude Code session, their swarmia-cli settings) leaks in.
	e.base = []string{
		"PATH=" + filepath.Join(root, "bin") + string(os.PathListSeparator) + os.Getenv("PATH"),
		"HOME=" + e.home,
		"TMPDIR=" + os.TempDir(),
		"LANG=en_US.UTF-8",
		"CLAUDE_CONFIG_DIR=" + filepath.Join(root, "claude-config"),
		"CODEX_HOME=" + filepath.Join(root, "codex-home"),
		"XDG_CONFIG_HOME=" + filepath.Join(root, "xdg-config"),
		"SWARMIA_STATE_DIR=" + filepath.Join(root, "state"),
		"GIT_CONFIG_GLOBAL=" + gitconfig,
		"GIT_CONFIG_NOSYSTEM=1",
		"GIT_SSH_COMMAND=" + sshStandIn,
		"GIT_SSH_VARIANT=simple",
		"E2E_REMOTES=" + e.remotes,
		"DISABLE_AUTOUPDATER=1",
		"CI=1",
	}
	return e
}

func must(t *testing.T, err error) {
	t.Helper()
	if err != nil {
		t.Fatal(err)
	}
}

// run runs a command in dir with the isolated environment (plus extra
// variables), failing the test on error. Output is returned and, when
// artifacts are kept, appended to commands.log.
func (e *env) run(dir string, extra []string, name string, args ...string) string {
	e.t.Helper()
	out, err := e.try(dir, extra, name, args...)
	if err != nil {
		e.t.Fatalf("%s %s: %v\n%s", name, strings.Join(args, " "), err, out)
	}
	return out
}

func (e *env) try(dir string, extra []string, name string, args ...string) (string, error) {
	cmd := exec.Command(name, args...)
	cmd.Dir = dir
	cmd.Env = append(append([]string{}, e.base...), extra...)
	out, err := cmd.CombinedOutput()
	e.logCommand(dir, name, args, out, err)
	return string(out), err
}

func (e *env) logCommand(dir, name string, args []string, out []byte, err error) {
	e.writeArtifact("commands.log", []byte(fmt.Sprintf("$ (cd %s && %s %s)  # err=%v\n%s\n",
		dir, name, strings.Join(args, " "), err, truncate(string(out), 4000))), true)
}

func truncate(s string, n int) string {
	if len(s) > n {
		return s[:n] + "\n…"
	}
	return s
}

// keep copies a file into the artifacts directory, if one is set.
func (e *env) keep(src, name string) {
	if e.artifacts == "" {
		return
	}
	if data, err := os.ReadFile(src); err == nil {
		e.writeArtifact(name, data, false)
	}
}

func (e *env) keepBytes(name string, data []byte) {
	e.writeArtifact(name, data, false)
}

// secretVars are the caller's variables holding credentials. Their values
// (and every OIDC token the suite mints) are scrubbed from artifacts: CI
// uploads those publicly, unmasked, and an agent's transcript or output could
// echo its environment.
var secretVars = []string{"ANTHROPIC_API_KEY", "CLAUDE_CODE_OAUTH_TOKEN", "OPENAI_API_KEY", "CODEX_API_KEY",
	"ACTIONS_ID_TOKEN_REQUEST_TOKEN"}

func (e *env) addSecret(v string) {
	if len(v) >= 8 { // shorter is no credential, and would scrub innocent text
		e.secrets = append(e.secrets, v)
	}
}

// scrub replaces every known secret in data, and reports whether it found one.
func (e *env) scrub(data []byte) ([]byte, bool) {
	found := false
	for _, s := range e.secrets {
		if bytes.Contains(data, []byte(s)) {
			data, found = bytes.ReplaceAll(data, []byte(s), []byte("[E2E-SCRUBBED]")), true
		}
	}
	return data, found
}

// writeArtifact is the one way into the artifacts directory: it scrubs
// secrets at write time, so even a run killed by a timeout (which skips
// cleanups) leaves nothing to leak.
func (e *env) writeArtifact(name string, data []byte, appendTo bool) {
	if e.artifacts == "" {
		return
	}
	data, found := e.scrub(data)
	if found {
		e.leaked = true
	}
	flags := os.O_CREATE | os.O_WRONLY | os.O_TRUNC
	if appendTo {
		flags = os.O_CREATE | os.O_WRONLY | os.O_APPEND
	}
	f, err := os.OpenFile(filepath.Join(e.artifacts, name), flags, 0o644)
	if err != nil {
		return
	}
	defer f.Close()
	f.Write(data)
}

// --- setup ---------------------------------------------------------------------

// buildSwarmia compiles swarmia-cli from this working tree. It uses the
// caller's own Go environment (module and build caches), not the sandbox:
// compiling our code needs none of the isolation, and Go's read-only module
// cache inside the sandbox would break the temp directory's cleanup.
func (e *env) buildSwarmia() {
	e.t.Helper()
	moduleRoot, err := filepath.Abs("..")
	must(e.t, err)
	e.swarmia = filepath.Join(e.root, "bin", "swarmia-cli")
	cmd := exec.Command("go", "build", "-ldflags", "-X github.com/swarmia/swarmia-cli/cmd.version=e2e", "-o", e.swarmia, ".")
	cmd.Dir = moduleRoot
	if out, err := cmd.CombinedOutput(); err != nil {
		e.t.Fatalf("building swarmia-cli: %v\n%s", err, out)
	}
}

// sshStandInScript stands in for ssh to e2e.example, so remotes look like
// real hosted repositories (ssh://git@e2e.example/acme/app.git, which gives
// the repository a slug in payloads) while living on disk: it runs the git
// command it's given against the bare repositories under $E2E_REMOTES. With
// GIT_SSH_VARIANT=simple, git calls it as: <host> <command>.
const sshStandInScript = `#!/bin/sh
case "$2" in
  "git-upload-pack '/"*|"git-receive-pack '/"*|"git-upload-archive '/"*) ;;
  *) echo "e2e-ssh: unexpected command: $2" >&2; exit 1 ;;
esac
exec sh -c "$(printf '%s' "$2" | sed "s|'/|'$E2E_REMOTES/|")"
`

// remoteURL is the URL of the stand-in hosted repository acme/<name>.
func remoteURL(name string) string { return "ssh://git@e2e.example/acme/" + name + ".git" }

// addRemote publishes the repository in dir as acme/<name> and makes that
// its origin, tracking main.
func (e *env) addRemote(dir, name string) {
	e.t.Helper()
	bare := filepath.Join(e.remotes, "acme", name+".git")
	must(e.t, os.MkdirAll(filepath.Dir(bare), 0o755))
	e.run(e.root, nil, "git", "clone", "-q", "--bare", dir, bare)
	e.run(dir, nil, "git", "remote", "add", "origin", remoteURL(name))
	e.run(dir, nil, "git", "fetch", "-q", "origin")
	e.run(dir, nil, "git", "branch", "-q", "--set-upstream-to=origin/main", "main")
}

// setupFixture fetches the fixture at its pinned commit, publishes it as
// acme/ufo with a teammate's clone, installs dependencies and our hooks.
func (e *env) setupFixture() {
	e.t.Helper()
	must(e.t, os.MkdirAll(e.repo, 0o755))
	e.run(e.repo, nil, "git", "init", "-q")
	e.run(e.repo, nil, "git", "remote", "add", "upstream", fixtureURL)
	e.run(e.repo, nil, "git", "fetch", "-q", "--depth", "1", "upstream", fixtureSHA)
	e.run(e.repo, nil, "git", "checkout", "-q", "-b", "main", "FETCH_HEAD")
	e.addRemote(e.repo, "ufo")
	e.run(e.root, nil, "git", "clone", "-q", remoteURL("ufo"), e.team)

	e.run(e.repo, nil, "npx", "-y", fixturePNPM, "install", "--frozen-lockfile", "--reporter=silent")
}

// installHooks installs our hooks into the workspace's settings for the
// agent (.claude/, .codex/), excluded from git if the workspace is a
// repository so they're never committed.
func (e *env) installHooks() {
	e.t.Helper()
	dir := e.agent.projectDir()
	must(e.t, os.MkdirAll(filepath.Join(e.workspace, dir), 0o755))
	if _, err := os.Stat(filepath.Join(e.workspace, ".git")); err == nil {
		exclude := filepath.Join(e.workspace, ".git", "info", "exclude")
		f, err := os.OpenFile(exclude, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o644)
		must(e.t, err)
		f.WriteString("\n" + dir + "/\n")
		f.Close()
	}
	args := append([]string{"install", "--scope", "project", "--dir", e.workspace, "--binary", e.swarmia}, e.agent.installFlags()...)
	out := e.run(e.workspace, nil, e.swarmia, args...)
	e.t.Logf("swarmia-cli install:\n%s", out)
	e.agent.prepare(e)
}

func init() {
	if runtime.GOOS == "windows" {
		panic("the e2e suite runs on macOS and Linux")
	}
}
