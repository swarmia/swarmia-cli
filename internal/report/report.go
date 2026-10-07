// Package report turns the local event log into per-session attribution: which
// lines of which commits came from each agent session. It folds the raw log
// into compact per-session records (see Store) and attributes each session's
// edits to commits. It only reads the log and the repository; nothing is sent
// anywhere.
package report

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/swarmia/swarmia-cli/internal/attribution"
	"github.com/swarmia/swarmia-cli/internal/events"
	"github.com/swarmia/swarmia-cli/internal/usage"
)

// Options configures attribution.
type Options struct {
	// RepoDir is any directory in the repository the log belongs to.
	RepoDir string
	// FallbackRoot resolves file paths in records logged before records carried
	// their worktree root.
	FallbackRoot string
	// AuthorEmail limits candidate commits to this author; "" considers all. In a
	// busy repository this keeps other people's commits out of the scan.
	AuthorEmail string
}

// Session is one agent session: what happened in it, and which committed lines
// came from it. It is what the Store keeps once the raw events are folded in.
type Session struct {
	ID        string    `json:"id"`
	Agent     string    `json:"agent"`
	Model     string    `json:"model,omitempty"`
	StartedAt time.Time `json:"started_at"`
	EndedAt   time.Time `json:"ended_at"`
	Prompts   []Prompt  `json:"prompts,omitempty"`
	Tools     []Tool    `json:"tools,omitempty"`
	Edits     []Edit    `json:"edits,omitempty"`
	// AILinesByPath counts, per file, the non-blank lines the agent wrote that
	// were still there after its last edit to the file — lines it wrote and then
	// replaced itself don't count.
	AILinesByPath map[string]int `json:"ai_lines_by_path,omitempty"`
	Attribution   *Attribution   `json:"attribution,omitempty"`
	// Ended is set once the agent reported the session's end.
	Ended bool `json:"ended,omitempty"`
	// TranscriptPath is the agent's transcript of the session (from its hook
	// payloads); Usage holds the token counts read from it.
	TranscriptPath string         `json:"transcript_path,omitempty"`
	Usage          *usage.Tracker `json:"usage,omitempty"`
	// Delivery records the last payload sent to Swarmia for this session.
	Delivery *Delivery `json:"delivery,omitempty"`
}

// Delivery is the outcome of the last delivery attempt that reached the
// server for a session. Hash identifies the payload, so an unchanged session
// isn't sent again; Rejected holds the server's reason if it refused it.
type Delivery struct {
	Hash     string    `json:"hash"`
	SentAt   time.Time `json:"sent_at"`
	Rejected string    `json:"rejected,omitempty"`
}

type Prompt struct {
	At   time.Time `json:"at"`
	Text string    `json:"text"`
}

// Tool is one tool call. File and Command are set when the tool has them.
type Tool struct {
	At      time.Time `json:"at"`
	Name    string    `json:"name"`
	File    string    `json:"file,omitempty"`
	Command string    `json:"command,omitempty"`
}

// Edit is one change the agent made to a file: hunks in diff-style lines (see
// attribution.Hunk.Encode). ViaShell marks changes a shell command made, found
// by snapshotting the worktree around it.
type Edit struct {
	At       time.Time  `json:"at"`
	Path     string     `json:"path"`
	Hunks    [][]string `json:"hunks"`
	ViaShell bool       `json:"via_shell,omitempty"`
}

// Attribution is the result of matching a session's edits to commits. Key
// identifies the inputs it was computed from, so it is only recomputed when
// those change (new edits, new or rewritten commits, a new matcher).
type Attribution struct {
	Key        string    `json:"key"`
	ComputedAt time.Time `json:"computed_at"`
	Commits    []Commit  `json:"commits"`
}

// Commit is a commit containing lines the session wrote.
type Commit struct {
	SHA     string    `json:"sha"`
	At      time.Time `json:"at"`
	Subject string    `json:"subject"`
	Pushed  bool      `json:"pushed"`
	Files   []File    `json:"files"`
}

// File lists the attributed lines of one path in a commit: only lines that
// commit itself added, each with its best class across the session's hunks.
type File struct {
	Path  string  `json:"path"`
	Lines []Range `json:"lines"`
}

// Range is a run of committed lines [Start, End] sharing a class.
type Range struct {
	Start int               `json:"start"`
	End   int               `json:"end"`
	Class attribution.Class `json:"class"`
}

// Count returns how many attributed lines have the given class.
func (f File) Count(c attribution.Class) int {
	n := 0
	for _, r := range f.Lines {
		if r.Class == c {
			n += r.End - r.Start + 1
		}
	}
	return n
}

// Total returns how many lines are attributed.
func (f File) Total() int {
	n := 0
	for _, r := range f.Lines {
		n += r.End - r.Start + 1
	}
	return n
}

// Survived counts the session's lines in the commit that are exact or only
// reformatted.
func (c Commit) Survived() int {
	n := 0
	for _, f := range c.Files {
		n += f.Count(attribution.Exact) + f.Count(attribution.Normalized)
	}
	return n
}

// AILines is the session's total of AILinesByPath.
func (s *Session) AILines() int {
	n := 0
	for _, c := range s.AILinesByPath {
		n += c
	}
	return n
}

// ShellEdits counts edits made by shell commands.
func (s *Session) ShellEdits() int {
	n := 0
	for _, e := range s.Edits {
		if e.ViaShell {
			n++
		}
	}
	return n
}

// Commits returns the attributed commits, oldest first (none if not yet
// attributed).
func (s *Session) Commits() []Commit {
	if s.Attribution == nil {
		return nil
	}
	return s.Attribution.Commits
}

// clockSlack tolerates second-granularity timestamps when ordering edits and
// commits made in quick succession.
const clockSlack = 2 * time.Second

// Build folds the records into fresh sessions and attributes them, newest
// first. It is the whole pipeline in memory, without a Store.
func Build(recs []events.Record, opts Options) ([]*Session, error) {
	sessions := map[string]*Session{}
	Fold(sessions, recs, opts.FallbackRoot)
	list := sortedSessions(sessions)
	UpdateUsage(list)
	if err := Attribute(list, opts, time.Now()); err != nil {
		return nil, err
	}
	return list, nil
}

func sortedSessions(m map[string]*Session) []*Session {
	list := make([]*Session, 0, len(m))
	for _, s := range m {
		list = append(list, s)
	}
	sort.Slice(list, func(a, b int) bool { return list[a].StartedAt.After(list[b].StartedAt) })
	return list
}

// hookPayload is the subset of Claude Code hook fields folding reads.
type hookPayload struct {
	HookEventName  string `json:"hook_event_name"`
	SessionID      string `json:"session_id"`
	TranscriptPath string `json:"transcript_path"`
	Model          string `json:"model"`
	Prompt         string `json:"prompt"`
	ToolName       string `json:"tool_name"`
	ToolInput      struct {
		FilePath string `json:"file_path"`
		Command  string `json:"command"`
	} `json:"tool_input"`
}

// Fold merges records into sessions (keyed by id), creating sessions as
// needed. Records identical to one already seen in the same call are skipped:
// that's an event logged twice by overlapping hooks.
func Fold(sessions map[string]*Session, recs []events.Record, fallbackRoot string) {
	seen := map[string]bool{}
	for _, rec := range recs {
		var p hookPayload
		if json.Unmarshal(rec.Event, &p) != nil || p.SessionID == "" {
			continue
		}
		at, err := time.Parse(time.RFC3339, rec.LoggedAt)
		if err != nil {
			continue
		}
		if key := recordKey(rec); seen[key] {
			continue
		} else {
			seen[key] = true
		}
		s, ok := sessions[p.SessionID]
		if !ok {
			s = &Session{ID: p.SessionID, Agent: "claude-code", StartedAt: at, AILinesByPath: map[string]int{}}
			sessions[p.SessionID] = s
		}
		if s.AILinesByPath == nil {
			s.AILinesByPath = map[string]int{}
		}
		if at.Before(s.StartedAt) {
			s.StartedAt = at
		}
		if at.After(s.EndedAt) {
			s.EndedAt = at
		}
		if p.TranscriptPath != "" {
			s.TranscriptPath = p.TranscriptPath
		}
		switch p.HookEventName {
		case "SessionStart":
			if p.Model != "" {
				s.Model = p.Model
			}
			s.Ended = false // resumed
		case "SessionEnd":
			s.Ended = true
		case "UserPromptSubmit":
			s.Prompts = append(s.Prompts, Prompt{At: at, Text: p.Prompt})
		case "PostToolUse", "PostToolUseFailure":
			s.Tools = append(s.Tools, Tool{At: at, Name: p.ToolName, File: p.ToolInput.FilePath, Command: p.ToolInput.Command})
			root := fallbackRoot
			if rec.Git != nil && rec.Git.Root != "" {
				root = rec.Git.Root
			}
			if e, ok := attribution.ClaudeEdit(rec.Event, root); ok {
				s.addEdit(Edit{At: at, Path: e.Path, Hunks: encode(e.Hunks)}, e.After, e.AfterKnown)
			}
			for _, fc := range rec.Changes { // what a shell command authored
				s.addEdit(Edit{At: at, Path: fc.Path, Hunks: fc.Hunks, ViaShell: true}, "", false)
			}
		}
	}
}

func recordKey(rec events.Record) string {
	h := sha256.New()
	h.Write([]byte(rec.LoggedAt))
	h.Write(rec.Event)
	if len(rec.Changes) > 0 {
		b, _ := json.Marshal(rec.Changes)
		h.Write(b)
	}
	return hex.EncodeToString(h.Sum(nil))
}

func encode(hunks []attribution.Hunk) [][]string {
	out := make([][]string, len(hunks))
	for i, h := range hunks {
		out[i] = h.Encode()
	}
	return out
}

// addEdit records an edit and updates the file's AI line count. With the
// file's content after the edit, the count is recomputed: the agent's added
// lines (across all its edits to the file) still present, each occurrence
// used once. Without it (shell changes), the edit's added lines are added on.
func (s *Session) addEdit(e Edit, after string, afterKnown bool) {
	s.Edits = append(s.Edits, e)
	if !afterKnown {
		s.AILinesByPath[e.Path] += len(addedLines(e))
		return
	}
	present := map[string]int{}
	for _, l := range strings.Split(after, "\n") {
		present[l]++
	}
	n := 0
	for _, prev := range s.Edits {
		if prev.Path != e.Path {
			continue
		}
		for _, l := range addedLines(prev) {
			if present[l] > 0 {
				present[l]--
				n++
			}
		}
	}
	s.AILinesByPath[e.Path] = n
}

func addedLines(e Edit) []string {
	var out []string
	for _, h := range e.Hunks {
		for _, l := range h {
			if strings.HasPrefix(l, "+") && strings.TrimSpace(l[1:]) != "" {
				out = append(out, l[1:])
			}
		}
	}
	return out
}

// Attribute (re)computes the attribution of each session that has edits. It
// lists candidate commits once — non-merge commits on any ref since the
// earliest edit, by opts.AuthorEmail if set — and recomputes a session only
// when the commits touching its files, its edits, or the matcher changed.
// Commits rebased or amended away are no longer reachable, so they drop out
// and their replacements are matched. Pushed status is refreshed every time.
func Attribute(sessions []*Session, opts Options, now time.Time) error {
	var earliest time.Time
	for _, s := range sessions {
		for _, e := range s.Edits {
			if earliest.IsZero() || e.At.Before(earliest) {
				earliest = e.At
			}
		}
	}
	if earliest.IsZero() {
		return nil
	}
	r := newRepo(opts.RepoDir)
	commits, err := r.commitsSince(earliest.Add(-clockSlack), opts.AuthorEmail)
	if err != nil {
		return err
	}
	for _, s := range sessions {
		if len(s.Edits) == 0 {
			continue
		}
		candidates := candidatesFor(s, commits)
		key := attributionKey(s, candidates)
		if s.Attribution == nil || s.Attribution.Key != key {
			s.Attribution = &Attribution{Key: key, ComputedAt: now, Commits: attribute(r, s.Edits, candidates)}
		}
		for i := range s.Attribution.Commits {
			s.Attribution.Commits[i].Pushed = r.isPushed(s.Attribution.Commits[i].SHA)
		}
	}
	return nil
}

// UpdateUsage reads new token usage from each session's transcript. It is
// cheap for transcripts that haven't grown (a stat per file). Errors only mean
// the counts lag; they are retried on the next run.
func UpdateUsage(sessions []*Session) {
	for _, s := range sessions {
		if s.TranscriptPath == "" {
			continue
		}
		if s.Usage == nil {
			s.Usage = &usage.Tracker{}
		}
		_ = s.Usage.Update(s.TranscriptPath)
	}
}

// candidatesFor narrows commits to those that touch one of the session's files
// at or after its first edit to that file.
func candidatesFor(s *Session, commits []commit) []commit {
	firstEdit := map[string]time.Time{}
	for _, e := range s.Edits {
		if t, ok := firstEdit[e.Path]; !ok || e.At.Before(t) {
			firstEdit[e.Path] = e.At
		}
	}
	var out []commit
	for _, c := range commits {
		for path, t := range firstEdit {
			if c.Paths[path] && !c.At.Before(t.Add(-clockSlack)) {
				out = append(out, c)
				break
			}
		}
	}
	return out
}

func attributionKey(s *Session, candidates []commit) string {
	h := sha256.New()
	fmt.Fprintf(h, "%s\n%d\n", attribution.Version, len(s.Edits))
	for _, c := range candidates {
		fmt.Fprintln(h, c.SHA)
	}
	return hex.EncodeToString(h.Sum(nil))
}

// rank orders classes from strongest to weakest evidence of the agent's work.
var rank = map[attribution.Class]int{attribution.Exact: 3, attribution.Normalized: 2, attribution.Modified: 1}

// attribute matches the edits' hunks against each candidate commit that
// touched the edited file after the edit, keeping only lines the commit itself
// added. A line claimed by several hunks gets the strongest class.
func attribute(r *repo, edits []Edit, commits []commit) []Commit {
	var out []Commit
	paths := editedPaths(edits)
	for _, c := range commits {
		var files []File
		for _, path := range paths {
			if !c.Paths[path] {
				continue
			}
			content, ok := r.content(c.SHA, path)
			if !ok {
				continue // deleted in this commit
			}
			introduced := r.addedLines(c.SHA, path)
			best := map[int]attribution.Class{}
			for _, e := range edits {
				if e.Path != path || e.At.After(c.At.Add(clockSlack)) {
					continue // a different file, or edited after this commit
				}
				for _, enc := range e.Hunks {
					for _, l := range attribution.MatchHunk(attribution.DecodeHunk(enc), content).Lines {
						if introduced[l.Line] && rank[l.Class] > rank[best[l.Line]] {
							best[l.Line] = l.Class
						}
					}
				}
			}
			if len(best) == 0 {
				continue
			}
			var lines []attribution.LineMatch
			for line, class := range best {
				lines = append(lines, attribution.LineMatch{Line: line, Class: class})
			}
			sort.Slice(lines, func(a, b int) bool { return lines[a].Line < lines[b].Line })
			files = append(files, File{Path: path, Lines: Ranges(lines)})
		}
		if len(files) > 0 {
			out = append(out, Commit{SHA: c.SHA, At: c.At, Subject: c.Subject, Files: files})
		}
	}
	return out
}

func editedPaths(edits []Edit) []string {
	seen := map[string]bool{}
	var paths []string
	for _, e := range edits {
		if !seen[e.Path] {
			seen[e.Path] = true
			paths = append(paths, e.Path)
		}
	}
	sort.Strings(paths)
	return paths
}

// Ranges compresses attributed lines into runs of consecutive lines with the
// same class.
func Ranges(lines []attribution.LineMatch) []Range {
	var out []Range
	for _, l := range lines {
		if n := len(out); n > 0 && out[n-1].Class == l.Class && out[n-1].End == l.Line-1 {
			out[n-1].End = l.Line
			continue
		}
		out = append(out, Range{Start: l.Line, End: l.Line, Class: l.Class})
	}
	return out
}
