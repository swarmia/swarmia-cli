package cmd

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"os/exec"
	"regexp"
	"sort"
	"strings"
	"syscall"

	"github.com/spf13/cobra"
)

const (
	// refPrefix is the namespace all session refs live under. The Swarmia server
	// enumerates refs/ai-sessions/* to find sessions to process.
	refPrefix = "refs/ai-sessions/"
	// eventsFileName is the single file stored in each session ref's tree.
	eventsFileName = "events.jsonl"
	// noSessionKey buckets events with a missing or unsafe session id so they are
	// stored rather than dropped.
	noSessionKey = "no-session"
)

var (
	processFile   string
	processPush   bool
	processRemote string
	processKeep   bool
)

var processLogsCmd = &cobra.Command{
	Use:   "process-logs",
	Short: "Store captured hook events into refs/ai-sessions/*",
	Long: "process-logs reads the captured hook events (see `log-hook`), groups them by\n" +
		"session, and stores each session's events into a git ref named\n" +
		"refs/ai-sessions/<session_id>. It uses git plumbing only: it never touches the\n" +
		"working tree, index, or HEAD, so nothing shows up in `git status`.\n\n" +
		"Each run appends the new events to the session's ref (a session that continues\n" +
		"after its logs were shipped keeps updating the same ref) and then empties the\n" +
		"local log file. It does not push by default; pass --push to also push the refs\n" +
		"to a remote.\n\n" +
		"It is meant to be run from a periodic or lifecycle hook, not on every tool call.",
	// Match log-hook's passive discipline: never fail the process, so wiring it into
	// a hook can't block the agent. Errors go to stderr; exit stays 0.
	SilenceUsage:  true,
	SilenceErrors: true,
	RunE: func(cmd *cobra.Command, args []string) error {
		opts := processOpts{push: processPush, remote: processRemote, keep: processKeep}
		res, err := processLogs(projectDir(), eventsPath(processFile), opts)
		if err != nil {
			fmt.Fprintf(os.Stderr, "swarmia-cli process-logs: %v\n", err)
			return nil
		}
		if res.Events > 0 {
			fmt.Fprintf(cmd.OutOrStdout(), "stored %d event(s) across %d session(s): %s\n",
				res.Events, res.Sessions, strings.Join(res.Refs, ", "))
		}
		return nil
	},
}

func init() {
	processLogsCmd.Flags().StringVar(&processFile, "file", "", "path to the JSONL log file (overrides $SWARMIA_LOG_FILE and the default)")
	processLogsCmd.Flags().BoolVar(&processPush, "push", false, "also push each stored ref to the remote (best-effort; off by default)")
	processLogsCmd.Flags().StringVar(&processRemote, "remote", "origin", "remote to push to when --push is set")
	processLogsCmd.Flags().BoolVar(&processKeep, "keep", false, "do not empty the log file after storing (for debugging; re-running will duplicate)")
	rootCmd.AddCommand(processLogsCmd)
}

type processOpts struct {
	push   bool
	remote string
	keep   bool
}

type processResult struct {
	Sessions int
	Events   int
	Refs     []string
}

// processLogs claims the current batch of events from logPath, stores each
// session's events into refs/ai-sessions/<session_id> in the repo at repoDir, and
// (unless opts.keep) empties the log file. On a store failure the unstored events
// are restored to the log file so nothing is lost and the next run retries.
func processLogs(repoDir, logPath string, opts processOpts) (processResult, error) {
	var res processResult

	batch, err := claimBatch(logPath, opts.keep)
	if err != nil {
		return res, err
	}
	if len(batch) == 0 {
		return res, nil // nothing logged since last run
	}

	groups := groupBySession(batch)
	keys := sortedKeys(groups)

	var failed error
	var unstored [][]byte
	for _, key := range keys {
		if failed != nil {
			unstored = append(unstored, groups[key]...)
			continue
		}
		if err := storeRef(repoDir, key, groups[key]); err != nil {
			failed = fmt.Errorf("storing %s%s: %w", refPrefix, key, err)
			unstored = append(unstored, groups[key]...)
			continue
		}
		res.Sessions++
		res.Events += len(groups[key])
		res.Refs = append(res.Refs, refPrefix+key)
	}

	if failed != nil {
		// Only restore when we actually truncated; with --keep the events are still
		// in the file. Restore just the unstored sessions — already-stored refs are
		// committed and must not be replayed (that would duplicate their events).
		if !opts.keep {
			if rerr := restoreBatch(logPath, unstored); rerr != nil {
				return res, fmt.Errorf("%v; also failed to restore unstored events: %w", failed, rerr)
			}
		}
		return res, failed
	}

	if opts.push {
		for _, key := range keys {
			if err := pushRef(repoDir, opts.remote, key); err != nil {
				fmt.Fprintf(os.Stderr, "swarmia-cli process-logs: pushing %s%s to %s failed (non-fatal): %v\n",
					refPrefix, key, opts.remote, err)
			}
		}
	}

	return res, nil
}

// claimBatch reads the whole log file under an exclusive lock and (unless keep)
// truncates it to zero in the same critical section. Because log-hook only writes
// while holding the same lock, every concurrent append is either fully included in
// the returned bytes (then truncated away) or lands in the now-empty file after we
// release — never interleaved, never lost.
func claimBatch(path string, keep bool) ([]byte, error) {
	f, err := os.OpenFile(path, os.O_RDWR, 0o644)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil // nothing has been logged yet
		}
		return nil, fmt.Errorf("opening %s: %w", path, err)
	}
	defer f.Close()

	if err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX); err != nil {
		return nil, fmt.Errorf("locking %s: %w", path, err)
	}
	defer syscall.Flock(int(f.Fd()), syscall.LOCK_UN)

	data, err := io.ReadAll(f)
	if err != nil {
		return nil, fmt.Errorf("reading %s: %w", path, err)
	}
	if len(bytes.TrimSpace(data)) == 0 {
		return nil, nil
	}
	if !keep {
		if err := f.Truncate(0); err != nil {
			return nil, fmt.Errorf("truncating %s: %w", path, err)
		}
	}
	return data, nil
}

// groupBySession splits the batch into lines and buckets them by session id,
// preserving per-session line order. Lines that don't parse or lack a usable
// session id go to the no-session bucket so they are still stored.
func groupBySession(batch []byte) map[string][][]byte {
	groups := map[string][][]byte{}
	for _, line := range bytes.Split(batch, []byte("\n")) {
		line = bytes.TrimSpace(line)
		if len(line) == 0 {
			continue
		}
		// Copy the line out of the shared backing array so later appends can't alias it.
		buf := make([]byte, len(line))
		copy(buf, line)
		key := sessionKey(buf)
		groups[key] = append(groups[key], buf)
	}
	return groups
}

// sessionKey extracts and sanitizes the session id from a log record envelope
// ({"logged_at":..., "event":{...}}). Returns noSessionKey when absent or unsafe.
func sessionKey(line []byte) string {
	var rec struct {
		Event struct {
			SessionID string `json:"session_id"`
		} `json:"event"`
	}
	if err := json.Unmarshal(line, &rec); err != nil {
		return noSessionKey
	}
	if key := sanitizeRefKey(rec.Event.SessionID); key != "" {
		return key
	}
	return noSessionKey
}

var refKeyRe = regexp.MustCompile(`^[A-Za-z0-9._-]+$`)

// sanitizeRefKey returns s if it is safe to use as the last component of a git ref
// name, else "". Session ids are UUIDs, which pass; anything odd is rejected so it
// falls back to the no-session bucket rather than producing an invalid ref.
func sanitizeRefKey(s string) string {
	if s == "" || !refKeyRe.MatchString(s) {
		return ""
	}
	if strings.HasPrefix(s, "-") || strings.HasPrefix(s, ".") || strings.Contains(s, "..") || strings.HasSuffix(s, ".lock") {
		return ""
	}
	return s
}

// storeRef appends lines to refs/ai-sessions/<key>: it reads any prior content,
// concatenates the new lines, and commits the result with the previous ref as
// parent. All via git plumbing, so the working tree and index are never touched.
func storeRef(repoDir, key string, lines [][]byte) error {
	ref := refPrefix + key

	// Prior full content, if the ref already exists (the accumulation path).
	var content bytes.Buffer
	if prior, err := runGit(repoDir, nil, "cat-file", "-p", ref+":"+eventsFileName); err == nil {
		if prior = bytes.TrimRight(prior, "\n"); len(prior) > 0 {
			content.Write(prior)
			content.WriteByte('\n')
		}
	}
	for _, l := range lines {
		content.Write(l)
		content.WriteByte('\n')
	}

	blob, err := runGit(repoDir, content.Bytes(), "hash-object", "-w", "--stdin")
	if err != nil {
		return fmt.Errorf("hash-object: %w", err)
	}
	blobSHA := strings.TrimSpace(string(blob))

	treeSpec := fmt.Sprintf("100644 blob %s\t%s\n", blobSHA, eventsFileName)
	tree, err := runGit(repoDir, []byte(treeSpec), "mktree")
	if err != nil {
		return fmt.Errorf("mktree: %w", err)
	}
	treeSHA := strings.TrimSpace(string(tree))

	// Current ref value becomes the commit parent and the compare-and-swap guard.
	oldValue := ""
	if out, err := runGit(repoDir, nil, "rev-parse", "--verify", "--quiet", ref); err == nil {
		oldValue = strings.TrimSpace(string(out))
	}

	commitArgs := []string{"commit-tree", treeSHA}
	if oldValue != "" {
		commitArgs = append(commitArgs, "-p", oldValue)
	}
	commitArgs = append(commitArgs, "-m", fmt.Sprintf("swarmia session %s: +%d events", key, len(lines)))
	commit, err := runGit(repoDir, nil, commitArgs...)
	if err != nil {
		return fmt.Errorf("commit-tree: %w", err)
	}
	commitSHA := strings.TrimSpace(string(commit))

	// update-ref with the old value (empty string means "must not exist") so a
	// concurrent writer racing us can't be silently clobbered.
	if _, err := runGit(repoDir, nil, "update-ref", ref, commitSHA, oldValue); err != nil {
		return fmt.Errorf("update-ref: %w", err)
	}
	return nil
}

// pushRef pushes a single session ref to the remote, preserving its full name.
func pushRef(repoDir, remote, key string) error {
	ref := refPrefix + key
	_, err := runGit(repoDir, nil, "push", remote, ref+":"+ref)
	return err
}

// restoreBatch re-appends unstored lines to the log file (under the same lock
// log-hook uses) so a failed store retries on the next run instead of losing data.
func restoreBatch(path string, lines [][]byte) error {
	if len(lines) == 0 {
		return nil
	}
	var buf bytes.Buffer
	for _, l := range lines {
		buf.Write(l)
		buf.WriteByte('\n')
	}
	return appendLine(path, buf.Bytes())
}

// runGit runs `git -C dir args...` with a fixed committer identity (so it never
// depends on or mutates the user's git config), feeding stdin when provided.
func runGit(dir string, stdin []byte, args ...string) ([]byte, error) {
	cmd := exec.Command("git", append([]string{"-C", dir}, args...)...)
	cmd.Env = append(os.Environ(),
		"GIT_AUTHOR_NAME=Swarmia Agent CLI",
		"GIT_AUTHOR_EMAIL=agent-cli@swarmia.com",
		"GIT_COMMITTER_NAME=Swarmia Agent CLI",
		"GIT_COMMITTER_EMAIL=agent-cli@swarmia.com",
	)
	if stdin != nil {
		cmd.Stdin = bytes.NewReader(stdin)
	}
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		return nil, fmt.Errorf("git %s: %w: %s", strings.Join(args, " "), err, strings.TrimSpace(stderr.String()))
	}
	return stdout.Bytes(), nil
}

func sortedKeys(m map[string][][]byte) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}
