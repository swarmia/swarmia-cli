package cmd

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"

	"github.com/spf13/cobra"

	"github.com/swarmia/swarmia-cli/internal/agent"
)

const binaryName = "swarmia-cli"

var (
	installDir    string
	installBinary string
	installDryRun bool
	installScopeF string
	installTrust  bool
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
		scope := agent.Scope(installScopeF)
		if !slices.Contains(agent.Scopes, scope) {
			return fmt.Errorf("unknown --scope %q (want project, local or user)", installScopeF)
		}
		return runInstall(cmd.OutOrStdout(), dir, installOpts{binaryRef: installBinary, dryRun: installDryRun, scope: scope, trust: installTrust})
	},
}

func init() {
	installCmd.Flags().StringVar(&installDir, "dir", "", "repository directory to install into (default: $CLAUDE_PROJECT_DIR or cwd)")
	installCmd.Flags().StringVar(&installBinary, "binary", "", "override the auto-detected command used to invoke swarmia-cli in hooks")
	installCmd.Flags().BoolVar(&installDryRun, "dry-run", false, "report what would change without writing anything")
	installCmd.Flags().StringVar(&installScopeF, "scope", string(agent.ScopeProject), "where to install: project, local or user")
	installCmd.Flags().BoolVar(&installTrust, "trust", false, "trust swarmia-cli's Codex hooks in ~/.codex/config.toml, as approving them with /hooks would")
	rootCmd.AddCommand(installCmd)
}

type installOpts struct {
	binaryRef string
	dryRun    bool
	scope     agent.Scope // "" means project
	trust     bool        // record trust for Codex hooks
}

// agentConfig describes how to install into one supported coding agent:
// whether it is used for a scope (ok is false when it isn't, and why explains
// it), how its hooks run a swarmia-cli inside the repository, and the hooks to
// add to its settings. New agents plug in by appending to supportedAgents.
type agentConfig struct {
	*agent.Agent
	detected func(repoDir string, scope agent.Scope) (ok bool, why string)
	// inRepo is a hook command running the swarmia-cli at rel, relative to the
	// repository root, with args.
	inRepo func(rel, args string) string
	hooks  []hookSpec
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
		scope = agent.ScopeProject
	}
	ref := binaryRef{cmd: opts.binaryRef, source: "--binary override"}
	if ref.cmd == "" {
		if scope == agent.ScopeProject {
			ref = detectBinaryRef(repoDir)
		} else {
			ref = executableRef()
		}
	}
	fmt.Fprintf(out, "swarmia-cli: %s (%s)\n", ref, ref.source)

	detected := 0
	for _, ag := range supportedAgents {
		path, err := ag.HooksFile(repoDir, scope)
		if noScope := (*agent.NoScopeError)(nil); errors.As(err, &noScope) {
			fmt.Fprintf(out, "%s: %v; use --scope user, skipping\n", ag.Title, err)
			continue
		}
		if err != nil {
			fmt.Fprintf(out, "%s: %v, skipping\n", ag.Title, err)
			continue
		}
		if ok, why := ag.detected(repoDir, scope); !ok {
			fmt.Fprintf(out, "%s: %s, skipping\n", ag.Title, why)
			continue
		}
		detected++
		res, err := installHooks(path, ag.hookCommand(ref, scope), ag.hooks, opts.dryRun)
		if err != nil {
			return fmt.Errorf("%s: %w", ag.Title, err)
		}
		rel := displayPath(repoDir, path)
		for _, dup := range duplicateHooks(ag.Agent, repoDir, scope) {
			fmt.Fprintf(out, "Warning: %s already runs swarmia-cli log-hook for this project; with both, every event is logged twice. Remove the hooks from one of them.\n",
				displayPath(repoDir, dup))
		}
		if res.added == 0 && res.removed == 0 {
			fmt.Fprintf(out, "%s (%s): already configured\n", ag.Title, rel)
		} else {
			verb := "added"
			if opts.dryRun {
				verb = "would add"
			}
			msg := fmt.Sprintf("%s %d hook(s)", verb, res.added)
			if res.removed > 0 {
				msg += fmt.Sprintf(", removed %d obsolete hook(s)", res.removed)
			}
			fmt.Fprintf(out, "%s (%s): %s\n", ag.Title, rel, msg)
		}
		if err := ag.activate(out, path, opts); err != nil {
			return fmt.Errorf("%s: %w", ag.Title, err)
		}
	}

	if detected == 0 {
		fmt.Fprintln(out, "No supported coding agents detected in this repo. Nothing to do.")
	}
	return nil
}

// duplicateHooks lists the agent's other hooks files that already log this
// project's events, so installing into scope would log every event twice.
// User-scope hooks stand down where a project has its own, so they have none.
func duplicateHooks(ag *agent.Agent, repoDir string, scope agent.Scope) []string {
	if scope == agent.ScopeUser {
		return nil
	}
	var out []string
	for _, other := range agent.Scopes {
		if other == scope {
			continue
		}
		if path, err := ag.HooksFile(repoDir, other); err == nil && hooksFileRunsLogHook(path) {
			out = append(out, path)
		}
	}
	return out
}

// hookCommand is the command the agent's hooks run: log-hook, telling it
// which agent calls, and in user scope to stand down for project hooks.
func (ag agentConfig) hookCommand(ref binaryRef, scope agent.Scope) string {
	args := "log-hook"
	if ag.Agent != agent.ClaudeCode { // the default
		args += " --agent " + ag.Name
	}
	if scope == agent.ScopeUser {
		args += " " + skipIfProjectHooksFlag
	}
	if ref.rel != "" {
		return ag.inRepo(ref.rel, args)
	}
	return ref.cmd + " " + args
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

// --- Claude Code -----------------------------------------------------------

var claudeCodeAgent = agentConfig{
	Agent: agent.ClaudeCode,
	detected: func(repoDir string, scope agent.Scope) (bool, string) {
		if scope == agent.ScopeUser {
			return true, "" // its user settings are created as needed
		}
		return dirExists(filepath.Join(repoDir, agent.ClaudeCode.ConfigDir)), "not detected"
	},
	inRepo: func(rel, args string) string { return "${CLAUDE_PROJECT_DIR}/" + rel + " " + args },
	activate: func(out io.Writer, path string, opts installOpts) error {
		if !opts.dryRun {
			fmt.Fprintln(out, "  Reload hooks with /hooks in Claude Code (or start a fresh session) to activate.")
		}
		return nil
	},
	hooks: claudeHookSpecs,
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

// installHooks adds log-hook, run by command, to an agent's settings file for
// each spec, and removes obsolete swarmia-cli hooks. The file is only written
// when something changed, and never in a dry run.
func installHooks(path, command string, specs []hookSpec, dryRun bool) (hookChanges, error) {
	doc, err := readJSONObject(path)
	if err != nil {
		return hookChanges{}, err
	}
	res := hookChanges{removed: removeObsoleteHooks(doc)}
	for _, s := range specs {
		if ensureHook(doc, s, command) {
			res.added++
		}
	}
	if (res.added > 0 || res.removed > 0) && !dryRun {
		if err := writeJSONObject(path, doc); err != nil {
			return hookChanges{}, err
		}
	}
	return res, nil
}

// ensureHook makes sure the spec's event has a command hook running our
// log-hook, under a group with the spec's matcher. It is a no-op (returns false)
// when such a hook already exists, matching any swarmia-cli command regardless of
// how the binary is referenced so re-running with a different --binary won't
// duplicate. Existing unrelated groups and hooks are preserved.
func ensureHook(doc map[string]any, spec hookSpec, command string) bool {
	event, matcher := spec.event, spec.matcher
	hooks := childMap(doc, "hooks")
	arr, _ := hooks[event].([]any)

	for _, g := range arr {
		gm, ok := g.(map[string]any)
		if !ok || !matcherEquals(gm["matcher"], matcher) {
			continue
		}
		if groupHasSwarmia(gm, "log-hook") {
			return false // already installed
		}
	}

	newHook := map[string]any{"type": "command", "command": command}
	if spec.timeout > 0 {
		newHook["timeout"] = spec.timeout
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
	for _, sub := range obsoleteSubcommands {
		if runsSwarmia(hookCommand(h), sub) {
			return true
		}
	}
	return false
}

func groupHasSwarmia(group map[string]any, subcmd string) bool {
	for _, h := range asSlice(group, "hooks") {
		if runsSwarmia(hookCommand(h), subcmd) {
			return true
		}
	}
	return false
}

// runsSwarmia reports whether a hook command runs swarmia-cli's subcmd, however
// the binary is referenced.
func runsSwarmia(command, subcmd string) bool {
	return strings.Contains(command, binaryName) && strings.Contains(command, subcmd)
}

// hookCommand is a hook entry's command, or "" if it has none.
func hookCommand(h any) string {
	hm, _ := h.(map[string]any)
	cmd, _ := hm["command"].(string)
	return cmd
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
