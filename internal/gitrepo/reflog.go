package gitrepo

import (
	"bufio"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"time"
)

// createdHere matches HEAD reflog messages of operations that create a commit
// in this checkout: commits (including amends), rebase and pull --rebase
// picks, cherry-picks, reverts and applied patches. Entries that only move HEAD
// to commits made elsewhere — checkout, reset, fast-forward pulls and merges,
// rebase start/finish — don't match.
var createdHere = regexp.MustCompile(`^(commit( \((initial|amend|merge)\))?|(rebase|pull --rebase)( -i| --interactive)? \((pick|reword|edit|squash|fixup)\)|cherry-pick|revert|am):`)

// LocallyCreatedCommits returns the commits created on this machine since the
// given time, according to the HEAD reflogs of every worktree of the
// repository. Reflogs expire (90 days by default) and are local to a clone, so
// this complements rather than replaces other ways of finding one's commits.
// Reftable repositories, whose reflogs aren't plain files, yield an empty set.
func (r Repo) LocallyCreatedCommits(since time.Time) (map[string]bool, error) {
	logs := []string{filepath.Join(r.CommonDir, "logs", "HEAD")}
	wts, _ := filepath.Glob(filepath.Join(r.CommonDir, "worktrees", "*", "logs", "HEAD"))
	logs = append(logs, wts...)

	created := map[string]bool{}
	for _, path := range logs {
		f, err := os.Open(path)
		if err != nil {
			if os.IsNotExist(err) {
				continue
			}
			return created, err
		}
		sc := bufio.NewScanner(f)
		sc.Buffer(make([]byte, 0, 64*1024), 1<<20)
		for sc.Scan() {
			if sha, at, msg, ok := parseReflogLine(sc.Text()); ok && !at.Before(since) && createdHere.MatchString(msg) {
				created[sha] = true
			}
		}
		f.Close()
	}
	return created, nil
}

// parseReflogLine splits "<old> <new> <name> <<email>> <unix time> <tz>\t<msg>".
func parseReflogLine(line string) (newSHA string, at time.Time, msg string, ok bool) {
	head, msg, found := strings.Cut(line, "\t")
	if !found {
		return "", time.Time{}, "", false
	}
	fields := strings.Fields(head)
	if len(fields) < 5 {
		return "", time.Time{}, "", false
	}
	secs, err := strconv.ParseInt(fields[len(fields)-2], 10, 64)
	if err != nil {
		return "", time.Time{}, "", false
	}
	return fields[1], time.Unix(secs, 0), msg, true
}
