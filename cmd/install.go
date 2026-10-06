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
	installScopeF string
)

// installScope says which settings file the hooks go into.
type installScope string

const (
	// scopeProject is the repo's shared, committed settings: hooks for everyone.
	scopeProject installScope = "project"
	// scopeLocal is the repo's personal, gitignored settings.
	scopeLocal installScope = "local"
	// scopeUser is the user's settings: every project on this machine, including
	// worktrees that lack the repo's untracked local settings.
	scopeUser installScope = "user"
)

// skipIfProjectHooksFlag makes a user-scope log-hook stand down in projects
// that wire swarmia-cli themselves, so events aren't logged twice.
const skipIfProjectHooksFlag = "--skip-if-project-hooks"

var installCmd = &cobra.Command{
	Use:   "install",
	Short: "Install Swarmia hooks into supported coding agents found in this repo",
	Long: "install detects which supported coding agents are configured in this repository\n" +
		"(currently Claude Code, via a .claude directory) and adds the Swarmia hooks to\n" +
		"each one's settings — the same log-hook wiring this repo uses.\n\n" +
		"It is idempotent: hooks already present are left alone, and unrelated settings and\n" +
		"hooks are preserved. Hooks for subcommands that no longer exist (process-logs) are\n" +
		"removed.\n\n" +
		"--scope picks the settings file:\n" +
		"  project  .claude/settings.json — committed, so hooks for everyone (default)\n" +
		"  local    .claude/settings.local.json — just you, in this checkout\n" +
		"  user     ~/.claude/settings.json — just you, in every project and worktree;\n" +
		"           stands down in projects that have their own swarmia-cli hooks\n\n" +
		"For project scope the command used to invoke swarmia-cli is auto-detected: a\n" +
		"swarmia-cli binary at the repo root (handy for local development), then an npm\n" +
		"dependency in node_modules/.bin, then a swarmia-cli on your $PATH. For local and\n" +
		"user scope it is the absolute path of the swarmia-cli running this install.\n" +
		"Override with --binary.",
	Args: cobra.NoArgs,
	RunE: func(cmd *cobra.Command, args []string) error {
		dir := installDir
		if dir == "" {
			dir = projectDir()
		}
		scope := installScope(installScopeF)
		switch scope {
		case scopeProject, scopeLocal, scopeUser:
		default:
			return fmt.Errorf("unknown --scope %q (want project, local or user)", installScopeF)
		}
		return runInstall(cmd.OutOrStdout(), dir, installOpts{binaryRef: installBinary, dryRun: installDryRun, scope: scope})
	},
}

func init() {
	installCmd.Flags().StringVar(&installDir, "dir", "", "repository directory to install into (default: $CLAUDE_PROJECT_DIR or cwd)")
	installCmd.Flags().StringVar(&installBinary, "binary", "", "override the auto-detected command used to invoke swarmia-cli in hooks")
	installCmd.Flags().BoolVar(&installDryRun, "dry-run", false, "report what would change without writing anything")
	installCmd.Flags().StringVar(&installScopeF, "scope", string(scopeProject), "where to install: project, local or user")
	rootCmd.AddCommand(installCmd)
}

type installOpts struct {
	binaryRef string
	dryRun    bool
	scope     installScope // "" means project
}

// agentConfig describes one supported coding agent: where its settings for a
// scope live (ok is false when the agent isn't used there) and how to add our
// hooks to them. New agents (Codex, Cursor, ...) plug in by appending to
// supportedAgents.
type agentConfig struct {
	name         string
	settingsPath func(repoDir string, scope installScope) (path string, ok bool)
	install      func(path, hookCommand string, dryRun bool) (hookChanges, error)
}

// hookChanges counts what an install did (or, in a dry run, would do) to one
// agent's settings.
type hookChanges struct {
	added   int
	removed int
}

var supportedAgents = []agentConfig{claudeCodeAgent}

func runInstall(out io.Writer, repoDir string, opts installOpts) error {
	scope := opts.scope
	if scope == "" {
		scope = scopeProject
	}
	binRef, source := opts.binaryRef, "--binary override"
	if binRef == "" {
		if scope == scopeProject {
			binRef, source = detectBinaryRef(repoDir)
		} else {
			binRef, source = executableRef()
		}
	}
	hookCommand := binRef + " log-hook"
	if scope == scopeUser {
		hookCommand += " " + skipIfProjectHooksFlag
	}
	fmt.Fprintf(out, "Hook command: %s (%s)\n", hookCommand, source)

	detected := 0
	for _, ag := range supportedAgents {
		path, ok := ag.settingsPath(repoDir, scope)
		if !ok {
			fmt.Fprintf(out, "%s: not detected, skipping\n", ag.name)
			continue
		}
		detected++
		res, err := ag.install(path, hookCommand, opts.dryRun)
		if err != nil {
			return fmt.Errorf("%s: %w", ag.name, err)
		}
		rel := displayPath(repoDir, path)
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

// executableRef is the absolute path of the running swarmia-cli, shell-quoted
// if needed: personal installs (local, user scope) point straight at the binary
// that installed them, so it needn't be on $PATH.
func executableRef() (ref, source string) {
	exe, err := os.Executable()
	if err == nil {
		exe, err = filepath.EvalSymlinks(exe)
	}
	if err != nil {
		return binaryName, "can't resolve this binary's path; assuming swarmia-cli is on $PATH"
	}
	if strings.ContainsAny(exe, " '\"\\$`") {
		exe = "'" + strings.ReplaceAll(exe, "'", `'\''`) + "'"
	}
	return exe, "this swarmia-cli binary"
}

// displayPath shows path relative to repoDir when inside it, else with the home
// directory abbreviated to ~.
func displayPath(repoDir, path string) string {
	if rel, err := filepath.Rel(repoDir, path); err == nil && !strings.HasPrefix(rel, "..") {
		return rel
	}
	if home, err := os.UserHomeDir(); err == nil {
		if rel, err := filepath.Rel(home, path); err == nil && !strings.HasPrefix(rel, "..") {
			return filepath.Join("~", rel)
		}
	}
	return path
}

// claudeConfigDir is Claude Code's user configuration directory.
func claudeConfigDir() (string, error) {
	if dir := os.Getenv("CLAUDE_CONFIG_DIR"); dir != "" {
		return dir, nil
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(home, ".claude"), nil
}

// --- Claude Code -----------------------------------------------------------

var claudeCodeAgent = agentConfig{
	name: "Claude Code",
	settingsPath: func(repoDir string, scope installScope) (string, bool) {
		switch scope {
		case scopeUser:
			dir, err := claudeConfigDir()
			return filepath.Join(dir, "settings.json"), err == nil
		case scopeLocal:
			return filepath.Join(repoDir, ".claude", "settings.local.json"), dirExists(filepath.Join(repoDir, ".claude"))
		default:
			return filepath.Join(repoDir, ".claude", "settings.json"), dirExists(filepath.Join(repoDir, ".claude"))
		}
	},
	install: func(path, hookCommand string, dryRun bool) (hookChanges, error) {
		doc, err := readJSONObject(path)
		if err != nil {
			return hookChanges{}, err
		}
		res := hookChanges{
			removed: removeObsoleteHooks(doc),
			added:   ensureClaudeHooks(doc, hookCommand),
		}
		if (res.added > 0 || res.removed > 0) && !dryRun {
			if err := writeJSONObject(path, doc); err != nil {
				return hookChanges{}, err
			}
		}
		return res, nil
	},
}

// ensureClaudeHooks adds the Swarmia hook set to a parsed settings document,
// returning how many entries were newly added. It mirrors the config this repo
// ships: log-hook on prompts, all tool use, and session lifecycle.
func ensureClaudeHooks(doc map[string]any, logHook string) int {
	specs := []struct {
		event, matcher, command, subcmd string
	}{
		{"UserPromptSubmit", "", logHook, "log-hook"},
		{"PreToolUse", "*", logHook, "log-hook"},
		{"PostToolUse", "*", logHook, "log-hook"},
		{"PostToolUseFailure", "*", logHook, "log-hook"},
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
