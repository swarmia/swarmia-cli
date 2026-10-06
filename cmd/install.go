package cmd

import (
	"encoding/json"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"

	"github.com/spf13/cobra"
)

const binaryName = "swarmia-cli"

var (
	installDir    string
	installBinary string
	installDryRun bool
)

var installCmd = &cobra.Command{
	Use:   "install",
	Short: "Install Swarmia hooks into supported coding agents found in this repo",
	Long: "install detects which supported coding agents are configured in this repository\n" +
		"(currently Claude Code, via a .claude directory) and adds the Swarmia hooks to\n" +
		"each one's settings — the same log-hook wiring this repo uses.\n\n" +
		"It is idempotent: hooks already present are left alone, and unrelated settings and\n" +
		"hooks are preserved. Hooks for subcommands that no longer exist (process-logs) are\n" +
		"removed. The command used to invoke swarmia-cli in the hooks is\n" +
		"auto-detected: a swarmia-cli binary at the repo root is preferred (handy for local\n" +
		"development), then an npm dependency in node_modules/.bin, then a swarmia-cli on\n" +
		"your $PATH. Override with --binary.",
	Args: cobra.NoArgs,
	RunE: func(cmd *cobra.Command, args []string) error {
		dir := installDir
		if dir == "" {
			dir = projectDir()
		}
		return runInstall(cmd.OutOrStdout(), dir, installOpts{binaryRef: installBinary, dryRun: installDryRun})
	},
}

func init() {
	installCmd.Flags().StringVar(&installDir, "dir", "", "repository directory to install into (default: $CLAUDE_PROJECT_DIR or cwd)")
	installCmd.Flags().StringVar(&installBinary, "binary", "", "override the auto-detected command used to invoke swarmia-cli in hooks")
	installCmd.Flags().BoolVar(&installDryRun, "dry-run", false, "report what would change without writing anything")
	rootCmd.AddCommand(installCmd)
}

type installOpts struct {
	binaryRef string
	dryRun    bool
}

// agentConfig describes one supported coding agent: how to tell it is used in a
// repo and how to add our hooks to its settings. New agents (Codex, Cursor, ...)
// plug in by appending to supportedAgents.
type agentConfig struct {
	name    string
	detect  func(repoDir string) bool
	install func(repoDir, binRef string, dryRun bool) (res hookChanges, path string, err error)
}

// hookChanges counts what an install did (or, in a dry run, would do) to one
// agent's settings.
type hookChanges struct {
	added   int
	removed int
}

var supportedAgents = []agentConfig{claudeCodeAgent}

func runInstall(out io.Writer, repoDir string, opts installOpts) error {
	binRef, source := opts.binaryRef, "--binary override"
	if binRef == "" {
		binRef, source = detectBinaryRef(repoDir)
	}
	fmt.Fprintf(out, "Hook command: %s (%s)\n", binRef, source)

	detected := 0
	for _, ag := range supportedAgents {
		if !ag.detect(repoDir) {
			fmt.Fprintf(out, "%s: not detected, skipping\n", ag.name)
			continue
		}
		detected++
		res, path, err := ag.install(repoDir, binRef, opts.dryRun)
		if err != nil {
			return fmt.Errorf("%s: %w", ag.name, err)
		}
		rel := relPath(repoDir, path)
		if res.added == 0 && res.removed == 0 {
			fmt.Fprintf(out, "%s (%s): already configured\n", ag.name, rel)
			continue
		}
		verb := "added"
		if opts.dryRun {
			verb = "would add"
		}
		msg := fmt.Sprintf("%s %d hook(s)", verb, res.added)
		if res.removed > 0 {
			msg += fmt.Sprintf(", removed %d obsolete hook(s)", res.removed)
		}
		fmt.Fprintf(out, "%s (%s): %s\n", ag.name, rel, msg)
	}

	if detected == 0 {
		fmt.Fprintln(out, "No supported coding agents detected in this repo. Nothing to do.")
	} else if !opts.dryRun {
		fmt.Fprintln(out, "Reload hooks with /hooks in Claude Code (or start a fresh session) to activate.")
	}
	return nil
}

// detectBinaryRef decides how hooks should invoke swarmia-cli, in order of
// preference: a binary built at the repo root (local development), then an npm
// dependency in node_modules/.bin (the @swarmia/cli package), then a binary on
// $PATH (global install).
func detectBinaryRef(repoDir string) (ref, source string) {
	if isExecutable(filepath.Join(repoDir, binaryName)) {
		return "${CLAUDE_PROJECT_DIR}/" + binaryName, "local binary at repo root"
	}
	if isExecutable(filepath.Join(repoDir, "node_modules", ".bin", binaryName)) {
		return "${CLAUDE_PROJECT_DIR}/node_modules/.bin/" + binaryName, "npm dependency (node_modules/.bin)"
	}
	if _, err := exec.LookPath(binaryName); err == nil {
		return binaryName, "found on $PATH"
	}
	return binaryName, "not found; assuming it will be on $PATH"
}

// --- Claude Code -----------------------------------------------------------

var claudeCodeAgent = agentConfig{
	name: "Claude Code",
	detect: func(repoDir string) bool {
		return dirExists(filepath.Join(repoDir, ".claude"))
	},
	install: func(repoDir, binRef string, dryRun bool) (hookChanges, string, error) {
		path := filepath.Join(repoDir, ".claude", "settings.json")
		doc, err := readJSONObject(path)
		if err != nil {
			return hookChanges{}, path, err
		}
		res := hookChanges{
			removed: removeObsoleteHooks(doc),
			added:   ensureClaudeHooks(doc, binRef),
		}
		if (res.added > 0 || res.removed > 0) && !dryRun {
			if err := writeJSONObject(path, doc); err != nil {
				return hookChanges{}, path, err
			}
		}
		return res, path, nil
	},
}

// ensureClaudeHooks adds the Swarmia hook set to a parsed settings document,
// returning how many entries were newly added. It mirrors the config this repo
// ships: log-hook on prompts, all tool use, and session lifecycle.
func ensureClaudeHooks(doc map[string]any, binRef string) int {
	logHook := binRef + " log-hook"

	specs := []struct {
		event, matcher, command, subcmd string
	}{
		{"UserPromptSubmit", "", logHook, "log-hook"},
		{"PreToolUse", "*", logHook, "log-hook"},
		{"PostToolUse", "*", logHook, "log-hook"},
		{"SessionStart", "", logHook, "log-hook"},
		{"SessionEnd", "", logHook, "log-hook"},
		{"Stop", "", logHook, "log-hook"},
	}

	added := 0
	for _, s := range specs {
		if ensureHook(doc, s.event, s.matcher, s.command, s.subcmd) {
			added++
		}
	}
	return added
}

// ensureHook makes sure hooks[event] contains a command hook running our
// <subcmd>, under a group with the given matcher. It is a no-op (returns false)
// when such a hook already exists, matching any swarmia-cli command regardless of
// how the binary is referenced so re-running with a different --binary won't
// duplicate. Existing unrelated groups and hooks are preserved.
func ensureHook(doc map[string]any, event, matcher, command, subcmd string) bool {
	hooks := childMap(doc, "hooks")
	arr, _ := hooks[event].([]any)

	for _, g := range arr {
		gm, ok := g.(map[string]any)
		if !ok || !matcherEquals(gm["matcher"], matcher) {
			continue
		}
		if groupHasSwarmia(gm, subcmd) {
			return false // already installed
		}
	}

	newHook := map[string]any{"type": "command", "command": command}

	// Prefer appending to an existing group with the same matcher.
	for _, g := range arr {
		gm, ok := g.(map[string]any)
		if !ok || !matcherEquals(gm["matcher"], matcher) {
			continue
		}
		gm["hooks"] = append(asSlice(gm, "hooks"), newHook)
		hooks[event] = arr
		return true
	}

	// Otherwise start a new group for this matcher.
	group := map[string]any{"hooks": []any{newHook}}
	if matcher != "" {
		group["matcher"] = matcher
	}
	hooks[event] = append(arr, group)
	return true
}

// obsoleteSubcommands are swarmia-cli subcommands that earlier versions wired
// into hooks but that no longer exist; install strips them so agents don't keep
// invoking an unknown command.
var obsoleteSubcommands = []string{"process-logs"}

// removeObsoleteHooks deletes swarmia-cli hooks for obsoleteSubcommands from
// every event, dropping groups and events left empty. Returns how many hooks it
// removed.
func removeObsoleteHooks(doc map[string]any) int {
	hooks, ok := doc["hooks"].(map[string]any)
	if !ok {
		return 0
	}
	removed := 0
	for event, v := range hooks {
		arr, ok := v.([]any)
		if !ok {
			continue
		}
		before := removed
		var keptGroups []any
		for _, g := range arr {
			gm, ok := g.(map[string]any)
			if !ok {
				keptGroups = append(keptGroups, g)
				continue
			}
			var kept []any
			for _, h := range asSlice(gm, "hooks") {
				if isObsoleteHook(h) {
					removed++
					continue
				}
				kept = append(kept, h)
			}
			if len(kept) == 0 && len(asSlice(gm, "hooks")) > 0 {
				continue // the group held only obsolete hooks
			}
			if kept != nil {
				gm["hooks"] = kept
			}
			keptGroups = append(keptGroups, gm)
		}
		if removed == before {
			continue // nothing obsolete here; leave the event exactly as it was
		}
		if len(keptGroups) == 0 {
			delete(hooks, event)
		} else {
			hooks[event] = keptGroups
		}
	}
	return removed
}

func isObsoleteHook(h any) bool {
	hm, ok := h.(map[string]any)
	if !ok {
		return false
	}
	cmd, _ := hm["command"].(string)
	if !strings.Contains(cmd, binaryName) {
		return false
	}
	for _, sub := range obsoleteSubcommands {
		if strings.Contains(cmd, sub) {
			return true
		}
	}
	return false
}

func groupHasSwarmia(group map[string]any, subcmd string) bool {
	for _, h := range asSlice(group, "hooks") {
		hm, ok := h.(map[string]any)
		if !ok {
			continue
		}
		cmd, _ := hm["command"].(string)
		if strings.Contains(cmd, binaryName) && strings.Contains(cmd, subcmd) {
			return true
		}
	}
	return false
}

// matcherEquals treats a missing matcher key and an empty string as the same slot.
func matcherEquals(v any, matcher string) bool {
	s, _ := v.(string)
	return s == matcher
}

// --- small JSON / fs helpers ----------------------------------------------

func readJSONObject(path string) (map[string]any, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return map[string]any{}, nil
		}
		return nil, err
	}
	if len(strings.TrimSpace(string(data))) == 0 {
		return map[string]any{}, nil
	}
	var doc map[string]any
	if err := json.Unmarshal(data, &doc); err != nil {
		return nil, fmt.Errorf("parsing %s: %w", path, err)
	}
	if doc == nil {
		doc = map[string]any{}
	}
	return doc, nil
}

func writeJSONObject(path string, doc map[string]any) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	data, err := json.MarshalIndent(doc, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(path, append(data, '\n'), 0o644)
}

func childMap(parent map[string]any, key string) map[string]any {
	if m, ok := parent[key].(map[string]any); ok {
		return m
	}
	m := map[string]any{}
	parent[key] = m
	return m
}

func asSlice(m map[string]any, key string) []any {
	s, _ := m[key].([]any)
	return s
}

func isExecutable(path string) bool {
	fi, err := os.Stat(path)
	if err != nil || fi.IsDir() {
		return false
	}
	return fi.Mode()&0o111 != 0
}

func dirExists(path string) bool {
	fi, err := os.Stat(path)
	return err == nil && fi.IsDir()
}

func relPath(base, path string) string {
	if rel, err := filepath.Rel(base, path); err == nil {
		return rel
	}
	return path
}
