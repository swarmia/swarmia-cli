package cmd

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"maps"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"time"

	"github.com/spf13/cobra"

	"github.com/swarmia/swarmia-cli/internal/agent"
	"github.com/swarmia/swarmia-cli/internal/deliver"
	"github.com/swarmia/swarmia-cli/internal/events"
	"github.com/swarmia/swarmia-cli/internal/fslock"
	"github.com/swarmia/swarmia-cli/internal/gitrepo"
	"github.com/swarmia/swarmia-cli/internal/snapshot"
)

var (
	logFile               string
	logSkipIfProjectHooks bool
	logAgent              string
)

var logHookCmd = &cobra.Command{
	Use:   "log-hook",
	Short: "Log a coding agent's hook payload to a JSONL file",
	Long: "log-hook reads a hook payload from stdin and appends it to a JSONL file, one\n" +
		"event per line. It is meant to be registered as a hook command in a coding\n" +
		"agent's settings (see install) so that agent activity can later be attributed\n" +
		"to commits. --agent says which agent sends the payloads.\n\n" +
		"It is a passive logger: it always exits 0 and writes nothing to stdout, so it\n" +
		"can never block or interfere with the tool call that triggered it.",
	// SilenceUsage/SilenceErrors keep cobra from ever writing to stdout/stderr on
	// our behalf; RunE always returns nil so a logging failure can't fail the hook.
	SilenceUsage:  true,
	SilenceErrors: true,
	// Hook commands are committed to repositories, so binaries of different
	// versions run them: a flag added later must not fail an older binary's
	// hook on every event. Unknown flags are ignored; any other flag error is
	// reported and the event skipped, still exiting 0.
	FParseErrWhitelist: cobra.FParseErrWhitelist{UnknownFlags: true},
	RunE: func(cmd *cobra.Command, args []string) error {
		if err := runLogHook(cmd.InOrStdin()); err != nil {
			// Non-blocking: surface in the transcript but still exit 0.
			fmt.Fprintf(os.Stderr, "swarmia-cli log-hook: %v\n", err)
		}
		return nil
	},
}

func init() {
	logHookCmd.Flags().StringVar(&logFile, "file", "", "path to the JSONL log file (overrides $SWARMIA_LOG_FILE and the default)")
	logHookCmd.Flags().BoolVar(&logSkipIfProjectHooks, strings.TrimPrefix(skipIfProjectHooksFlag, "--"), false,
		"do nothing if the project's own agent settings run swarmia-cli log-hook (set by user-scope installs)")
	logHookCmd.Flags().StringVar(&logAgent, "agent", agent.ClaudeCode.Name, "the agent sending the payloads: claude-code, codex or copilot")
	logHookCmd.SetFlagErrorFunc(func(cmd *cobra.Command, err error) error {
		fmt.Fprintf(os.Stderr, "swarmia-cli log-hook: %v\n", err)
		return nil
	})
	rootCmd.AddCommand(logHookCmd)
}

func runLogHook(stdin io.Reader) error {
	ag := agent.Lookup(logAgent)
	if ag == nil {
		return fmt.Errorf("unknown --agent %q", logAgent)
	}
	dir := projectDir()
	repo, repoErr := gitrepo.Find(dir)
	if logSkipIfProjectHooks && projectHasLogHook(ag, dir, repo.Root) {
		return nil // the project's own hook logs this event
	}
	payload, err := io.ReadAll(stdin)
	if err != nil {
		return fmt.Errorf("reading stdin: %w", err)
	}
	payload = []byte(strings.TrimSpace(string(payload)))
	if len(payload) == 0 {
		// Nothing to log; don't write a blank line.
		return nil
	}
	if ag == agent.ClaudeCode && agent.IsCopilotPayload(payload) {
		// GitHub Copilot runs the repository's Claude Code hooks too. Its
		// own hooks log the event, if it has them here; else this one does,
		// as Copilot's.
		if copilotHasLogHook(dir, repo.Root) {
			return nil
		}
		ag = agent.Copilot
	}

	// Preserve the payload verbatim when it is valid JSON. If it isn't (defensive
	// — Claude Code always sends JSON), store it as a JSON string so every line of
	// the file stays valid JSONL.
	var event json.RawMessage
	if json.Valid(payload) {
		event = json.RawMessage(payload)
	} else {
		encoded, err := json.Marshal(string(payload))
		if err != nil {
			return fmt.Errorf("encoding non-JSON payload: %w", err)
		}
		event = encoded
	}

	path, err := eventsPath(logFile)
	if err != nil {
		return fmt.Errorf("resolving log file: %w", err)
	}
	rec := events.Record{LoggedAt: time.Now().UTC().Format(time.RFC3339), Agent: ag.Name, Event: event}
	ev, _ := ag.Parse(payload)
	// Best-effort: an unreadable repo state must not stop the event being logged.
	if repoErr == nil {
		if st, err := repo.ReadState(); err == nil {
			rec.Git = &events.Git{Root: repo.Root, Head: st.Head, Branch: st.Branch}
		}
	}
	store := snapshot.Store{Dir: filepath.Join(filepath.Dir(path), "snapshots")}
	if ev.Cwd == "" {
		ev.Cwd = dir // Claude Code's project directory, or where Codex runs hooks
	}
	touchWorkingSet(store, ev)
	snapshotShellCommand(store, ev, &rec)
	if ev.Name == "Stop" && ev.Response == "" {
		rec.Response = ag.TurnResponse(ev.TranscriptPath)
	}
	rec.Format = events.FormatOf(rec)
	line, err := json.Marshal(rec)
	if err != nil {
		return fmt.Errorf("encoding record: %w", err)
	}
	line = append(line, '\n')
	if err := appendLine(path, line); err != nil {
		return err
	}
	if shouldProcess(ev, path) {
		// Waiting at session end, so a periodic run already in progress can't
		// leave the session's last events unsent.
		startBackgroundProcess(path, ev.Name == "SessionEnd")
	}
	return nil
}

// appendLine appends a single pre-serialized line under an exclusive file lock,
// so concurrent hook invocations (parallel tool calls, subagents) can't produce
// interleaved or corrupted lines.
func appendLine(path string, line []byte) error {
	if dir := filepath.Dir(path); dir != "" {
		if err := os.MkdirAll(dir, 0o700); err != nil {
			return fmt.Errorf("creating %s: %w", dir, err)
		}
	}

	// Owner-only: the log holds prompts and code from agent sessions.
	f, err := os.OpenFile(path, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o600)
	if err != nil {
		return fmt.Errorf("opening %s: %w", path, err)
	}
	defer f.Close()

	unlock, err := fslock.Lock(f)
	if err != nil {
		return fmt.Errorf("locking %s: %w", path, err)
	}
	defer unlock()

	if _, err := f.Write(line); err != nil {
		return fmt.Errorf("writing %s: %w", path, err)
	}
	return nil
}

// projectHasLogHook reports whether the project's own hooks for the agent
// already run swarmia-cli log-hook, without our user-scope flag.
func projectHasLogHook(ag *agent.Agent, projectDir, repoRoot string) bool {
	for _, path := range ag.ProjectHooksFiles(projectDir, repoRoot) {
		if hooksFileRunsLogHook(path) {
			return true
		}
	}
	return false
}

// copilotHasLogHook reports whether GitHub Copilot's own hooks run swarmia-cli
// log-hook in the project: its project or local hooks, or its user hooks
// (which, with their skip flag, stand down only for its own project hooks).
func copilotHasLogHook(projectDir, repoRoot string) bool {
	files := agent.Copilot.ProjectHooksFiles(projectDir, repoRoot)
	if user, err := agent.Copilot.HooksFile("", agent.ScopeUser); err == nil {
		files = append(files, user)
	}
	for _, path := range files {
		if hooksFileHasLogHook(path, true) {
			return true
		}
	}
	return false
}

// hooksFileRunsLogHook reports whether an agent's settings or hooks file runs
// swarmia-cli log-hook unconditionally (not as a user-scope hook that stands
// down for project hooks).
func hooksFileRunsLogHook(path string) bool {
	return hooksFileHasLogHook(path, false)
}

// hooksFileHasLogHook reports whether an agent's settings or hooks file runs
// swarmia-cli log-hook, counting user-scope hooks only if asked. Hooks are
// in matcher groups (Claude Code, Codex) or listed directly (Copilot).
func hooksFileHasLogHook(path string, userScope bool) bool {
	data, err := os.ReadFile(path)
	if err != nil {
		return false
	}
	var doc map[string]any
	if json.Unmarshal(data, &doc) != nil {
		return false
	}
	hooks, _ := doc["hooks"].(map[string]any)
	for _, entries := range hooks {
		es, _ := entries.([]any)
		for _, e := range es {
			em, _ := e.(map[string]any)
			for _, h := range append([]any{em}, asSlice(em, "hooks")...) {
				if cmd := hookCommand(h); runsSwarmia(cmd, "log-hook") && (userScope || !strings.Contains(cmd, skipIfProjectHooksFlag)) {
					return true
				}
			}
		}
	}
	return false
}

// touchWorkingSet adds the worktrees a tool call works in, its file's and its
// working directory's, to the session's working set, which later shell
// commands are snapshotted in (see snapshot.Scope).
func touchWorkingSet(store snapshot.Store, ev agent.Event) {
	if ev.Tool.Name == "" {
		return
	}
	var roots []string
	if filepath.IsAbs(ev.Tool.File) {
		if repo, err := gitrepo.Find(filepath.Dir(ev.Tool.File)); err == nil {
			roots = append(roots, repo.Root)
		}
	}
	if repo, err := gitrepo.Find(ev.Cwd); err == nil {
		roots = append(roots, repo.Root)
	}
	store.Touch(ev.SessionID, roots...)
}

// snapshotShellCommand brackets a shell tool call: before it runs, it
// snapshots the dirty files of each worktree the call may write in (see
// snapshot.Scope); when it finishes (successfully or not), it records on rec
// what the command authored in them. Edits through the agent's own edit tools
// are in their payloads already, so only shell calls need this, and not
// read-only ones. Errors just mean changes go unrecorded in that worktree —
// logging the event matters more — and rec.Snapshot says so.
//
// A background shell command (see agent.Tool.Background) is still running when
// its call ends: its snapshot is set aside then, and its changes recorded on
// the call that finds it finished (agent.Tool.Awaited).
func snapshotShellCommand(store snapshot.Store, ev agent.Event, rec *events.Record) {
	if ev.Tool.Awaited != "" && ev.Name == "PostToolUse" {
		id := backgroundID(ev.SessionID, ev.Tool.Awaited)
		if at, ok := store.SavedAt(id); ok && time.Since(at) > backgroundShellLimit {
			store.Load(id) // drop it
			rec.Snapshot = events.SnapshotOverBudget
			return
		}
		recordShellChanges(store, ev, rec, id)
		return
	}
	if !ev.Tool.Shell || ev.Tool.UseID == "" {
		return
	}
	if ev.Tool.Background != "" && ev.Name == "PostToolUse" && ev.Tool.Status == "" {
		if pre, ok := store.Load(ev.Tool.UseID); ok {
			if err := store.Save(backgroundID(ev.SessionID, ev.Tool.Background), pre); err != nil {
				rec.Snapshot = events.SnapshotFailed
			}
		}
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), snapshot.Budget)
	defer cancel()
	switch ev.Name {
	case "PreToolUse":
		store.Prune(time.Now())
		if snapshot.ReadOnly(ev.Tool.Command) {
			rec.Snapshot = events.SnapshotReadOnly // so the command's end has nothing to diff
			return
		}
		repos := snapshot.Scope(ev.Tool.Command, ev.Cwd, store.WorkingSet(ev.SessionID))
		pre, err := store.Before(ctx, repos)
		if len(pre) > 0 {
			if serr := store.Save(ev.Tool.UseID, pre); err == nil {
				err = serr
			}
		}
		rec.Snapshot, rec.Snapshotted = snapshotMarker(err), slices.Sorted(maps.Keys(pre))
	case "PostToolUse", "PostToolUseFailure":
		recordShellChanges(store, ev, rec, ev.Tool.UseID)
	}
}

// backgroundShellLimit is how long a background shell command may run and
// still have its changes recorded. Everything that changed in the worktree
// while it ran is taken as its work, and the longer it runs, the likelier
// that includes the developer's own edits.
const backgroundShellLimit = 10 * time.Minute

// backgroundID is the id a background shell command's snapshot is kept under
// while it runs: its shell's id is only unique within the session.
func backgroundID(session, shell string) string {
	return "bg-" + session + "-" + shell
}

// recordShellChanges records on rec what the shell command whose snapshot was
// saved under id authored since.
func recordShellChanges(store snapshot.Store, ev agent.Event, rec *events.Record, id string) {
	pre, ok := store.Load(id)
	if !ok {
		return // read-only, or its start already says what went wrong
	}
	ctx, cancel := context.WithTimeout(context.Background(), snapshot.Budget)
	defer cancel()
	changes, err := store.After(ctx, pre)
	rec.Snapshot, rec.Snapshotted = snapshotMarker(err), slices.Sorted(maps.Keys(pre))
	own := ""
	if rec.Git != nil {
		own = rec.Git.Root
	}
	var changed []string
	for _, root := range slices.Sorted(maps.Keys(changes)) {
		changed = append(changed, root)
		for _, c := range changes[root] {
			fc := events.FileChange{Path: c.Path}
			if root != own {
				fc.Root = root
			}
			for _, h := range c.Hunks {
				fc.Hunks = append(fc.Hunks, h.Encode())
			}
			rec.Changes = append(rec.Changes, fc)
		}
	}
	store.Touch(ev.SessionID, changed...)
}

func snapshotMarker(err error) string {
	switch {
	case err == nil:
		return ""
	case errors.Is(err, snapshot.ErrBudget):
		return events.SnapshotOverBudget
	case errors.Is(err, snapshot.ErrUnknownSource):
		return events.SnapshotGitOperation
	}
	return events.SnapshotFailed
}

// shouldProcess decides when to fold the log in the background: when a session
// ends, whenever the log has grown large, and at the end of a turn once a
// resend interval has passed, so a session that never ends (a remote-control
// session in a container that is killed) is still delivered while it runs.
func shouldProcess(ev agent.Event, logPath string) bool {
	if ev.Name == "SessionEnd" {
		return true
	}
	if fi, err := os.Stat(logPath); err == nil && fi.Size() >= backgroundLogSize {
		return true
	}
	return ev.Name == "Stop" && periodicProcessDue(filepath.Dir(logPath), time.Now())
}

// periodicProcessStamp is touched in the state directory whenever a turn end
// starts a periodic run, so concurrent sessions share one interval.
const periodicProcessStamp = "periodic-process"

// periodicProcessDue reports whether MinResendInterval has passed since the
// last periodic run (or there was none), and if so restarts the interval.
func periodicProcessDue(stateDir string, now time.Time) bool {
	stamp := filepath.Join(stateDir, periodicProcessStamp)
	fi, err := os.Stat(stamp)
	if err == nil && now.Sub(fi.ModTime()) < deliver.MinResendInterval {
		return false
	}
	if err := os.WriteFile(stamp, nil, 0o600); err != nil {
		return false
	}
	return os.Chtimes(stamp, now, now) == nil
}
