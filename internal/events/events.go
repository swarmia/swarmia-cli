// Package events defines the local event log: one JSON record per line, each
// wrapping a raw agent hook payload. log-hook writes it; report reads it.
package events

import (
	"bufio"
	"encoding/json"
	"io"
	"os"
)

// Record is one line of the log.
type Record struct {
	LoggedAt string          `json:"logged_at"`     // RFC 3339, UTC
	Git      *Git            `json:"git,omitempty"` // omitted outside a repo
	Event    json.RawMessage `json:"event"`         // the hook payload, verbatim
}

// Git is the project's repository position when the event was logged.
type Git struct {
	Root   string `json:"root,omitempty"`   // worktree top-level directory
	Head   string `json:"head,omitempty"`   // commit sha; empty on an unborn branch
	Branch string `json:"branch,omitempty"` // short name; empty when detached
}

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
		var rec Record
		if json.Unmarshal(sc.Bytes(), &rec) == nil && len(rec.Event) > 0 {
			recs = append(recs, rec)
		}
	}
	return recs, sc.Err()
}
