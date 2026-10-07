package report

import (
	"bytes"
	"fmt"
	"os/exec"
	"regexp"
	"strconv"
	"strings"
	"time"
)

// repo runs read-only git queries against one repository and caches the
// answers, since many sessions and hunks ask about the same commits.
type repo struct {
	dir      string
	contents map[string]*string // sha:path → file content (nil: absent)
	added    map[string]map[int]bool
	pushed   map[string]bool
}

func newRepo(dir string) *repo {
	return &repo{dir: dir, contents: map[string]*string{}, added: map[string]map[int]bool{}, pushed: map[string]bool{}}
}

func (r *repo) git(args ...string) ([]byte, error) {
	cmd := exec.Command("git", append([]string{"-C", r.dir}, args...)...)
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	out, err := cmd.Output()
	if err != nil {
		return nil, fmt.Errorf("git %s: %w: %s", strings.Join(args, " "), err, strings.TrimSpace(stderr.String()))
	}
	return out, nil
}

// commit is a candidate commit and the paths it touched.
type commit struct {
	SHA         string
	At          time.Time
	AuthorEmail string
	Subject     string
	Paths       map[string]bool
}

// commitsSince lists non-merge commits reachable from any ref, committed at or
// after since, oldest first.
func (r *repo) commitsSince(since time.Time) ([]commit, error) {
	out, err := r.git("log", "--all", "--no-merges", "--reverse",
		"--since="+strconv.FormatInt(since.Unix(), 10),
		"--format=%x1e%H%x1f%ct%x1f%ae%x1f%s", "--name-only")
	if err != nil {
		return nil, err
	}
	var commits []commit
	for _, rec := range strings.Split(string(out), "\x1e") {
		header, files, _ := strings.Cut(rec, "\n")
		f := strings.SplitN(header, "\x1f", 4)
		if len(f) != 4 {
			continue
		}
		ct, _ := strconv.ParseInt(f[1], 10, 64)
		c := commit{SHA: f[0], At: time.Unix(ct, 0).UTC(), AuthorEmail: f[2], Subject: f[3], Paths: map[string]bool{}}
		for _, p := range strings.Split(files, "\n") {
			if p = strings.TrimSpace(p); p != "" {
				c.Paths[p] = true
			}
		}
		commits = append(commits, c)
	}
	return commits, nil
}

// content returns path as of sha; ok is false if the commit doesn't have it.
func (r *repo) content(sha, path string) (string, bool) {
	key := sha + ":" + path
	if c, seen := r.contents[key]; seen {
		return deref(c)
	}
	out, err := r.git("show", key)
	if err != nil {
		r.contents[key] = nil
		return "", false
	}
	s := string(out)
	r.contents[key] = &s
	return s, true
}

func deref(s *string) (string, bool) {
	if s == nil {
		return "", false
	}
	return *s, true
}

// hunkHeader matches the new-side range of a unified diff hunk header.
var hunkHeader = regexp.MustCompile(`^@@ -\d+(?:,\d+)? \+(\d+)(?:,(\d+))? @@`)

// addedLines returns the line numbers (in sha's version of path) that sha
// itself added relative to its parent — the lines it introduced. The root
// commit introduces every line.
func (r *repo) addedLines(sha, path string) map[int]bool {
	key := sha + ":" + path
	if a, ok := r.added[key]; ok {
		return a
	}
	added := map[int]bool{}
	out, err := r.git("diff-tree", "-p", "-U0", "--no-color", "--no-ext-diff", "--root", sha, "--", path)
	if err == nil {
		for _, l := range strings.Split(string(out), "\n") {
			m := hunkHeader.FindStringSubmatch(l)
			if m == nil {
				continue
			}
			start, _ := strconv.Atoi(m[1])
			n := 1
			if m[2] != "" {
				n, _ = strconv.Atoi(m[2])
			}
			for i := start; i < start+n; i++ {
				added[i] = true
			}
		}
	}
	r.added[key] = added
	return added
}

// isPushed reports whether any remote-tracking ref contains sha.
func (r *repo) isPushed(sha string) bool {
	if p, ok := r.pushed[sha]; ok {
		return p
	}
	out, err := r.git("for-each-ref", "--contains", sha, "--count=1", "--format=%(refname)", "refs/remotes/")
	p := err == nil && len(bytes.TrimSpace(out)) > 0
	r.pushed[sha] = p
	return p
}

// configValue reads a git config value, "" if unset.
func (r *repo) configValue(key string) string {
	out, err := r.git("config", "--get", key)
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(out))
}
