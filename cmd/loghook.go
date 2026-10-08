package cmd

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/spf13/cobra"

	"github.com/swarmia/swarmia-cli/internal/agent"
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
	logHookCmd.Flags().StringVar(&logAgent, "agent", agent.ClaudeCode.Name, "the agent sending the payloads: claude-code or codex")
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
	rec := events.Record{Format: events.Format, LoggedAt: time.Now().UTC().Format(time.RFC3339), Agent: ag.Name, Event: event}
	ev, _ := ag.Parse(payload)
	// Best-effort: an unreadable repo state must not stop the event being logged.
	if repoErr == nil {
		if st, err := repo.ReadState(); err == nil {
			rec.Git = &events.Git{Root: repo.Root, Head: st.Head, Branch: st.Branch}
		}
		rec.Changes, rec.Snapshot = snapshotShellCommand(repo, ev, filepath.Join(filepath.Dir(path), "snapshots"))
	}
	line, err := json.Marshal(rec)
	if err != nil {
		return fmt.Errorf("encoding record: %w", err)
	}
	line = append(line, '\n')
	if err := appendLine(path, line); err != nil {
		return err
	}
	if shouldProcess(ev, path) {
		startBackgroundProcess(path)
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

// hooksFileRunsLogHook reports whether an agent's settings or hooks file runs
// swarmia-cli log-hook unconditionally (not as a user-scope hook that stands
// down for project hooks).
func hooksFileRunsLogHook(path string) bool {
	data, err := os.ReadFile(path)
	if err != nil {
		return false
	}
	var doc map[string]any
	if json.Unmarshal(data, &doc) != nil {
		return false
	}
	hooks, _ := doc["hooks"].(map[string]any)
	for _, groups := range hooks {
		gs, _ := groups.([]any)
		for _, g := range gs {
			gm, _ := g.(map[string]any)
			for _, h := range asSlice(gm, "hooks") {
				if cmd := hookCommand(h); runsSwarmia(cmd, "log-hook") && !strings.Contains(cmd, skipIfProjectHooksFlag) {
					return true
				}
			}
		}
	}
	return false
}

// snapshotShellCommand brackets a shell tool call: before it runs, it snapshots
// the worktree's dirty files; when it finishes (successfully or not), it returns
// what the command authored. Edits through the agent's own edit tools are in
// their payloads already, so only shell calls need this, and not read-only
// ones. Errors just mean no changes are recorded — logging the event matters
// more — and the returned marker (see events.Record.Snapshot) says so.
func snapshotShellCommand(repo gitrepo.Repo, ev agent.Event, dir string) ([]events.FileChange, string) {
	if !ev.Tool.Shell || ev.Tool.UseID == "" {
		return nil, ""
	}
	store := snapshot.Store{Dir: dir}
	ctx, cancel := context.WithTimeout(context.Background(), snapshot.Budget)
	defer cancel()
	switch ev.Name {
	case "PreToolUse":
		store.Prune(time.Now())
		if snapshot.ReadOnly(ev.Tool.Command) {
			return nil, events.SnapshotReadOnly // so the command's end has nothing to diff
		}
		snap, err := store.Take(ctx, repo)
		if err == nil {
			err = store.Save(ev.Tool.UseID, snap)
		}
		return nil, snapshotMarker(err)
	case "PostToolUse", "PostToolUseFailure":
		pre, ok := store.Load(ev.Tool.UseID)
		if !ok {
			return nil, "" // read-only, or its start already says what went wrong
		}
		changes, err := store.Changes(ctx, repo, pre)
		if err != nil {
			return nil, snapshotMarker(err)
		}
		var out []events.FileChange
		for _, c := range changes {
			fc := events.FileChange{Path: c.Path}
			for _, h := range c.Hunks {
				fc.Hunks = append(fc.Hunks, h.Encode())
			}
			out = append(out, fc)
		}
		return out, ""
	}
	return nil, ""
}

func snapshotMarker(err error) string {
	switch {
	case err == nil:
		return ""
	case errors.Is(err, snapshot.ErrBudget):
		return events.SnapshotOverBudget
	}
	return events.SnapshotFailed
}

// shouldProcess decides when to fold the log in the background: when a session
// ends, and whenever the log has grown large (long-running sessions).
func shouldProcess(ev agent.Event, logPath string) bool {
	if ev.Name == "SessionEnd" {
		return true
	}
	fi, err := os.Stat(logPath)
	return err == nil && fi.Size() >= backgroundLogSize
}
