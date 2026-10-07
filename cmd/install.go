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
	installTrust  bool
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
		"(Claude Code, via a .claude directory; Codex, via a .codex directory) and adds\n" +
		"the Swarmia hooks to each one's settings.\n\n" +
		"It is idempotent: hooks already present are left alone, and unrelated settings and\n" +
		"hooks are preserved. Hooks for subcommands that no longer exist (process-logs) are\n" +
		"removed.\n\n" +
		"--scope picks the settings file:\n" +
		"  project  .claude/settings.json, .codex/hooks.json — committed, so hooks for\n" +
		"           everyone (default)\n" +
		"  local    .claude/settings.local.json — just you, in this checkout (Claude Code\n" +
		"           only: Codex has no personal per-project hooks file)\n" +
		"  user     ~/.claude/settings.json, ~/.codex/hooks.json — just you, in every\n" +
		"           project and worktree; stands down in projects that have their own\n" +
		"           swarmia-cli hooks\n\n" +
		"For project scope the command used to invoke swarmia-cli is auto-detected: a\n" +
		"swarmia-cli binary at the repo root (handy for local development), then an npm\n" +
		"dependency in node_modules, then a swarmia-cli on your $PATH. For local and\n" +
		"user scope it is the absolute path of the swarmia-cli running this install.\n" +
		"Override with --binary.\n\n" +
		"Codex runs a hook only once you have trusted it, normally by approving it with\n" +
		"/hooks inside Codex (each person, for project hooks too). --trust records that\n" +
		"approval for swarmia-cli's Codex hooks in your ~/.codex/config.toml instead.",
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
		return runInstall(cmd.OutOrStdout(), dir, installOpts{binaryRef: installBinary, dryRun: installDryRun, scope: scope, trust: installTrust})
	},
}

func init() {
	installCmd.Flags().StringVar(&installDir, "dir", "", "repository directory to install into (default: $CLAUDE_PROJECT_DIR or cwd)")
	installCmd.Flags().StringVar(&installBinary, "binary", "", "override the auto-detected command used to invoke swarmia-cli in hooks")
	installCmd.Flags().BoolVar(&installDryRun, "dry-run", false, "report what would change without writing anything")
	installCmd.Flags().StringVar(&installScopeF, "scope", string(scopeProject), "where to install: project, local or user")
	installCmd.Flags().BoolVar(&installTrust, "trust", false, "trust swarmia-cli's Codex hooks in ~/.codex/config.toml, as approving them with /hooks would")
	rootCmd.AddCommand(installCmd)
}

type installOpts struct {
	binaryRef string
	dryRun    bool
	scope     installScope // "" means project
	trust     bool         // record trust for Codex hooks
}

// agentConfig describes one supported coding agent: where its settings for a
// scope live (ok is false when the agent isn't used there, and why explains
// it), the hook command that finds swarmia-cli, and how to add our hooks to
// the settings. New agents plug in by appending to supportedAgents.
type agentConfig struct {
	name         string
	settingsPath func(repoDir string, scope installScope) (path string, ok bool, why string)
	hookCommand  func(ref binaryRef, scope installScope) string
	install      func(path, hookCommand string, dryRun bool) (hookChanges, error)
	// duplicates lists other settings files that already log this project's
	// events, so installing into scope would log every event twice.
	duplicates func(repoDir string, scope installScope) []string
	// activate tells the user what's left to do once hooks are installed at
	// path (and does it, where asked and possible).
	activate func(out io.Writer, path string, opts installOpts) error
}

// binaryRef is how a hook finds swarmia-cli.
type binaryRef struct {
	rel    string // a path relative to the repository root (project scope), or
	cmd    string // a command as is: an absolute path or a name on $PATH
	source string // how it was chosen, for the user
}

// hookChanges counts what an install did (or, in a dry run, would do) to one
// agent's settings.
type hookChanges struct {
	added   int
	removed int
}

var supportedAgents = []agentConfig{claudeCodeAgent, codexAgent}

func runInstall(out io.Writer, repoDir string, opts installOpts) error {
	scope := opts.scope
	if scope == "" {
		scope = scopeProject
	}
	ref := binaryRef{cmd: opts.binaryRef, source: "--binary override"}
	if ref.cmd == "" {
		if scope == scopeProject {
			ref = detectBinaryRef(repoDir)
		} else {
			ref = executableRef()
		}
	}
	fmt.Fprintf(out, "swarmia-cli: %s (%s)\n", ref, ref.source)

	detected := 0
	for _, ag := range supportedAgents {
		path, ok, why := ag.settingsPath(repoDir, scope)
		if !ok {
			fmt.Fprintf(out, "%s: %s, skipping\n", ag.name, why)
			continue
		}
		detected++
		res, err := ag.install(path, ag.hookCommand(ref, scope), opts.dryRun)
		if err != nil {
			return fmt.Errorf("%s: %w", ag.name, err)
		}
		rel := displayPath(repoDir, path)
		for _, dup := range ag.duplicates(repoDir, scope) {
			fmt.Fprintf(out, "Warning: %s already runs swarmia-cli log-hook for this project; with both, every event is logged twice. Remove the hooks from one of them.\n",
				displayPath(repoDir, dup))
		}
		if res.added == 0 && res.removed == 0 {
			fmt.Fprintf(out, "%s (%s): already configured\n", ag.name, rel)
		} else {
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
		if err := ag.activate(out, path, opts); err != nil {
			return fmt.Errorf("%s: %w", ag.name, err)
		}
	}

	if detected == 0 {
		fmt.Fprintln(out, "No supported coding agents detected in this repo. Nothing to do.")
	}
	return nil
}

func (r binaryRef) String() string {
	if r.rel != "" {
		return "<repo>/" + r.rel
	}
	return r.cmd
}

// detectBinaryRef decides how project hooks should invoke swarmia-cli, in
// order of preference: a binary built at the repo root (local development),
// then an npm dependency (the @swarmia/cli package), then a binary on $PATH
// (global install).
func detectBinaryRef(repoDir string) binaryRef {
	if isExecutable(filepath.Join(repoDir, binaryName)) {
		return binaryRef{rel: binaryName, source: "local binary at repo root"}
	}
	// The package's own launcher, not node_modules/.bin: pnpm's .bin shim is
	// itself a shell script that forks several processes, doubling the cost of
	// every hook. npm and pnpm both provide this path for direct dependencies.
	if isExecutable(filepath.Join(repoDir, "node_modules", "@swarmia", "cli", "bin", binaryName)) {
		return binaryRef{rel: "node_modules/@swarmia/cli/bin/" + binaryName, source: "npm dependency (@swarmia/cli)"}
	}
	if isExecutable(filepath.Join(repoDir, "node_modules", ".bin", binaryName)) {
		return binaryRef{rel: "node_modules/.bin/" + binaryName, source: "npm dependency (node_modules/.bin)"}
	}
	if _, err := exec.LookPath(binaryName); err == nil {
		return binaryRef{cmd: binaryName, source: "found on $PATH"}
	}
	return binaryRef{cmd: binaryName, source: "not found; assuming it will be on $PATH"}
}

// executableRef is the absolute path of the running swarmia-cli, shell-quoted
// if needed: personal installs (local, user scope) point straight at the binary
// that installed them, so it needn't be on $PATH.
func executableRef() binaryRef {
	exe, err := os.Executable()
	if err == nil {
		exe, err = filepath.EvalSymlinks(exe)
	}
	if err != nil {
		return binaryRef{cmd: binaryName, source: "can't resolve this binary's path; assuming swarmia-cli is on $PATH"}
	}
	return binaryRef{cmd: shellQuote(exe), source: "this swarmia-cli binary"}
}

// shellQuote single-quotes s for sh if it needs quoting.
func shellQuote(s string) string {
	if strings.ContainsAny(s, " '\"\\$`") {
		return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'"
	}
	return s
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
	settingsPath: func(repoDir string, scope installScope) (string, bool, string) {
		switch scope {
		case scopeUser:
			dir, err := claudeConfigDir()
			return filepath.Join(dir, "settings.json"), err == nil, "no home directory"
		case scopeLocal:
			return filepath.Join(repoDir, ".claude", "settings.local.json"), dirExists(filepath.Join(repoDir, ".claude")), "not detected"
		default:
			return filepath.Join(repoDir, ".claude", "settings.json"), dirExists(filepath.Join(repoDir, ".claude")), "not detected"
		}
	},
	hookCommand: func(ref binaryRef, scope installScope) string {
		command := ref.cmd
		if ref.rel != "" {
			command = "${CLAUDE_PROJECT_DIR}/" + ref.rel
		}
		command += " log-hook"
		if scope == scopeUser {
			command += " " + skipIfProjectHooksFlag
		}
		return command
	},
	activate: func(out io.Writer, path string, opts installOpts) error {
		if !opts.dryRun {
			fmt.Fprintln(out, "  Reload hooks with /hooks in Claude Code (or start a fresh session) to activate.")
		}
		return nil
	},
	duplicates: func(repoDir string, scope installScope) []string {
		if scope == scopeUser {
			return nil // user-scope hooks stand down where a project has its own
		}
		other := "settings.local.json"
		if scope == scopeLocal {
			other = "settings.json"
		}
		if path := filepath.Join(repoDir, ".claude", other); settingsRunLogHook(path) {
			return []string{path}
		}
		return nil
	},
	install: func(path, hookCommand string, dryRun bool) (hookChanges, error) {
		doc, err := readJSONObject(path)
		if err != nil {
			return hookChanges{}, err
		}
		res := hookChanges{
			removed: removeObsoleteHooks(doc),
			added:   ensureHooks(doc, hookCommand, claudeHookSpecs),
		}
		if (res.added > 0 || res.removed > 0) && !dryRun {
			if err := writeJSONObject(path, doc); err != nil {
				return hookChanges{}, err
			}
		}
		return res, nil
	},
}

// hookSpec is one hook to install: log-hook on an event, under a matcher,
// with a timeout in seconds (0: the agent's default).
type hookSpec struct {
	event, matcher string
	timeout        int
}

// claudeHookSpecs is log-hook on prompts, all tool use, and session lifecycle.
var claudeHookSpecs = []hookSpec{
	{"UserPromptSubmit", "", 0},
	{"PreToolUse", "*", 0},
	{"PostToolUse", "*", 0},
	{"PostToolUseFailure", "*", 0},
	{"SessionStart", "", 0},
	{"SessionEnd", "", 0},
	{"Stop", "", 0},
}

// ensureHooks adds the Swarmia hook set to a parsed settings document,
// returning how many entries were newly added.
func ensureHooks(doc map[string]any, logHook string, specs []hookSpec) int {
	added := 0
	for _, s := range specs {
		if ensureHook(doc, s.event, s.matcher, logHook, "log-hook", s.timeout) {
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
func ensureHook(doc map[string]any, event, matcher, command, subcmd string, timeout int) bool {
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
	if timeout > 0 {
		newHook["timeout"] = timeout
	}

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
