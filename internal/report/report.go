// Package report turns the local event log into per-session attribution: which
// lines of which commits came from each agent session. It only reads the log
// and the repository; nothing is sent anywhere.
package report

import (
	"encoding/json"
	"sort"
	"strings"
	"time"

	"github.com/swarmia/swarmia-cli/internal/attribution"
	"github.com/swarmia/swarmia-cli/internal/events"
)

// Options configures Build.
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

// Session is one agent session and what it contributed.
type Session struct {
	ID        string
	Agent     string
	Model     string
	StartedAt time.Time
	EndedAt   time.Time
	Prompts   []Prompt
	Tools     []Tool
	Edits     int
	// ShellEdits is how many of Edits are changes shell commands made, found
	// by snapshotting the worktree around them.
	ShellEdits int
	// AILines counts the non-blank lines the agent wrote that were still in the
	// files when the session's last edit to each file was made — lines it wrote
	// and then replaced itself don't count.
	AILines int
	Commits []Commit
}

type Prompt struct {
	At   time.Time
	Text string
}

// Tool is one tool call. File and Command are set when the tool has them.
type Tool struct {
	At      time.Time
	Name    string
	File    string
	Command string
}

// Commit is a commit containing lines the session wrote.
type Commit struct {
	SHA     string
	At      time.Time
	Subject string
	Pushed  bool
	Files   []File
}

// File lists the attributed lines of one path in a commit: only lines that
// commit itself added, each with its best class across the session's hunks.
type File struct {
	Path  string
	Lines []attribution.LineMatch
}

// Count returns how many attributed lines have the given class.
func (f File) Count(c attribution.Class) int {
	n := 0
	for _, l := range f.Lines {
		if l.Class == c {
			n++
		}
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

// clockSlack tolerates second-granularity timestamps when ordering edits and
// commits made in quick succession.
const clockSlack = 2 * time.Second

type edit struct {
	at time.Time
	attribution.Edit
	viaShell bool // made by a shell command, found by snapshot diff
}

// Build groups the records into sessions, newest first, and attributes each
// session's edits to the commits they landed in.
func Build(recs []events.Record, opts Options) ([]Session, error) {
	sessions, editsBySession := group(recs, opts.FallbackRoot)

	var earliest time.Time
	for _, edits := range editsBySession {
		for _, e := range edits {
			if earliest.IsZero() || e.at.Before(earliest) {
				earliest = e.at
			}
		}
	}
	r := newRepo(opts.RepoDir)
	var commits []commit
	if !earliest.IsZero() {
		var err error
		if commits, err = r.commitsSince(earliest.Add(-clockSlack), opts.AuthorEmail); err != nil {
			return nil, err
		}
	}

	for i := range sessions {
		s := &sessions[i]
		edits := editsBySession[s.ID]
		s.Edits = len(edits)
		for _, e := range edits {
			if e.viaShell {
				s.ShellEdits++
			}
		}
		s.AILines = aiLinesAtEnd(edits)
		s.Commits = attribute(r, edits, commits)
	}
	sort.Slice(sessions, func(a, b int) bool { return sessions[a].StartedAt.After(sessions[b].StartedAt) })
	return sessions, nil
}

// hookPayload is the subset of Claude Code hook fields the report reads.
type hookPayload struct {
	HookEventName string `json:"hook_event_name"`
	SessionID     string `json:"session_id"`
	Model         string `json:"model"`
	Prompt        string `json:"prompt"`
	ToolName      string `json:"tool_name"`
	ToolInput     struct {
		FilePath string `json:"file_path"`
		Command  string `json:"command"`
	} `json:"tool_input"`
}

func group(recs []events.Record, fallbackRoot string) ([]Session, map[string][]edit) {
	byID := map[string]*Session{}
	var order []string
	edits := map[string][]edit{}
	for _, rec := range recs {
		var p hookPayload
		if json.Unmarshal(rec.Event, &p) != nil || p.SessionID == "" {
			continue
		}
		at, err := time.Parse(time.RFC3339, rec.LoggedAt)
		if err != nil {
			continue
		}
		s, ok := byID[p.SessionID]
		if !ok {
			s = &Session{ID: p.SessionID, Agent: "claude-code", StartedAt: at}
			byID[p.SessionID] = s
			order = append(order, p.SessionID)
		}
		if at.Before(s.StartedAt) {
			s.StartedAt = at
		}
		if at.After(s.EndedAt) {
			s.EndedAt = at
		}
		switch p.HookEventName {
		case "SessionStart":
			if p.Model != "" {
				s.Model = p.Model
			}
		case "UserPromptSubmit":
			s.Prompts = append(s.Prompts, Prompt{At: at, Text: p.Prompt})
		case "PostToolUse", "PostToolUseFailure":
			s.Tools = append(s.Tools, Tool{At: at, Name: p.ToolName, File: p.ToolInput.FilePath, Command: p.ToolInput.Command})
			root := fallbackRoot
			if rec.Git != nil && rec.Git.Root != "" {
				root = rec.Git.Root
			}
			if e, ok := attribution.ClaudeEdit(rec.Event, root); ok {
				edits[p.SessionID] = append(edits[p.SessionID], edit{at: at, Edit: e})
			}
			for _, fc := range rec.Changes { // what a shell command authored
				e := attribution.Edit{Path: fc.Path}
				for _, h := range fc.Hunks {
					e.Hunks = append(e.Hunks, attribution.DecodeHunk(h))
				}
				edits[p.SessionID] = append(edits[p.SessionID], edit{at: at, Edit: e, viaShell: true})
			}
		}
	}
	sessions := make([]Session, 0, len(order))
	for _, id := range order {
		sessions = append(sessions, *byID[id])
	}
	return sessions, edits
}

// aiLinesAtEnd counts, per file, the agent's added lines still present in the
// file after the session's last edit to it (matched by text, each occurrence
// used once). Without a known final state it counts every added line.
func aiLinesAtEnd(edits []edit) int {
	byPath := map[string][]edit{}
	for _, e := range edits {
		byPath[e.Path] = append(byPath[e.Path], e)
	}
	total := 0
	for _, es := range byPath {
		var added []string
		for _, e := range es {
			for _, h := range e.Hunks {
				for _, l := range h.Lines {
					if l.Added && strings.TrimSpace(l.Text) != "" {
						added = append(added, l.Text)
					}
				}
			}
		}
		last := es[len(es)-1]
		if !last.AfterKnown {
			total += len(added)
			continue
		}
		present := map[string]int{}
		for _, l := range strings.Split(last.After, "\n") {
			present[l]++
		}
		for _, l := range added {
			if present[l] > 0 {
				present[l]--
				total++
			}
		}
	}
	return total
}

// rank orders classes from strongest to weakest evidence of the agent's work.
var rank = map[attribution.Class]int{attribution.Exact: 3, attribution.Normalized: 2, attribution.Modified: 1}

// attribute matches the session's hunks against every candidate commit that
// touched the edited file after the edit, keeping only lines the commit itself
// added. A line claimed by several hunks gets the strongest class.
func attribute(r *repo, edits []edit, commits []commit) []Commit {
	var out []Commit
	for _, c := range commits {
		var files []File
		for _, path := range sortedPaths(edits) {
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
				if e.Path != path || e.at.After(c.At.Add(clockSlack)) {
					continue // a different file, or edited after this commit
				}
				for _, h := range e.Hunks {
					for _, l := range attribution.MatchHunk(h, content).Lines {
						if introduced[l.Line] && rank[l.Class] > rank[best[l.Line]] {
							best[l.Line] = l.Class
						}
					}
				}
			}
			if len(best) == 0 {
				continue
			}
			f := File{Path: path}
			for line, class := range best {
				f.Lines = append(f.Lines, attribution.LineMatch{Line: line, Class: class})
			}
			sort.Slice(f.Lines, func(a, b int) bool { return f.Lines[a].Line < f.Lines[b].Line })
			files = append(files, f)
		}
		if len(files) > 0 {
			out = append(out, Commit{SHA: c.SHA, At: c.At, Subject: c.Subject, Pushed: r.isPushed(c.SHA), Files: files})
		}
	}
	return out
}

func sortedPaths(edits []edit) []string {
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
// same class, as in the delivery payload.
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

// Range is a run of committed lines [Start, End] sharing a class.
type Range struct {
	Start int               `json:"start"`
	End   int               `json:"end"`
	Class attribution.Class `json:"class"`
}
