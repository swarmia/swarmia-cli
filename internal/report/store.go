package report

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/swarmia/swarmia-cli/internal/events"
	"github.com/swarmia/swarmia-cli/internal/fslock"
)

// Retention is how long a session is kept after its last activity.
const Retention = 30 * 24 * time.Hour

// SessionFormat is the version of the session file format this binary writes
// and reads. Binaries of different versions share a repository's state (every
// worktree does, and each may have its own swarmia-cli), so bump it when an
// older binary would lose or misread something in a new session file: older
// binaries then leave such sessions, and their events, alone.
const SessionFormat = 1

// Store keeps one JSON file per session under Dir. Files are owner-only: they
// hold prompts and the agent's code.
type Store struct {
	Dir string
}

func (st Store) path(id string) string {
	return filepath.Join(st.Dir, safeFileName(id)+".json")
}

// List loads every stored session, newest first. Unreadable files are skipped.
// Sessions stored by a newer swarmia-cli, in a format this one doesn't know,
// aren't loaded but listed in newer by id, to be left alone.
func (st Store) List() (sessions []*Session, newer map[string]bool, err error) {
	entries, err := os.ReadDir(st.Dir)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil, nil
		}
		return nil, nil, err
	}
	m := map[string]*Session{}
	newer = map[string]bool{}
	for _, e := range entries {
		if !strings.HasSuffix(e.Name(), ".json") {
			continue
		}
		data, err := os.ReadFile(filepath.Join(st.Dir, e.Name()))
		if err != nil {
			continue
		}
		var s Session
		if json.Unmarshal(data, &s) != nil || s.ID == "" {
			continue
		}
		if s.Format > SessionFormat {
			newer[s.ID] = true
			continue
		}
		m[s.ID] = &s
	}
	return sortedSessions(m), newer, nil
}

// Save writes sessions atomically (temp file + rename), in SessionFormat.
func (st Store) Save(sessions ...*Session) error {
	if err := os.MkdirAll(st.Dir, 0o700); err != nil {
		return err
	}
	for _, s := range sessions {
		s.Format = SessionFormat
		data, err := json.Marshal(s)
		if err != nil {
			return err
		}
		tmp, err := os.CreateTemp(st.Dir, ".tmp-*")
		if err != nil {
			return err
		}
		_, werr := tmp.Write(data)
		cerr := tmp.Close()
		if err := errors.Join(werr, cerr); err != nil {
			os.Remove(tmp.Name())
			return err
		}
		if err := os.Rename(tmp.Name(), st.path(s.ID)); err != nil {
			return err
		}
	}
	return nil
}

// Prune deletes sessions whose last activity is older than Retention and
// returns how many it deleted.
func (st Store) Prune(sessions []*Session, now time.Time) ([]*Session, int) {
	var kept []*Session
	pruned := 0
	for _, s := range sessions {
		if now.Sub(s.EndedAt) > Retention {
			if os.Remove(st.path(s.ID)) == nil {
				pruned++
				continue
			}
		}
		kept = append(kept, s)
	}
	return kept, pruned
}

// ProcessOptions configures Process.
type ProcessOptions struct {
	Options
	LogPath  string // the raw event log to fold in
	StateDir string // where sessions/ and the process lock live
	// Wait makes Process wait for a concurrent run to finish; otherwise it
	// returns ErrBusy at once (what background runs want).
	Wait bool
	Now  time.Time
	// AfterAttribute, if set, runs on the up-to-date sessions while the lock
	// is still held, before they are saved — delivery hooks in here.
	AfterAttribute func([]*Session) error
}

// ErrBusy means another Process run holds the lock.
var ErrBusy = errors.New("another swarmia-cli process run is in progress")

// ProcessResult summarizes a Process run.
type ProcessResult struct {
	Folded   int // records folded in from the log
	Sessions []*Session
	Pruned   int
}

// Process brings the store up to date: it folds the raw log into session files
// and empties the log, attributes sessions to commits, and prunes sessions
// past retention. Only one run at a time proceeds (see Wait).
func Process(opts ProcessOptions) (ProcessResult, error) {
	var res ProcessResult
	unlock, err := processLock(opts.StateDir, opts.Wait)
	if err != nil {
		return res, err
	}
	defer unlock()

	st := Store{Dir: filepath.Join(opts.StateDir, "sessions")}
	sessions, newer, err := st.List()
	if err != nil {
		return res, err
	}
	byID := map[string]*Session{}
	for _, s := range sessions {
		byID[s.ID] = s
	}
	if res.Folded, err = foldLog(opts.LogPath, st, byID, newer, opts.FallbackRoot); err != nil {
		return res, err
	}

	sessions = sortedSessions(byID)
	sessions, res.Pruned = st.Prune(sessions, opts.Now)
	UpdateUsage(sessions)
	if err := Attribute(sessions, opts.Options, opts.Now); err != nil {
		return res, err
	}
	res.Sessions = sessions
	var hookErr error
	if opts.AfterAttribute != nil {
		hookErr = opts.AfterAttribute(sessions)
	}
	return res, errors.Join(st.Save(sessions...), hookErr)
}

// foldLog claims the log's contents, folds them into the sessions and saves
// the affected ones, then empties the log — all under the log's lock, so
// concurrent hook appends land either in this batch or after it. A crash
// between saving and truncating would fold that batch again on the next run
// (records are deduplicated within a batch, not across).
//
// Records a newer swarmia-cli wrote, in a format this one doesn't know, and
// records of sessions stored in a newer format (newer, by id) stay in the log,
// byte for byte, for a newer swarmia-cli to fold.
func foldLog(path string, st Store, byID map[string]*Session, newer map[string]bool, fallbackRoot string) (int, error) {
	f, err := os.OpenFile(path, os.O_RDWR, 0o600)
	if err != nil {
		if os.IsNotExist(err) {
			return 0, nil
		}
		return 0, err
	}
	defer f.Close()
	unlock, err := fslock.Lock(f)
	if err != nil {
		return 0, fmt.Errorf("locking %s: %w", path, err)
	}
	defer unlock()

	data, err := io.ReadAll(f)
	if err != nil {
		return 0, err
	}
	if len(bytes.TrimSpace(data)) == 0 {
		return 0, nil
	}
	var recs []events.Record
	var kept []byte
	touched := map[string]bool{}
	for line := range bytes.Lines(data) {
		rec, ok := events.Parse(line)
		if !ok {
			continue // a torn write
		}
		var p struct {
			SessionID string `json:"session_id"`
		}
		json.Unmarshal(rec.Event, &p)
		if rec.Newer() || newer[p.SessionID] {
			kept = append(kept, line...)
			if !bytes.HasSuffix(line, []byte("\n")) {
				kept = append(kept, '\n')
			}
			continue
		}
		recs = append(recs, rec)
		if p.SessionID != "" {
			touched[p.SessionID] = true
		}
	}
	if len(recs) == 0 {
		return 0, nil // nothing this version can fold; leave the log as it is
	}
	Fold(byID, recs, fallbackRoot)
	var changed []*Session
	for id := range touched {
		if s, ok := byID[id]; ok {
			changed = append(changed, s)
		}
	}
	if err := st.Save(changed...); err != nil {
		return 0, err // the log is left intact; the next run retries
	}
	if err := f.Truncate(0); err != nil {
		return 0, err
	}
	if len(kept) > 0 {
		if _, err := f.WriteAt(kept, 0); err != nil {
			return 0, err
		}
	}
	return len(recs), nil
}

func processLock(stateDir string, wait bool) (func(), error) {
	if err := os.MkdirAll(stateDir, 0o700); err != nil {
		return nil, err
	}
	f, err := os.OpenFile(filepath.Join(stateDir, "process.lock"), os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		return nil, err
	}
	if wait {
		unlock, err := fslock.Lock(f)
		if err != nil {
			f.Close()
			return nil, err
		}
		return func() { unlock(); f.Close() }, nil
	}
	unlock, ok, err := fslock.TryLock(f)
	if err != nil || !ok {
		f.Close()
		if err == nil {
			err = ErrBusy
		}
		return nil, err
	}
	return func() { unlock(); f.Close() }, nil
}

// safeFileName keeps a session id usable as a file name.
func safeFileName(id string) string {
	return strings.Map(func(r rune) rune {
		if r == '-' || r == '_' || r >= '0' && r <= '9' || r >= 'a' && r <= 'z' || r >= 'A' && r <= 'Z' {
			return r
		}
		return '_'
	}, id)
}
