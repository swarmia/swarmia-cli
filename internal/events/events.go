// Package events defines the local event log: one JSON record per line, each
// wrapping a raw agent hook payload. log-hook writes it; report reads it.
package events

import (
	"bufio"
	"encoding/json"
	"io"
	"os"
)

// Format is the version of the record format this binary writes and reads.
// Binaries of different versions share a repository's log (every worktree
// does, and each may have its own swarmia-cli), so bump it when an older
// binary would lose or misread something in a new record: older binaries then
// leave such records in the log, as they are, for a newer one to fold. Every
// format's records must stay JSON objects with a "format" field.
const Format = 1

// Record is one line of the log.
type Record struct {
	// Format is the record's format (see Format); 0 in records from before the
	// field, which are format 1.
	Format   int    `json:"format,omitempty"`
	LoggedAt string `json:"logged_at"`     // RFC 3339, UTC
	Git      *Git   `json:"git,omitempty"` // omitted outside a repo
	// Agent is the coding agent that sent the event (see internal/agent).
	// Records from before the field are all from Claude Code.
	Agent string          `json:"agent,omitempty"`
	Event json.RawMessage `json:"event"` // the hook payload, verbatim
	// Changes holds what a shell command authored in the working tree, on the
	// record of the hook event that ended it (see internal/snapshot).
	Changes []FileChange `json:"changes,omitempty"`
	// Snapshot, on a shell command's records, says why its changes weren't
	// looked for: SnapshotReadOnly, or SnapshotOverBudget / SnapshotFailed
	// when they went unrecorded. Older binaries ignore it, losing only that.
	Snapshot string `json:"snapshot,omitempty"`
}

// Values of Record.Snapshot.
const (
	SnapshotReadOnly   = "read-only"
	SnapshotOverBudget = "over-budget"
	SnapshotFailed     = "failed"
)

// FileChange is one file a shell command changed: hunks in diff-style lines,
// "+" for lines it authored, " " for context.
type FileChange struct {
	Path  string     `json:"path"`
	Hunks [][]string `json:"hunks"`
}

// Git is the project's repository position when the event was logged.
type Git struct {
	Root   string `json:"root,omitempty"`   // worktree top-level directory
	Head   string `json:"head,omitempty"`   // commit sha; empty on an unborn branch
	Branch string `json:"branch,omitempty"` // short name; empty when detached
}

// Newer reports whether the record is in a format newer than this binary's.
func (r Record) Newer() bool { return r.Format > Format }

// ReadFile parses a log file. A missing file is an empty log; lines that don't
// parse (a torn write, a future format) are skipped rather than failing.
func ReadFile(path string) ([]Record, error) {
	f, err := os.Open(path)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, err
	}
	defer f.Close()
	return Read(f)
}

// Read parses log records from r; see ReadFile.
func Read(r io.Reader) ([]Record, error) {
	var recs []Record
	sc := bufio.NewScanner(r)
	sc.Buffer(make([]byte, 0, 64*1024), 256<<20) // Write payloads carry whole files
	for sc.Scan() {
		if rec, ok := Parse(sc.Bytes()); ok {
			recs = append(recs, rec)
		}
	}
	return recs, sc.Err()
}

// Parse parses one line of a log. ok is false if it isn't a record: a blank
// line, or a torn write.
func Parse(line []byte) (rec Record, ok bool) {
	if json.Unmarshal(line, &rec) != nil {
		return Record{}, false
	}
	return rec, len(rec.Event) > 0 || rec.Newer()
}
