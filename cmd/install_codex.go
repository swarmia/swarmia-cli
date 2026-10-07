package cmd

import (
	"fmt"
	"io"
	"os"
	"path/filepath"

	"github.com/swarmia/swarmia-cli/internal/agent"
	"github.com/swarmia/swarmia-cli/internal/codex"
)

// codexHookSpecs is log-hook on Codex's session lifecycle, prompts and tool
// use. Codex has no PostToolUseFailure: PostToolUse covers failed calls. Its
// default timeout is ten minutes, too long for a hang to go unnoticed, and it
// caps SessionEnd at 3 seconds (killing the hook's process tree after that).
var codexHookSpecs = []hookSpec{
	{"SessionStart", "", 30},
	{"UserPromptSubmit", "", 30},
	{"PreToolUse", "", 30},
	{"PostToolUse", "", 30},
	{"Stop", "", 30},
	{"SessionEnd", "", 3},
}

var codexAgent = agentConfig{
	Agent: agent.Codex,
	detected: func(repoDir string, scope agent.Scope) (bool, string) {
		if scope == agent.ScopeUser {
			home, err := codex.Home()
			return err == nil && dirExists(home), "not detected (no " + displayPath(repoDir, home) + ")"
		}
		return dirExists(filepath.Join(repoDir, agent.Codex.ConfigDir)), "not detected"
	},
	hookCommand: func(ref binaryRef, scope agent.Scope) string {
		if ref.rel != "" {
			return codexRootedCommand(ref.rel, "log-hook --agent codex")
		}
		command := ref.cmd + " log-hook --agent codex"
		if scope == agent.ScopeUser {
			command += " " + skipIfProjectHooksFlag
		}
		return command
	},
	install: func(path, hookCommand string, dryRun bool) (hookChanges, error) {
		doc, err := readJSONObject(path)
		if err != nil {
			return hookChanges{}, err
		}
		res := hookChanges{added: ensureHooks(doc, hookCommand, codexHookSpecs)}
		if res.added > 0 && !dryRun {
			if err := writeJSONObject(path, doc); err != nil {
				return hookChanges{}, err
			}
		}
		return res, nil
	},
	activate: activateCodex,
}

// codexRootedCommand runs a repo-relative swarmia-cli from a Codex hook. Codex
// runs hooks in the session's working directory, which may be below the
// repository root, and (unlike Claude Code) passes no project directory, so
// the command walks up to the nearest directory holding rel. That costs well
// under a millisecond; `git rev-parse --show-toplevel` would cost ~10 ms on
// every hook. If rel isn't found (say, node_modules isn't installed yet), the
// hook does nothing, quietly.
func codexRootedCommand(rel, args string) string {
	return fmt.Sprintf(`d=$PWD; until [ -x "$d/%[1]s" ] || [ -z "$d" ]; do d=${d%%/*}; done; [ -z "$d" ] || exec "$d/%[1]s" %[2]s`, rel, args)
}

// activateCodex reports whether Codex trusts swarmia-cli's hooks in path, and
// with --trust records that trust in the user's config.toml.
func activateCodex(out io.Writer, path string, opts installOpts) error {
	if opts.dryRun {
		return nil
	}
	untrusted, err := untrustedCodexHooks(path)
	if err != nil {
		return err
	}
	configPath, err := codex.ConfigPath()
	if err != nil {
		return err
	}
	if len(untrusted) == 0 {
		fmt.Fprintf(out, "  Trusted in %s.\n", displayPath("", configPath))
		return nil
	}
	if opts.trust {
		n, err := codex.Trust(configPath, untrusted)
		if err != nil {
			return err
		}
		fmt.Fprintf(out, "  Trusted %d hook(s) in %s.\n", n, displayPath("", configPath))
		return nil
	}
	fmt.Fprintf(out, "  Codex skips hooks until you trust them: run /hooks in Codex and approve the\n"+
		"  %d swarmia-cli hook(s), or rerun with --trust to record that in %s.\n", len(untrusted), displayPath("", configPath))
	if opts.scope == agent.ScopeProject || opts.scope == "" {
		fmt.Fprintln(out, "  Everyone using these project hooks approves them once, the same way.")
	}
	return nil
}

// untrustedCodexHooks returns swarmia-cli's handlers in a Codex hooks file
// that the user's config.toml doesn't trust as they now are: trust key → the
// hash to trust.
func untrustedCodexHooks(path string) (map[string]string, error) {
	handlers, err := codex.ReadHandlers(path)
	if err != nil {
		return nil, err
	}
	configPath, err := codex.ConfigPath()
	if err != nil {
		return nil, err
	}
	trusted, err := codex.TrustedHashes(configPath)
	if err != nil {
		return nil, err
	}
	out := map[string]string{}
	for _, h := range handlers {
		if !runsSwarmia(h.Command, "log-hook") {
			continue
		}
		key, hash := h.Key(path), h.Hash()
		if trusted[key] != hash {
			out[key] = hash
		}
	}
	return out, nil
}

// codexHooksFiles are the Codex hooks files that may hold swarmia-cli's hooks
// for a repository: the project's and the user's.
func codexHooksFiles(repoRoot string) []string {
	var out []string
	for _, scope := range agent.Scopes {
		if repoRoot == "" && scope != agent.ScopeUser {
			continue
		}
		if f, err := agent.Codex.HooksFile(repoRoot, scope); err == nil {
			if _, err := os.Stat(f); err == nil {
				out = append(out, f)
			}
		}
	}
	return out
}
