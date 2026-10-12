// Package snapshot captures what a shell command run by an agent changed in the
// working tree. Before the command it records the dirty files (copying their
// content into a small content-addressed store); afterwards it diffs every file
// that changed and keeps only the lines the command authored.
//
// It runs inside agent hooks, so it is built to be cheap: one `git status`
// per snapshot, and file contents are only re-read when size or mtime changed
// since the previous snapshot. Each side of a command gets at most Budget, so
// a huge repository delays the agent by that much at worst.
package snapshot

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/swarmia/swarmia-cli/internal/attribution"
	"github.com/swarmia/swarmia-cli/internal/gitrepo"
)

// Budget is the most time Take or Changes may spend; past it they give up
// with ErrBudget and the command's changes go unrecorded.
const Budget = 500 * time.Millisecond

// ErrBudget means a snapshot ran out of its Budget.
var ErrBudget = errors.New("snapshot exceeded its time budget")

// ErrUnknownSource means the command left a git operation stopped part way
// without saying which commit it is applying, so the worktree holds content
// from an unknown commit that must not be credited to the command.
var ErrUnknownSource = errors.New("git operation in progress with no known source commit")

// maxFileSize skips files too large to be hand-written source (bundles,
// fixtures, lockfiles of huge projects); they're never diffed.
const maxFileSize = 1 << 20

// Snapshot is the working tree's state before a command: HEAD plus every dirty
// file (modified, added, untracked or deleted, relative to HEAD). Busy says a
// git operation was already stopped part way (see gitrepo.Operation), and Stash
// is the latest stash entry.
type Snapshot struct {
	Head  string           `json:"head"`
	Files map[string]Entry `json:"files"`
	Busy  bool             `json:"busy,omitempty"`
	Stash string           `json:"stash,omitempty"`
}

// Entry is one dirty file. Blob names its content in the store; it is empty for
// a deleted file, and for a file too large or not a regular file (Skipped).
type Entry struct {
	Size    int64  `json:"size"`
	MtimeNs int64  `json:"mtime_ns"`
	Blob    string `json:"blob,omitempty"`
	Deleted bool   `json:"deleted,omitempty"`
	Skipped bool   `json:"skipped,omitempty"`
}

// Change is what a command authored in one file.
type Change struct {
	Path  string
	Hunks []attribution.Hunk
}

// Store keeps pending snapshots and file contents under Dir.
type Store struct {
	Dir string
}

func (s Store) pendingPath(id string) string {
	return filepath.Join(s.Dir, "pending", safeName(id)+".json")
}
func (s Store) blobPath(sum string) string { return filepath.Join(s.Dir, "blobs", sum) }
func (s Store) lastPath() string           { return filepath.Join(s.Dir, "last.json") }

// Take snapshots the worktree's dirty files.
func (s Store) Take(ctx context.Context, repo gitrepo.Repo) (Snapshot, error) {
	st, err := repo.ReadState()
	if err != nil {
		return Snapshot{}, err
	}
	dirty, err := dirtyPaths(ctx, repo.Root)
	if err != nil {
		return Snapshot{}, err
	}
	last := s.readLast()
	_, busy := repo.InProgress()
	snap := Snapshot{Head: st.Head, Files: map[string]Entry{}, Busy: busy, Stash: repo.Stash()}
	for _, path := range dirty {
		if ctx.Err() != nil {
			return Snapshot{}, ErrBudget
		}
		snap.Files[path] = s.entry(repo.Root, path, last[path])
	}
	s.writeLast(snap.Files)
	return snap, nil
}

// entry stats a dirty file and stores its content, reusing the previous
// snapshot's blob when size and mtime are unchanged.
func (s Store) entry(root, path string, prev Entry) Entry {
	fi, err := os.Lstat(filepath.Join(root, filepath.FromSlash(path)))
	if err != nil {
		return Entry{Deleted: true}
	}
	e := Entry{Size: fi.Size(), MtimeNs: fi.ModTime().UnixNano()}
	if !fi.Mode().IsRegular() || fi.Size() > maxFileSize {
		e.Skipped = true
		return e
	}
	if prev.Blob != "" && prev.Size == e.Size && prev.MtimeNs == e.MtimeNs {
		if _, err := os.Stat(s.blobPath(prev.Blob)); err == nil {
			e.Blob = prev.Blob
			return e
		}
	}
	data, err := os.ReadFile(filepath.Join(root, filepath.FromSlash(path)))
	if err != nil {
		e.Skipped = true
		return e
	}
	sum := sha256.Sum256(data)
	e.Blob = hex.EncodeToString(sum[:])
	if _, err := os.Stat(s.blobPath(e.Blob)); err != nil {
		if writeAtomic(s.blobPath(e.Blob), data) != nil {
			e.Blob, e.Skipped = "", true
		}
	}
	return e
}

// Save stores a snapshot until the command identified by id finishes.
func (s Store) Save(id string, snap Snapshot) error {
	data, err := json.Marshal(snap)
	if err != nil {
		return err
	}
	return writeAtomic(s.pendingPath(id), data)
}

// Load retrieves and removes the snapshot saved for id.
func (s Store) Load(id string) (Snapshot, bool) {
	path := s.pendingPath(id)
	data, err := os.ReadFile(path)
	if err != nil {
		return Snapshot{}, false
	}
	os.Remove(path)
	var snap Snapshot
	if json.Unmarshal(data, &snap) != nil {
		return Snapshot{}, false
	}
	return snap, true
}

// Changes diffs the worktree against the snapshot taken before a command and
// returns the lines the command authored in each file. Files the command
// deleted contribute nothing; lines it only reformatted or restored from git
// are excluded (see attribution.AuthoredOnly). Restored from git includes the
// commits a stopped rebase, cherry-pick, revert or merge is applying, their
// conflict markers, and a stash entry the command popped. If the command left
// such an operation stopped without saying which commit it applies, nothing
// is recorded (ErrUnknownSource): a gap beats crediting someone else's lines.
func (s Store) Changes(ctx context.Context, repo gitrepo.Repo, pre Snapshot) ([]Change, error) {
	st, err := repo.ReadState()
	if err != nil {
		return nil, err
	}
	op, busy := repo.InProgress()
	if busy && len(op.Sources) == 0 && (!pre.Busy || st.Head != pre.Head) {
		return nil, ErrUnknownSource
	}
	sources := op.Sources
	if pre.Stash != "" && repo.Stash() != pre.Stash {
		sources = append(sources, pre.Stash, pre.Stash+"^3") // popped or dropped; ^3 holds untracked files
	}
	dirty, err := dirtyPaths(ctx, repo.Root)
	if err != nil {
		return nil, err
	}
	candidates := map[string]bool{}
	for path := range pre.Files {
		candidates[path] = true
	}
	for _, path := range dirty {
		candidates[path] = true
	}

	var changes []Change
	for _, path := range sortedKeys(candidates) {
		if ctx.Err() != nil {
			return nil, ErrBudget
		}
		full := filepath.Join(repo.Root, filepath.FromSlash(path))
		fi, err := os.Lstat(full)
		if err != nil || !fi.Mode().IsRegular() || fi.Size() > maxFileSize {
			continue // deleted, not a file, or too large
		}
		prev, wasDirty := pre.Files[path]
		if prev.Skipped {
			continue
		}
		if wasDirty && !prev.Deleted && prev.Size == fi.Size() && prev.MtimeNs == fi.ModTime().UnixNano() {
			continue // untouched by the command
		}
		data, err := os.ReadFile(full)
		if err != nil || bytes.IndexByte(data, 0) >= 0 {
			continue // unreadable or binary
		}
		after := string(data)

		preHead, err := gitShow(ctx, repo.Root, pre.Head, path)
		if err != nil {
			return nil, err
		}
		before := preHead
		if wasDirty {
			before = ""
			if !prev.Deleted {
				blob, err := os.ReadFile(s.blobPath(prev.Blob))
				if err != nil {
					continue
				}
				before = string(blob)
			}
		}
		if before == after {
			continue
		}
		refs := []string{before, preHead}
		revs := sources
		if st.Head != pre.Head {
			revs = append([]string{st.Head}, revs...)
		}
		for _, rev := range revs {
			content, err := gitShow(ctx, repo.Root, rev, path)
			if err != nil {
				return nil, err
			}
			refs = append(refs, content)
		}
		if busy {
			refs = append(refs, conflictMarkers(after))
		}
		hunks := attribution.AuthoredOnly(attribution.DiffHunks(before, after), after, refs...)
		if len(hunks) > 0 {
			changes = append(changes, Change{Path: path, Hunks: hunks})
		}
	}
	return changes, nil
}

// conflictMarkers returns the lines of content that are git's conflict
// markers, as a reference version: the command didn't write them, git did.
func conflictMarkers(content string) string {
	var out []string
	for _, l := range strings.Split(content, "\n") {
		for _, m := range []string{"<<<<<<<", "|||||||", "=======", ">>>>>>>"} {
			if rest, ok := strings.CutPrefix(l, m); ok && (rest == "" || rest[0] == ' ') {
				out = append(out, l)
			}
		}
	}
	return strings.Join(out, "\n")
}

// gitShow returns a file's content at rev: "" if it isn't there. The only
// error is running out of the budget, since an empty base for a file that
// does exist would credit the command with all of it.
func gitShow(ctx context.Context, root, rev, path string) (string, error) {
	if rev == "" {
		return "", nil
	}
	out, err := exec.CommandContext(ctx, "git", "-C", root, "show", rev+":"+path).Output()
	if ctx.Err() != nil {
		return "", ErrBudget
	}
	if err != nil {
		return "", nil
	}
	return string(out), nil
}

// Prune removes snapshots of commands that never reported back (the agent was
// interrupted) and blobs nothing has referenced for a while. It is cheap to
// call often: it does nothing if it ran within the last hour.
func (s Store) Prune(now time.Time) {
	marker := filepath.Join(s.Dir, "pruned")
	if fi, err := os.Stat(marker); err == nil && now.Sub(fi.ModTime()) < time.Hour {
		return
	}
	_ = writeAtomic(marker, nil)
	removeOlderThan(filepath.Join(s.Dir, "pending"), now.Add(-24*time.Hour), nil)
	keep := map[string]bool{}
	for _, e := range s.readLast() {
		keep[e.Blob] = true
	}
	removeOlderThan(filepath.Join(s.Dir, "blobs"), now.Add(-7*24*time.Hour), keep)
}

func removeOlderThan(dir string, cutoff time.Time, keep map[string]bool) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return
	}
	for _, de := range entries {
		if keep[de.Name()] {
			continue
		}
		if fi, err := de.Info(); err == nil && fi.ModTime().Before(cutoff) {
			os.Remove(filepath.Join(dir, de.Name()))
		}
	}
}

// dirtyPaths lists files that differ from HEAD (including untracked ones, but
// not ignored ones), relative to the worktree root. --no-optional-locks keeps
// git status from refreshing the index, which takes index.lock: a git command
// the agent runs at the same time (parallel tool calls) would fail on it.
func dirtyPaths(ctx context.Context, root string) ([]string, error) {
	out, err := exec.CommandContext(ctx, "git", "-C", root, "--no-optional-locks", "status", "--porcelain=v1", "-z",
		"--untracked-files=all", "--no-renames", "--ignore-submodules=all").Output()
	if ctx.Err() != nil {
		return nil, ErrBudget
	}
	if err != nil {
		return nil, err
	}
	var paths []string
	for _, rec := range strings.Split(string(out), "\x00") {
		if len(rec) > 3 {
			paths = append(paths, rec[3:])
		}
	}
	return paths, nil
}

func (s Store) readLast() map[string]Entry {
	last := map[string]Entry{}
	if data, err := os.ReadFile(s.lastPath()); err == nil {
		_ = json.Unmarshal(data, &last)
	}
	return last
}

func (s Store) writeLast(files map[string]Entry) {
	if data, err := json.Marshal(files); err == nil {
		_ = writeAtomic(s.lastPath(), data)
	}
}

// writeAtomic writes via a temp file and rename, so concurrent hooks never see
// a partial file.
func writeAtomic(path string, data []byte) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return err
	}
	tmp, err := os.CreateTemp(filepath.Dir(path), ".tmp-*")
	if err != nil {
		return err
	}
	_, werr := tmp.Write(data)
	cerr := tmp.Close()
	if err := errors.Join(werr, cerr); err != nil {
		os.Remove(tmp.Name())
		return err
	}
	return os.Rename(tmp.Name(), path)
}

// safeName keeps a tool_use_id usable as a file name.
func safeName(id string) string {
	return strings.Map(func(r rune) rune {
		if r == '-' || r == '_' || r >= '0' && r <= '9' || r >= 'a' && r <= 'z' || r >= 'A' && r <= 'Z' {
			return r
		}
		return '_'
	}, id)
}

func sortedKeys(m map[string]bool) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}
