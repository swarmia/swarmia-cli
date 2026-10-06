package cmd

import (
	"os"
	"path/filepath"
)

// projectDir is the directory the CLI operates against: $CLAUDE_PROJECT_DIR when
// set (Claude Code exports it to hook commands), otherwise the current working
// directory. It is both where .swarmia/events.jsonl lives and the git repo that
// process-logs writes refs into.
func projectDir() string {
	if base := os.Getenv("CLAUDE_PROJECT_DIR"); base != "" {
		return base
	}
	if cwd, err := os.Getwd(); err == nil {
		return cwd
	}
	return ""
}

// eventsPath resolves the JSONL log file, first match wins: an explicit flag
// value, then $SWARMIA_LOG_FILE, then <project>/.swarmia/events.jsonl. Shared by
// log-hook (writer) and process-logs (reader) so both agree on the location.
func eventsPath(flagVal string) string {
	if flagVal != "" {
		return flagVal
	}
	if env := os.Getenv("SWARMIA_LOG_FILE"); env != "" {
		return env
	}
	return filepath.Join(projectDir(), ".swarmia", "events.jsonl")
}
