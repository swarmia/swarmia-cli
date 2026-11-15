package cmd

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"time"

	"github.com/swarmia/swarmia-cli/internal/gitrepo"
	"github.com/swarmia/swarmia-cli/internal/report"
)

// projectDir is the directory the CLI operates against: $CLAUDE_PROJECT_DIR when
// set (Claude Code exports it to hook commands), otherwise the current working
// directory (where Codex runs hooks: the session's, maybe below the repository
// root).
func projectDir() string {
	if base := os.Getenv("CLAUDE_PROJECT_DIR"); base != "" {
		return base
	}
	if cwd, err := os.Getwd(); err == nil {
		return cwd
	}
	return ""
}

// stateBaseDir is the per-user root for all local state, outside any repository
// so nothing shows up in `git status` or is lost to `git clean -fdx`.
// $SWARMIA_STATE_DIR overrides the OS default.
func stateBaseDir() (string, error) {
	if dir := os.Getenv("SWARMIA_STATE_DIR"); dir != "" {
		return dir, nil
	}
	switch runtime.GOOS {
	case "darwin":
		home, err := os.UserHomeDir()
		if err != nil {
			return "", err
		}
		return filepath.Join(home, "Library", "Application Support", "swarmia"), nil
	case "windows":
		if dir := os.Getenv("LocalAppData"); dir != "" {
			return filepath.Join(dir, "swarmia"), nil
		}
		dir, err := os.UserConfigDir()
		if err != nil {
			return "", err
		}
		return filepath.Join(dir, "swarmia"), nil
	default:
		if dir := os.Getenv("XDG_STATE_HOME"); dir != "" {
			return filepath.Join(dir, "swarmia"), nil
		}
		home, err := os.UserHomeDir()
		if err != nil {
			return "", err
		}
		return filepath.Join(home, ".local", "state", "swarmia"), nil
	}
}

// repoKey identifies the repository enclosing dir: its git common dir, so all
// worktrees of a clone share state. Outside a git repo it falls back to the
// directory itself, so sessions there are still captured.
func repoKey(dir string) string {
	if repo, err := gitrepo.Find(dir); err == nil {
		return repo.CommonDir
	}
	if abs, err := filepath.Abs(dir); err == nil {
		if resolved, err := gitrepo.Canonical(abs); err == nil {
			return resolved
		}
		return abs
	}
	return dir
}

// repoStateDir is <stateBase>/repos/<id> for the repository enclosing dir, where
// id is a short hash of repoKey. A repo.json inside records which repo it is,
// for humans browsing the state directory.
func repoStateDir(dir string) (string, error) {
	base, err := stateBaseDir()
	if err != nil {
		return "", err
	}
	key := repoKey(dir)
	sum := sha256.Sum256([]byte(key))
	return filepath.Join(base, "repos", hex.EncodeToString(sum[:8])), nil
}

// ensureRepoStateDir creates the repo's state directory (and its repo.json) if
// needed and returns its path.
func ensureRepoStateDir(dir string) (string, error) {
	stateDir, err := repoStateDir(dir)
	if err != nil {
		return "", err
	}
	meta := filepath.Join(stateDir, "repo.json")
	if _, err := os.Stat(meta); err == nil {
		return stateDir, nil
	}
	if err := os.MkdirAll(stateDir, 0o700); err != nil {
		return "", err
	}
	data, _ := json.MarshalIndent(map[string]string{"repo": repoKey(dir)}, "", "  ")
	if err := os.WriteFile(meta, append(data, '\n'), 0o600); err != nil {
		return "", err
	}
	return stateDir, nil
}

// eventsPath resolves the JSONL log file, first match wins: an explicit flag
// value, then $SWARMIA_LOG_FILE, then events.jsonl in the project's repo state
// directory.
func eventsPath(flagVal string) (string, error) {
	if flagVal != "" {
		return flagVal, nil
	}
	if env := os.Getenv("SWARMIA_LOG_FILE"); env != "" {
		return env, nil
	}
	stateDir, err := ensureRepoStateDir(projectDir())
	if err != nil {
		return "", err
	}
	return filepath.Join(stateDir, "events.jsonl"), nil
}

// ownersDir, under the state base, is the session owner index: one file per
// session, named by a hash of its id, holding the state directory (relative
// to the base) that logged the session first.
const ownersDir = "owners"

// sessionEventsPath is eventsPath for an event of session id. Where a session
// is logged depends on where the agent runs, and a session resumed from
// another directory (a repository inside the workspace it started in, or the
// other way round) would be split across two state directories, whose halves
// the server would take for the whole session in turn. So the state directory
// that logs a session first owns it, and its later events go there, wherever
// the agent runs: any state directory records work in any repository. An
// explicit log file, or an event without a session, bypasses the index.
// touch marks the session as still in use, so pruneOwners keeps its entry.
func sessionEventsPath(flagVal, sessionID string, touch bool) (string, error) {
	if flagVal != "" || os.Getenv("SWARMIA_LOG_FILE") != "" || sessionID == "" {
		return eventsPath(flagVal)
	}
	base, err := stateBaseDir()
	if err != nil {
		return "", err
	}
	sum := sha256.Sum256([]byte(sessionID))
	entry := filepath.Join(base, ownersDir, hex.EncodeToString(sum[:16]))
	if dir, ok := owner(base, entry); ok {
		if touch {
			now := time.Now()
			os.Chtimes(entry, now, now)
		}
		return filepath.Join(dir, "events.jsonl"), nil
	}
	path, err := eventsPath("")
	if err != nil {
		return "", err
	}
	if dir, ok := claimOwner(base, entry, filepath.Dir(path)); ok {
		return filepath.Join(dir, "events.jsonl"), nil
	}
	return path, nil // the index is best-effort; log here rather than not at all
}

// owner reads the state directory an owner index entry names, if it still
// exists.
func owner(base, entry string) (string, bool) {
	data, err := os.ReadFile(entry)
	if err != nil {
		return "", false
	}
	rel := strings.TrimSpace(string(data))
	if rel == "" || !filepath.IsLocal(rel) {
		return "", false
	}
	dir := filepath.Join(base, rel)
	if fi, err := os.Stat(dir); err != nil || !fi.IsDir() {
		return "", false
	}
	return dir, true
}

// claimOwner records stateDir as the owner of the session at entry and
// returns the session's owner: stateDir, or whichever state directory claimed
// the session concurrently. An entry naming a state directory that is gone is
// replaced.
func claimOwner(base, entry, stateDir string) (string, bool) {
	rel, err := filepath.Rel(base, stateDir)
	if err != nil || !filepath.IsLocal(rel) {
		return "", false
	}
	if err := os.MkdirAll(filepath.Dir(entry), 0o700); err != nil {
		return "", false
	}
	tmp, err := os.CreateTemp(filepath.Dir(entry), ".tmp-*")
	if err != nil {
		return "", false
	}
	defer os.Remove(tmp.Name())
	_, werr := tmp.WriteString(filepath.ToSlash(rel) + "\n")
	if errors.Join(werr, tmp.Close()) != nil {
		return "", false
	}
	// Link, unlike rename, fails if the entry exists: of concurrent first
	// events, one claims the session and the others follow it.
	if err := os.Link(tmp.Name(), entry); err == nil {
		return stateDir, true
	}
	if dir, ok := owner(base, entry); ok {
		return dir, true
	}
	if os.Rename(tmp.Name(), entry) == nil { // a stale entry
		return stateDir, true
	}
	return "", false
}

// pruneOwners deletes owner index entries not used for longer than
// report.Retention: a session's entry is renewed whenever it starts or
// resumes, so by then its session file is gone too.
func pruneOwners(now time.Time) {
	base, err := stateBaseDir()
	if err != nil {
		return
	}
	dir := filepath.Join(base, ownersDir)
	entries, err := os.ReadDir(dir)
	if err != nil {
		return
	}
	for _, e := range entries {
		if fi, err := e.Info(); err == nil && now.Sub(fi.ModTime()) > report.Retention {
			os.Remove(filepath.Join(dir, e.Name()))
		}
	}
}
