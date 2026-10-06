package cmd

import (
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/spf13/cobra"
)

var logFile string

var logHookCmd = &cobra.Command{
	Use:   "log-hook",
	Short: "Log a Claude Code hook payload to a JSONL file",
	Long: "log-hook reads a Claude Code hook payload from stdin and appends it to a JSONL\n" +
		"file, one event per line. It is meant to be registered as a hook command in\n" +
		"Claude Code settings so that agent activity can later be attributed to commits.\n\n" +
		"It is a passive logger: it always exits 0 and writes nothing to stdout, so it\n" +
		"can never block or interfere with the tool call that triggered it.",
	// SilenceUsage/SilenceErrors keep cobra from ever writing to stdout/stderr on
	// our behalf; RunE always returns nil so a logging failure can't fail the hook.
	SilenceUsage:  true,
	SilenceErrors: true,
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
	rootCmd.AddCommand(logHookCmd)
}

// hookRecord is the envelope written per line: a capture timestamp, the
// project's git position at that moment (omitted outside a repo), and the
// original payload nested verbatim.
type hookRecord struct {
	LoggedAt string          `json:"logged_at"`
	Git      *gitState       `json:"git,omitempty"`
	Event    json.RawMessage `json:"event"`
}

func runLogHook(stdin io.Reader) error {
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

	rec := hookRecord{LoggedAt: time.Now().UTC().Format(time.RFC3339), Event: event}
	// Best-effort: an unreadable repo state must not stop the event being logged.
	if st, err := readGitState(projectDir()); err == nil {
		rec.Git = &st
	}
	line, err := json.Marshal(rec)
	if err != nil {
		return fmt.Errorf("encoding record: %w", err)
	}
	line = append(line, '\n')

	path, err := eventsPath(logFile)
	if err != nil {
		return fmt.Errorf("resolving log file: %w", err)
	}
	return appendLine(path, line)
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

	unlock, err := lockFile(f)
	if err != nil {
		return fmt.Errorf("locking %s: %w", path, err)
	}
	defer unlock()

	if _, err := f.Write(line); err != nil {
		return fmt.Errorf("writing %s: %w", path, err)
	}
	return nil
}
