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
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/swarmia/swarmia-cli/internal/agent"
	"github.com/swarmia/swarmia-cli/internal/attribution"
	"github.com/swarmia/swarmia-cli/internal/events"
	"github.com/swarmia/swarmia-cli/internal/gitrepo"
	"github.com/swarmia/swarmia-cli/internal/usage"
)

// Options configures attribution.
type Options struct {
	// FallbackRoot is the worktree of edits and shell changes from before they
	// recorded their own: records without a git root, and sessions stored in
	// format 2 or earlier. "" when the project isn't in a repository.
	FallbackRoot string
	// AuthorEmail, if set, gives the user's email in a worktree. It limits each
	// repository's candidate commits to the user's own: those authored with
	// that email, plus those the reflog shows were created on this machine
	// (covering other emails). "" considers all. In a busy repository this
	// keeps other people's commits out of the scan.
	AuthorEmail func(root string) string
}

// Session is one agent session: what happened in it, and which committed lines
// came from it. It is what the Store keeps once the raw events are folded in.
type Session struct {
	// Format is the session file's format (see SessionFormat); 0 in files
	// from before the field, which are format 1.
	Format int    `json:"format,omitempty"`
	ID     string `json:"id"`
	Agent  string `json:"agent"`
	// Root is the worktree the session was started in, if it was started in
	// one.
	Root      string    `json:"root,omitempty"`
	Model     string    `json:"model,omitempty"`
	StartedAt time.Time `json:"started_at"`
	EndedAt   time.Time `json:"ended_at"`
	Prompts   []Prompt  `json:"prompts,omitempty"`
	Tools     []Tool    `json:"tools,omitempty"`
	Edits     []Edit    `json:"edits,omitempty"`
	// AILinesByRoot counts, per worktree root and file, the non-blank lines the
	// agent wrote that were still there after its last edit to the file — lines
	// it wrote and then replaced itself don't count.
	AILinesByRoot map[string]map[string]int `json:"ai_lines_by_root,omitempty"`
	// AILinesByPath is AILinesByRoot in format 2 and earlier, for the
	// project's worktree alone (see upgrade).
	AILinesByPath map[string]int `json:"ai_lines_by_path,omitempty"`
	// ShellUnrecorded counts shell commands whose changes weren't recorded:
	// snapshotting the worktree around them ran out of time or failed, or the
	// command left a git operation stopped on an unknown commit.
	ShellUnrecorded int          `json:"shell_unrecorded,omitempty"`
	Attribution     *Attribution `json:"attribution,omitempty"`
	// Ended is set once the agent reported the session's end.
	Ended bool `json:"ended,omitempty"`
	// TranscriptPath is the agent's transcript of the session (from its hook
	// payloads); Usage holds the token counts read from it.
	TranscriptPath string         `json:"transcript_path,omitempty"`
	Usage          *usage.Tracker `json:"usage,omitempty"`
	// Deliveries records the last payload sent to Swarmia for this session,
	// per delivery target (see payload.Target): each gets the part of the
	// session in its repositories.
	Deliveries map[string]*Delivery `json:"deliveries,omitempty"`
}

// Delivery is the outcome of the last delivery attempt that reached the
// server for a session's part. Hash identifies the payload, so an unchanged
// part isn't sent again; Rejected holds the server's reason if it refused it.
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
// Root is the worktree it worked in: its file's, else its working
// directory's; "" if neither is in a repository.
type Tool struct {
	At      time.Time `json:"at"`
	Name    string    `json:"name"`
	Root    string    `json:"root,omitempty"`
	File    string    `json:"file,omitempty"`
	Command string    `json:"command,omitempty"`
	// Status and ExitCode are as in agent.Tool.
	Status   string `json:"status,omitempty"`
	ExitCode *int   `json:"exit_code,omitempty"`
}

// Edit is one change the agent made to a file: the worktree it is in, its
// path there, and hunks in diff-style lines (see attribution.Hunk.Encode).
// ViaShell marks changes a shell command made, found by snapshotting the
// worktree around it.
type Edit struct {
	At       time.Time  `json:"at"`
	Root     string     `json:"root,omitempty"`
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

// Commit is a commit containing lines the session wrote, in the repository of
// the worktree Root (any of its worktrees, if the session edited several).
// Kept lists the
// session's added lines (numbered by lineIDs) that the commit holds exact or
// only reformatted, so a line carried by several commits — e.g. a commit and
// its rebased copy — can be counted once (see Session.Survived).
type Commit struct {
	Root    string    `json:"root,omitempty"`
	SHA     string    `json:"sha"`
	At      time.Time `json:"at"`
	Subject string    `json:"subject"`
	Pushed  bool      `json:"pushed"`
	Files   []File    `json:"files"`
	Kept    []Span    `json:"kept,omitempty"`
}

// Span is a run of numbers [Start, End].
type Span struct {
	Start int `json:"start"`
	End   int `json:"end"`
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

// Survived counts the session's added lines the commit holds exact or only
// reformatted.
func (c Commit) Survived() int {
	n := 0
	for _, sp := range c.Kept {
		n += sp.End - sp.Start + 1
	}
	return n
}

// Survived counts the session's added lines that some commit accepted by
// include holds exact or only reformatted. Each line counts once, however many
// commits hold it.
func (s *Session) Survived(include func(Commit) bool) int {
	seen := map[int]bool{}
	for _, c := range s.Commits() {
		if !include(c) {
			continue
		}
		for _, sp := range c.Kept {
			for id := sp.Start; id <= sp.End; id++ {
				seen[id] = true
			}
		}
	}
	return len(seen)
}

// AILines is the session's total of AILinesByRoot.
func (s *Session) AILines() int {
	n := 0
	for root := range s.AILinesByRoot {
		n += s.AILinesIn(root)
	}
	return n
}

// AILinesIn is the session's total of AILinesByRoot in one worktree.
func (s *Session) AILinesIn(root string) int {
	n := 0
	for _, c := range s.AILinesByRoot[root] {
		n += c
	}
	return n
}

// upgrade brings a session stored in format 2 or earlier up to date: it was
// started in the project's worktree, root, and its edits, commits and AI
// lines were all there, as were its tool calls without a file elsewhere.
func (s *Session) upgrade(root string) {
	if s.Format >= 3 || root == "" {
		return
	}
	s.Root = root
	for i, t := range s.Tools {
		if t.File == "" || insideDir(root, t.File) {
			s.Tools[i].Root = root
		}
	}
	for i := range s.Edits {
		if s.Edits[i].Root == "" {
			s.Edits[i].Root = root
		}
	}
	if s.Attribution != nil {
		for i := range s.Attribution.Commits {
			if s.Attribution.Commits[i].Root == "" {
				s.Attribution.Commits[i].Root = root
			}
		}
	}
	if len(s.AILinesByPath) > 0 {
		if s.AILinesByRoot == nil {
			s.AILinesByRoot = map[string]map[string]int{}
		}
		s.AILinesByRoot[root] = s.AILinesByPath
		s.AILinesByPath = nil
	}
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

// Fold merges records into sessions (keyed by id), creating sessions as
// needed. Records identical to one already seen in the same call are skipped:
// that's an event logged twice by overlapping hooks.
//
// An edit tool's file is placed in the worktree it is in, whichever repository
// that is; a shell command's changes, in the worktree they were snapshotted
// in (their own, else the record's, else fallbackRoot).
func Fold(sessions map[string]*Session, recs []events.Record, fallbackRoot string) {
	seen := map[string]bool{}
	loc := newLocator()
	for _, rec := range recs {
		ag := agent.OfRecord(rec.Agent)
		if ag == nil {
			continue // from a newer swarmia-cli
		}
		ev, ok := ag.Parse(rec.Event)
		if !ok || ev.SessionID == "" {
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
		s, ok := sessions[ev.SessionID]
		if !ok {
			s = &Session{ID: ev.SessionID, Agent: ag.Name, StartedAt: at, AILinesByRoot: map[string]map[string]int{}}
			sessions[ev.SessionID] = s
		}
		if s.AILinesByRoot == nil {
			s.AILinesByRoot = map[string]map[string]int{}
		}
		if at.Before(s.StartedAt) {
			s.StartedAt = at
		}
		if at.After(s.EndedAt) {
			s.EndedAt = at
		}
		if ev.TranscriptPath != "" {
			s.TranscriptPath = ev.TranscriptPath
		}
		root := fallbackRoot
		if rec.Git != nil && rec.Git.Root != "" {
			root = rec.Git.Root
		}
		if s.Root == "" {
			s.Root = root
		}
		switch rec.Snapshot {
		case events.SnapshotOverBudget, events.SnapshotFailed, events.SnapshotGitOperation:
			s.ShellUnrecorded++
		}
		switch ev.Name {
		case "SessionStart":
			if ev.Model != "" {
				s.Model = ev.Model
			}
			s.Ended = false // resumed
		case "UserPromptSubmit":
			if !ev.Injected {
				s.Prompts = append(s.Prompts, Prompt{At: at, Text: ev.Prompt})
			}
			if s.Model == "" && ev.Model != "" { // Codex sends the model with every event
				s.Model = ev.Model
			}
		case "SessionEnd":
			s.Ended = true
		case "PostToolUse", "PostToolUseFailure":
			edits := ag.Edits(rec.Event, loc.locate)
			tool := Tool{At: at, Name: ev.Tool.Name, File: ev.Tool.File, Command: ev.Tool.Command,
				Status: ev.Tool.Status, ExitCode: ev.Tool.ExitCode}
			if tool.File == "" && len(edits) > 0 { // e.g. a patch, which names its files inside
				tool.File = filepath.Join(edits[0].Root, filepath.FromSlash(edits[0].Path))
			}
			switch {
			case filepath.IsAbs(tool.File):
				tool.Root, _, _ = loc.locate(filepath.Clean(tool.File))
			case ev.Cwd != "":
				tool.Root = loc.dirRoot(ev.Cwd)
			default:
				tool.Root = root
			}
			s.Tools = append(s.Tools, tool)
			for _, e := range edits {
				s.addEdit(Edit{At: at, Root: e.Root, Path: e.Path, Hunks: encode(e.Hunks)}, e.After, e.AfterKnown)
			}
			for _, fc := range rec.Changes { // what a shell command authored
				r := root
				if fc.Root != "" {
					r = fc.Root
				}
				s.addEdit(Edit{At: at, Root: r, Path: fc.Path, Hunks: fc.Hunks, ViaShell: true}, "", false)
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

// locator places files and directories in the worktree of the repository
// they are in, so edits and tool calls count in any repository, not only the
// project's. Paths are resolved through symlinks first, as worktree roots
// are; the part of a path that no longer exists (a file or directory deleted
// since) is kept as it is.
type locator struct {
	roots map[string]string // resolved directory -> its worktree root, "" for none
}

func newLocator() *locator { return &locator{roots: map[string]string{}} }

// locate is an attribution.Locator.
func (l *locator) locate(path string) (string, string, bool) {
	path = resolveExisting(path)
	return attribution.Within(l.rootOf(filepath.Dir(path)))(path)
}

// dirRoot is the worktree a directory is in, "" if none.
func (l *locator) dirRoot(dir string) string {
	return l.rootOf(resolveExisting(filepath.Clean(dir)))
}

func (l *locator) rootOf(dir string) string {
	root, ok := l.roots[dir]
	if !ok {
		if repo, err := gitrepo.Find(dir); err == nil {
			root = repo.Root
		}
		l.roots[dir] = root
	}
	return root
}

// insideDir reports whether path is dir or below it.
func insideDir(dir, path string) bool {
	rel, err := filepath.Rel(dir, path)
	return err == nil && rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator))
}

// resolveExisting resolves symlinks in the longest part of path that exists.
func resolveExisting(path string) string {
	rest := ""
	for dir := path; ; dir = filepath.Dir(dir) {
		if resolved, err := filepath.EvalSymlinks(dir); err == nil {
			return filepath.Join(resolved, rest)
		}
		if parent := filepath.Dir(dir); parent == dir {
			return path
		}
		rest = filepath.Join(filepath.Base(dir), rest)
	}
}

// addEdit records an edit and updates the file's AI line count. With the
// file's content after the edit, the count is recomputed: the agent's added
// lines (across all its edits to the file) still present, each occurrence
// used once. Without it (shell changes), the edit's added lines are added on.
func (s *Session) addEdit(e Edit, after string, afterKnown bool) {
	s.Edits = append(s.Edits, e)
	byPath := s.AILinesByRoot[e.Root]
	if byPath == nil {
		byPath = map[string]int{}
		s.AILinesByRoot[e.Root] = byPath
	}
	if !afterKnown {
		byPath[e.Path] += len(addedLines(e))
		return
	}
	present := map[string]int{}
	for _, l := range strings.Split(after, "\n") {
		present[l]++
	}
	n := 0
	for _, prev := range s.Edits {
		if prev.Root != e.Root || prev.Path != e.Path {
			continue
		}
		for _, l := range addedLines(prev) {
			if present[l] > 0 {
				present[l]--
				n++
			}
		}
	}
	byPath[e.Path] = n
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

// Attribute (re)computes the attribution of each session that has edits, in
// every repository it edited. For each repository it lists candidate commits
// once — non-merge commits on any ref since the earliest edit there, the
// user's own if opts.AuthorEmail is set — and recomputes a session only when
// the commits touching its files, its edits, or the matcher changed. Commits
// rebased or amended away are no longer reachable, so they drop out and their
// replacements are matched. Pushed status is refreshed every time.
//
// A repository that can't be read (its worktrees were removed) keeps the
// commits it had.
func Attribute(sessions []*Session, opts Options, now time.Time) error {
	repos := map[string]*repoCommits{} // by repository (see repoKeyOf)
	keyOf := map[string]string{}       // worktree root -> repository
	for _, s := range sessions {
		for _, e := range s.Edits {
			if e.Root == "" {
				continue // from a session stored outside any repository
			}
			key, ok := keyOf[e.Root]
			if !ok {
				key = repoKeyOf(e.Root)
				keyOf[e.Root] = key
			}
			rc := repos[key]
			if rc == nil {
				rc = &repoCommits{root: e.Root}
				repos[key] = rc
			}
			if rc.earliest.IsZero() || e.At.Before(rc.earliest) {
				rc.earliest = e.At
			}
		}
	}
	for _, rc := range repos {
		rc.load(opts)
	}
	for _, s := range sessions {
		if len(s.Edits) == 0 {
			continue
		}
		edited := map[string]bool{}
		for _, e := range s.Edits {
			if e.Root != "" {
				edited[keyOf[e.Root]] = true
			}
		}
		candidates := map[string][]commit{}
		for k := range edited {
			if repos[k].ok {
				candidates[k] = candidatesFor(s.Edits, repos[k].commits, func(e Edit) bool { return keyOf[e.Root] == k })
			}
		}
		key := attributionKey(s, edited, repos, candidates)
		if s.Attribution == nil || s.Attribution.Key != key {
			var commits []Commit
			for _, c := range s.Commits() { // what repositories that can't be read had
				if rc := repos[keyOf[c.Root]]; rc == nil || !rc.ok {
					commits = append(commits, c)
				}
			}
			for k, cs := range candidates {
				rc := repos[k]
				for _, c := range attribute(rc.r, s.Edits, cs, func(e Edit) bool { return keyOf[e.Root] == k }) {
					c.Root = rc.root
					commits = append(commits, c)
				}
			}
			sort.SliceStable(commits, func(a, b int) bool {
				if !commits[a].At.Equal(commits[b].At) {
					return commits[a].At.Before(commits[b].At)
				}
				return commits[a].Root < commits[b].Root
			})
			s.Attribution = &Attribution{Key: key, ComputedAt: now, Commits: commits}
		}
		for i, c := range s.Attribution.Commits {
			if rc := repos[keyOf[c.Root]]; rc != nil && rc.ok {
				s.Attribution.Commits[i].Pushed = rc.r.isPushed(c.SHA)
			}
		}
	}
	return nil
}

// repoCommits is one repository's candidate commits for Attribute.
type repoCommits struct {
	root     string    // a worktree of it, where git runs
	earliest time.Time // the earliest edit in it, across sessions
	r        *repo
	commits  []commit
	ok       bool // false if its commits couldn't be listed
}

func (rc *repoCommits) load(opts Options) {
	rc.r = newRepo(rc.root)
	since := rc.earliest.Add(-clockSlack)
	commits, err := rc.r.commitsSince(since)
	if err != nil {
		return
	}
	if opts.AuthorEmail != nil {
		if email := opts.AuthorEmail(rc.root); email != "" {
			commits = ownCommits(commits, rc.root, email, since)
		}
	}
	rc.commits, rc.ok = commits, true
}

// repoKeyOf identifies the repository of a worktree, so the worktrees of one
// clone share their commits: its git common dir, or the root itself if it
// isn't a repository (any longer).
func repoKeyOf(root string) string {
	if repo, err := gitrepo.Find(root); err == nil && repo.Root == root {
		return repo.CommonDir
	}
	return root
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
		if ag := agent.Lookup(s.Agent); ag != nil {
			_ = s.Usage.Update(ag.Transcript, s.TranscriptPath)
		}
	}
}

// ownCommits keeps the commits that are the user's: authored with their
// email, or created on this machine according to the reflog — which also
// covers commits made under another email, rebased picks and cherry-picks.
// Reachability (commitsSince lists only commits reachable from refs) already
// drops commits that were amended or rebased away.
func ownCommits(commits []commit, root, email string, since time.Time) []commit {
	created := map[string]bool{}
	if repo, err := gitrepo.Find(root); err == nil {
		created, _ = repo.LocallyCreatedCommits(since)
	}
	var out []commit
	for _, c := range commits {
		if strings.EqualFold(c.AuthorEmail, email) || created[c.SHA] {
			out = append(out, c)
		}
	}
	return out
}

// candidatesFor narrows commits to those that touch the file of an edit in
// the repository (those include accepts) at or after its first edit there.
func candidatesFor(edits []Edit, commits []commit, include func(Edit) bool) []commit {
	firstEdit := map[string]time.Time{}
	for _, e := range edits {
		if !include(e) {
			continue
		}
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

// attributionKey identifies a session's attribution inputs: the matcher, its
// edits, and each repository's candidate commits (or that it couldn't be
// read).
func attributionKey(s *Session, edited map[string]bool, repos map[string]*repoCommits, candidates map[string][]commit) string {
	h := sha256.New()
	fmt.Fprintf(h, "%s\n%d\n", attribution.Version, len(s.Edits))
	keys := make([]string, 0, len(edited))
	for k := range edited {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, k := range keys {
		fmt.Fprintf(h, "repo %s %t\n", k, repos[k].ok)
		for _, c := range candidates[k] {
			fmt.Fprintln(h, c.SHA)
		}
	}
	return hex.EncodeToString(h.Sum(nil))
}

// rank orders classes from strongest to weakest evidence of the agent's work.
var rank = map[attribution.Class]int{attribution.Exact: 3, attribution.Normalized: 2, attribution.Modified: 1}

// attribute matches the hunks of the edits in r (those include accepts)
// against each candidate commit that touched the edited file after the edit,
// keeping only lines the commit itself added. A line claimed by several hunks
// gets the strongest class, and is credited to that hunk's added lines alone.
// Line ids number the lines of all the session's edits (see lineIDs), so they
// are unique across repositories.
func attribute(r *repo, edits []Edit, commits []commit, include func(Edit) bool) []Commit {
	var out []Commit
	paths := editedPaths(edits, include)
	ids := lineIDs(edits)
	for _, c := range commits {
		var files []File
		var kept []int
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
			from := map[int][]int{} // committed line -> ids of the added lines it came from
			for ei, e := range edits {
				if !include(e) || e.Path != path || e.At.After(c.At.Add(clockSlack)) {
					continue // a different file, or edited after this commit
				}
				for hi, enc := range e.Hunks {
					for _, l := range attribution.MatchHunk(attribution.DecodeHunk(enc), content).Lines {
						if introduced[l.Line] && rank[l.Class] > rank[best[l.Line]] {
							best[l.Line] = l.Class
							from[l.Line] = nil
							for _, i := range l.From {
								from[l.Line] = append(from[l.Line], ids[ei][hi]+i)
							}
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
				if class != attribution.Modified {
					kept = append(kept, from[line]...)
				}
			}
			sort.Slice(lines, func(a, b int) bool { return lines[a].Line < lines[b].Line })
			files = append(files, File{Path: path, Lines: Ranges(lines)})
		}
		if len(files) > 0 {
			out = append(out, Commit{SHA: c.SHA, At: c.At, Subject: c.Subject, Files: files, Kept: spans(kept)})
		}
	}
	return out
}

// lineIDs numbers the lines of the edits' hunks across the session:
// ids[e][h] + i is line i of hunk h of edit e. Numbering by encoded lines
// (which DecodeHunk only ever drops) keeps the ids unique.
func lineIDs(edits []Edit) [][]int {
	ids := make([][]int, len(edits))
	next := 0
	for ei, e := range edits {
		ids[ei] = make([]int, len(e.Hunks))
		for hi, enc := range e.Hunks {
			ids[ei][hi] = next
			next += len(enc)
		}
	}
	return ids
}

// spans sorts and deduplicates ids into runs.
func spans(ids []int) []Span {
	sort.Ints(ids)
	var out []Span
	for _, id := range ids {
		if n := len(out); n > 0 && id <= out[n-1].End+1 {
			out[n-1].End = max(out[n-1].End, id)
			continue
		}
		out = append(out, Span{Start: id, End: id})
	}
	return out
}

func editedPaths(edits []Edit, include func(Edit) bool) []string {
	seen := map[string]bool{}
	var paths []string
	for _, e := range edits {
		if include(e) && !seen[e.Path] {
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
