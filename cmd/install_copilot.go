package cmd

import (
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"

	"github.com/swarmia/swarmia-cli/internal/agent"
)

// copilotHookSpecs is log-hook on GitHub Copilot's session lifecycle, prompts
// and tool use, under Claude Code's event names, so that payloads come
// Claude-shaped. A failed tool call is a PostToolUseFailure.
var copilotHookSpecs = []hookSpec{
	{"SessionStart", "", 30},
	{"UserPromptSubmit", "", 30},
	{"PreToolUse", "", 30},
	{"PostToolUse", "", 30},
	{"PostToolUseFailure", "", 30},
	{"Stop", "", 30},
	{"SessionEnd", "", 30},
}

// copilotMarkers are what show a repository is set up for Copilot. Nearly
// every repository has a .github directory, so that alone doesn't.
var copilotMarkers = []string{".github/hooks", ".github/copilot", ".github/copilot-instructions.md"}

var copilotAgent = agentConfig{
	Agent: agent.Copilot,
	detected: func(repoDir string, scope agent.Scope) (bool, string) {
		if scope == agent.ScopeUser {
			home, err := agent.Copilot.HooksFile("", agent.ScopeUser)
			home = filepath.Dir(filepath.Dir(home))
			return err == nil && dirExists(home), "not detected (no " + displayPath(repoDir, home) + ")"
		}
		for _, m := range copilotMarkers {
			if _, err := os.Stat(filepath.Join(repoDir, filepath.FromSlash(m))); err == nil {
				return true, ""
			}
		}
		return false, "not detected (no " + strings.Join(copilotMarkers, ", ") + "; create .github/hooks to opt in)"
	},
	inRepo:   copilotRootedCommand,
	guard:    failSafeCommand,
	hooks:    copilotHookSpecs,
	install:  installCopilotHooks,
	activate: activateCopilot,
}

// copilotRootedCommand runs a repo-relative swarmia-cli from a Copilot hook.
// Copilot passes the repository root as $COPILOT_PROJECT_DIR (CLI 1.0.103 and
// later) and runs hooks there; without it, the command walks up from where
// the hook runs, as Codex's does. If rel isn't found, the hook does nothing,
// quietly.
//
// Copilot denies a tool call when its PreToolUse hook fails, so the hook must
// not fail where swarmia-cli isn't installed: a teammate's clone without
// node_modules, or Copilot cloud agent's sandbox, which runs a repository's
// hooks too.
func copilotRootedCommand(rel, args string) string {
	return fmt.Sprintf(`d=${COPILOT_PROJECT_DIR:-$PWD}; until [ -x "$d/%[1]s" ] || [ -z "$d" ]; do d=${d%%/*}; done; [ -z "$d" ] || exec "$d/%[1]s" %[2]s`, rel, args)
}

// failSafeCommand runs cmd with args, but does nothing, quietly, if cmd isn't
// installed (see copilotRootedCommand): an absolute path is checked for, a
// name looked up on $PATH. Any other command (a --binary with arguments of its
// own) can't fail the hook.
func failSafeCommand(cmd, args string) string {
	switch {
	case strings.HasPrefix(cmd, "/") || strings.HasPrefix(cmd, "'/"):
		return fmt.Sprintf("[ ! -x %[1]s ] || exec %[1]s %[2]s", cmd, args)
	case !strings.ContainsAny(cmd, " '\"\\$`;&|<>()"):
		return fmt.Sprintf("command -v %[1]s >/dev/null 2>&1 || exit 0; exec %[1]s %[2]s", cmd, args)
	}
	return cmd + " " + args + " || true"
}

// installCopilotHooks adds log-hook, run by command, to a Copilot hooks file
// or settings file for each spec. Copilot lists hooks under each event
// directly, without Claude Code's matcher groups, and runs a command's bash
// field on macOS and Linux. Only bash is set: on Windows, Copilot skips the
// hook, rather than failing every tool call. A hooks file (as opposed to
// settings) needs a version.
func installCopilotHooks(path, command, _ string, specs []hookSpec, dryRun bool) (hookChanges, error) {
	doc, layout, err := readJSONDocument(path)
	if err != nil {
		return hookChanges{}, err
	}
	var res hookChanges
	hooks := childMap(doc, "hooks")
	for _, s := range specs {
		entries, _ := hooks[s.event].([]any)
		if entriesRunLogHook(entries) {
			continue
		}
		entry := map[string]any{"type": "command", "bash": command}
		if s.timeout > 0 {
			entry["timeoutSec"] = s.timeout
		}
		hooks[s.event] = append(entries, entry)
		res.added++
	}
	if _, ok := doc["version"]; !ok && filepath.Base(path) == agent.CopilotHooksFile {
		doc["version"] = 1
	}
	if res.added > 0 && !dryRun {
		order := []string{"version", "hooks", "type", "bash", "timeoutSec"}
		for _, s := range specs {
			order = append(order, s.event)
		}
		if err := writeJSONObject(path, doc, layout, order); err != nil {
			return hookChanges{}, err
		}
	}
	return res, nil
}

// entriesRunLogHook reports whether any of an event's Copilot hook
// entries already runs log-hook, however the binary is referenced.
func entriesRunLogHook(entries []any) bool {
	for _, e := range entries {
		if runsSwarmia(hookCommand(e), "log-hook") {
			return true
		}
	}
	return false
}

// activateCopilot says what's left to do: Copilot runs a repository's hooks
// once its folder is trusted, and the local settings file should stay out of
// git.
func activateCopilot(out io.Writer, path string, opts installOpts) error {
	if opts.dryRun {
		return nil
	}
	switch opts.scope {
	case agent.ScopeLocal:
		dir := filepath.Dir(path)
		if exec.Command("git", "-C", dir, "check-ignore", "-q", path).Run() != nil {
			fmt.Fprintf(out, "  %s isn't ignored by git; add it to .gitignore to keep it to yourself.\n", filepath.Base(path))
		}
		fallthrough
	case agent.ScopeProject, "":
		fmt.Fprintln(out, "  Copilot runs a repository's hooks once you trust its folder; start a new session to activate.")
	default:
		fmt.Fprintln(out, "  Start a new Copilot session to activate.")
	}
	return nil
}
