// Package deliver sends session payloads to Swarmia's ingest endpoint.
//
// The endpoint is asynchronous: it queues what it receives and answers with a
// bare 2xx, so success means "received", not "processed". (A 200 body with
// per-session results is still honoured, should the endpoint ever return one.)
//
// Each session is sent as its complete current payload; the server keeps the
// latest version per session. A session is sent again only when its payload
// changes (more activity, a commit pushed), at most every MinResendInterval
// unless it has just ended. Failures back off; nothing is lost, since the
// sessions stay in the local store until retention.
//
// The server can also ask this CLI version to upgrade: with a notice in a
// 2xx reply (it's deprecated, still accepted), or by refusing it with 426
// (nothing more is sent from this version, except by a manual `send`). The
// server decides which versions those are; the client records what it said
// and shows it in `config`, `report` and `send`.
package deliver

import (
	"bytes"
	"compress/gzip"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/swarmia/swarmia-cli/internal/payload"
	"github.com/swarmia/swarmia-cli/internal/report"
)

const (
	// SchemaVersion identifies the request format.
	SchemaVersion = 1
	// MinResendInterval throttles resending a changed session that hasn't
	// ended, so a long session isn't uploaded on every background run.
	MinResendInterval = 15 * time.Minute
	// maxBatchSessions and maxBatchBytes bound one request (bytes before
	// compression).
	maxBatchSessions = 25
	maxBatchBytes    = 4 << 20
	requestTimeout   = 30 * time.Second
	maxBackoff       = 6 * time.Hour
	authBackoff      = 6 * time.Hour
	// upgradeMemory is how long what the server said about a version is kept;
	// a refused version asks again after that.
	upgradeMemory = 30 * 24 * time.Hour
	// maxMessage caps a server message shown to the developer.
	maxMessage = 300
)

// Request is the body POSTed to <endpoint>/sessions.
type Request struct {
	SchemaVersion int               `json:"schema_version"`
	CLIVersion    string            `json:"cli_version"`
	SentAt        time.Time         `json:"sent_at"`
	Sessions      []payload.Payload `json:"sessions"`
}

// Response is an optional reply body with one result per session. A bare 2xx
// marks every session in the batch as received.
type Response struct {
	Results []Result `json:"results"`
	// Upgrade, if set, asks this CLI version to upgrade; it's still accepted.
	Upgrade *UpgradeNotice `json:"upgrade,omitempty"`
}

// UpgradeNotice is the server asking a CLI version to upgrade, in a 2xx reply
// or a 426 reply's body.
type UpgradeNotice struct {
	Message string `json:"message"`
}

// Upgrade is what the server last said about a CLI version.
type Upgrade struct {
	// Required is set when the server refused the version (426): nothing is
	// sent from it until it's upgraded.
	Required bool      `json:"required,omitempty"`
	Message  string    `json:"message"`
	At       time.Time `json:"at"`
}

// Result is the server's verdict on one session.
type Result struct {
	SessionID string `json:"session_id"`
	Status    string `json:"status"` // "accepted" or "rejected"
	Error     string `json:"error,omitempty"`
}

// Options configures Deliver.
type Options struct {
	Endpoint   string
	Token      string
	CLIVersion string
	Context    payload.Context // shared by every session's payload
	StateDir   string          // where delivery.json (backoff state) lives
	Client     *http.Client    // nil uses a client with a 30 s timeout
	Now        time.Time
	// Force ignores MinResendInterval and any backoff (manual `send`).
	Force bool
}

// Outcome summarizes a Deliver call.
type Outcome struct {
	Sent, Accepted, Rejected int
	Skipped                  string // why nothing was attempted, if so
}

// state is the backoff bookkeeping kept across runs.
type state struct {
	Failures    int       `json:"failures"`
	NextAttempt time.Time `json:"next_attempt"`
	LastError   string    `json:"last_error,omitempty"`
	// Upgrades holds what the server said about CLI versions, by version:
	// worktrees with different versions share this state.
	Upgrades map[string]Upgrade `json:"upgrades,omitempty"`
}

// ErrUpgradeRequired is the server refusing this CLI version.
var ErrUpgradeRequired = errors.New("Swarmia no longer accepts sessions from this swarmia-cli version")

// UpgradeFor reports what the server last said about cliVersion, if anything,
// from the delivery state in stateDir.
func UpgradeFor(stateDir, cliVersion string) (Upgrade, bool) {
	u, ok := loadState(stateDir).Upgrades[cliVersion]
	return u, ok
}

// setUpgrade records what the server said about version (nil: nothing, so
// any earlier word is dropped), forgetting what it said long ago. It reports
// whether the state changed.
func (st *state) setUpgrade(version string, u *Upgrade, now time.Time) bool {
	changed := false
	for v, old := range st.Upgrades {
		if now.Sub(old.At) > upgradeMemory {
			delete(st.Upgrades, v)
			changed = true
		}
	}
	old, had := st.Upgrades[version]
	switch {
	case u == nil && had:
		delete(st.Upgrades, version)
		return true
	case u == nil:
		return changed
	case had && old.Required == u.Required && old.Message == u.Message:
		return changed // keep its original time
	}
	if st.Upgrades == nil {
		st.Upgrades = map[string]Upgrade{}
	}
	st.Upgrades[version] = *u
	return true
}

// Deliver sends the sessions whose payloads changed since they were last
// delivered, updating each one's Delivery. Sessions are only modified for
// attempts that reached the server and got an answer about them.
func Deliver(sessions []*report.Session, opts Options) (Outcome, error) {
	var out Outcome
	if opts.Token == "" {
		out.Skipped = "no token configured"
		return out, nil
	}
	st := loadState(opts.StateDir)
	if u := st.Upgrades[opts.CLIVersion]; u.Required && !opts.Force && opts.Now.Sub(u.At) <= upgradeMemory {
		out.Skipped = fmt.Sprintf("%v (%s): %s", ErrUpgradeRequired, opts.CLIVersion, u.Message)
		return out, nil
	}
	if !opts.Force && opts.Now.Before(st.NextAttempt) {
		out.Skipped = fmt.Sprintf("backing off until %s after: %s", st.NextAttempt.Local().Format(time.DateTime), st.LastError)
		return out, nil
	}

	due, err := dueSessions(sessions, opts)
	if err != nil {
		return out, err
	}

	client := opts.Client
	if client == nil {
		client = &http.Client{Timeout: requestTimeout}
	}
	limit := maxBatchSessions // halved on 413
	stateChanged := false
	for start := 0; start < len(due); {
		end, size := start, 0
		for end < len(due) && end-start < limit && (end == start || size+due[end].size <= maxBatchBytes) {
			size += due[end].size
			end++
		}
		batch := due[start:end]
		payloads := make([]payload.Payload, len(batch))
		for i, p := range batch {
			payloads[i] = p.payload
		}

		results, notice, err := post(client, opts, payloads)
		var refused *upgradeRequiredError
		if errors.As(err, &refused) {
			// Not a failure to back off from: nothing more is sent from this
			// version, and its sessions are sent as they are once upgraded.
			st.Failures, st.NextAttempt, st.LastError = 0, time.Time{}, ""
			st.setUpgrade(opts.CLIVersion, &Upgrade{Required: true, Message: refused.message, At: opts.Now}, opts.Now)
			saveState(opts.StateDir, st)
			return out, fmt.Errorf("%w (%s): %s", ErrUpgradeRequired, opts.CLIVersion, refused.message)
		}
		if errors.Is(err, errTooLarge) {
			if len(batch) > 1 {
				limit = max(len(batch)/2, 1)
				continue // retry the same sessions in smaller batches
			}
			results, err = []Result{{SessionID: batch[0].session.ID, Status: "rejected", Error: "payload too large"}}, nil
		} else if err == nil {
			var u *Upgrade
			if notice != nil {
				u = &Upgrade{Message: notice.Message, At: opts.Now}
			}
			stateChanged = st.setUpgrade(opts.CLIVersion, u, opts.Now) || stateChanged
		}
		if err != nil {
			st.fail(err, opts.Now)
			saveState(opts.StateDir, st)
			return out, err
		}
		out.Sent += len(batch)
		byID := map[string]Result{}
		for _, r := range results {
			byID[r.SessionID] = r
		}
		for _, p := range batch {
			r, ok := byID[p.session.ID]
			if !ok {
				continue // the server didn't answer for it; retry next time
			}
			d := &report.Delivery{Hash: p.hash, SentAt: opts.Now}
			if r.Status == "accepted" {
				out.Accepted++
			} else {
				d.Rejected = r.Error
				if d.Rejected == "" {
					d.Rejected = r.Status
				}
				out.Rejected++
			}
			p.session.Delivery = d
		}
		start = end
	}
	if st.Failures > 0 || st.LastError != "" || !st.NextAttempt.IsZero() {
		st.Failures, st.NextAttempt, st.LastError = 0, time.Time{}, ""
		stateChanged = true
	}
	if stateChanged {
		saveState(opts.StateDir, st)
	}
	return out, nil
}

type pending struct {
	session *report.Session
	payload payload.Payload
	hash    string
	size    int
}

// dueSessions picks the sessions to send: new ones, and changed ones that have
// ended or weren't sent within MinResendInterval (any changed one when forced).
func dueSessions(sessions []*report.Session, opts Options) ([]pending, error) {
	var due []pending
	for _, s := range sessions {
		p, ok := payload.Build(s, opts.Context)
		if !ok {
			continue // in no repository Swarmia could know
		}
		data, err := json.Marshal(p)
		if err != nil {
			return nil, err
		}
		hash := hashOf(data)
		if s.Delivery != nil {
			if s.Delivery.Hash == hash {
				continue // unchanged since last delivered
			}
			if !opts.Force && !s.Ended && opts.Now.Sub(s.Delivery.SentAt) < MinResendInterval {
				continue // changed, but sent recently and still going
			}
		}
		due = append(due, pending{s, p, hash, len(data)})
	}
	return due, nil
}

// Pending counts the sessions Deliver would send now.
func Pending(sessions []*report.Session, opts Options) int {
	due, _ := dueSessions(sessions, opts)
	return len(due)
}

// errTooLarge is a 413 reply: the caller splits the batch, or rejects a lone
// session that is too large by itself.
var errTooLarge = errors.New("request too large (413)")

// upgradeRequiredError is a 426 reply: the server refuses this CLI version.
type upgradeRequiredError struct{ message string }

func (e *upgradeRequiredError) Error() string { return "upgrade required (426): " + e.message }

// post sends one batch and decodes the per-session results, and any upgrade
// notice for this CLI version.
func post(client *http.Client, opts Options, sessions []payload.Payload) ([]Result, *UpgradeNotice, error) {
	body, err := json.Marshal(Request{SchemaVersion: SchemaVersion, CLIVersion: opts.CLIVersion, SentAt: opts.Now.UTC(), Sessions: sessions})
	if err != nil {
		return nil, nil, err
	}
	var gz bytes.Buffer
	w := gzip.NewWriter(&gz)
	w.Write(body)
	w.Close()

	req, err := http.NewRequest(http.MethodPost, strings.TrimRight(opts.Endpoint, "/")+"/sessions", &gz)
	if err != nil {
		return nil, nil, err
	}
	req.Header.Set("Authorization", "Bearer "+opts.Token)
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Content-Encoding", "gzip")
	req.Header.Set("User-Agent", "swarmia-cli/"+opts.CLIVersion)

	resp, err := client.Do(req)
	if err != nil {
		return nil, nil, err
	}
	defer resp.Body.Close()
	data, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))

	switch {
	case resp.StatusCode >= 200 && resp.StatusCode < 300:
		var r Response
		json.Unmarshal(data, &r)
		if r.Upgrade != nil {
			r.Upgrade.Message = displayable(r.Upgrade.Message, "please upgrade swarmia-cli")
		}
		if r.Results != nil {
			return r.Results, r.Upgrade, nil
		}
		// No per-session answer: the whole batch was received.
		results := make([]Result, len(sessions))
		for i, p := range sessions {
			results[i] = Result{SessionID: p.SessionID, Status: "accepted"}
		}
		return results, r.Upgrade, nil
	case resp.StatusCode == http.StatusUpgradeRequired:
		var n UpgradeNotice
		if json.Unmarshal(data, &n) != nil {
			n.Message = string(data) // plain text
		}
		return nil, nil, &upgradeRequiredError{displayable(n.Message, "upgrade swarmia-cli")}
	case resp.StatusCode == http.StatusRequestEntityTooLarge:
		return nil, nil, errTooLarge
	default:
		return nil, nil, &httpError{status: resp.StatusCode, msg: strings.TrimSpace(string(data)), retryAfter: resp.Header.Get("Retry-After")}
	}
}

// displayable makes a server message safe to print in a terminal: control
// characters (escape sequences) dropped, whitespace collapsed, length capped.
// An empty message becomes fallback.
func displayable(msg, fallback string) string {
	msg = strings.Map(func(r rune) rune {
		switch {
		case r == '\t' || r == '\n' || r == '\r':
			return ' '
		case r < 0x20 || r >= 0x7f && r < 0xa0:
			return -1
		}
		return r
	}, msg)
	msg = strings.Join(strings.Fields(msg), " ")
	if msg == "" {
		return fallback
	}
	return truncate(msg, maxMessage)
}

type httpError struct {
	status     int
	msg        string
	retryAfter string
}

func (e *httpError) Error() string {
	if e.msg == "" {
		return fmt.Sprintf("HTTP %d", e.status)
	}
	return fmt.Sprintf("HTTP %d: %s", e.status, truncate(e.msg, 200))
}

// fail records a failed attempt and schedules the next one: a long pause for
// an unusable token, Retry-After when given, else exponential backoff.
func (st *state) fail(err error, now time.Time) {
	st.Failures++
	st.LastError = err.Error()
	wait := min(time.Minute<<min(st.Failures-1, 10), maxBackoff)
	var he *httpError
	if errors.As(err, &he) {
		switch {
		case he.status == http.StatusUnauthorized || he.status == http.StatusForbidden:
			wait = authBackoff
		case he.retryAfter != "":
			if secs, err := strconv.Atoi(he.retryAfter); err == nil {
				wait = min(time.Duration(secs)*time.Second, maxBackoff)
			}
		}
	}
	st.NextAttempt = now.Add(wait)
}

func loadState(dir string) state {
	var st state
	if data, err := os.ReadFile(filepath.Join(dir, "delivery.json")); err == nil {
		_ = json.Unmarshal(data, &st)
	}
	return st
}

func saveState(dir string, st state) {
	data, err := json.Marshal(st)
	if err != nil {
		return
	}
	path := filepath.Join(dir, "delivery.json")
	tmp := path + ".tmp"
	if os.WriteFile(tmp, data, 0o600) == nil {
		os.Rename(tmp, path)
	}
}

func hashOf(data []byte) string {
	sum := sha256.Sum256(data)
	return hex.EncodeToString(sum[:])
}

// truncate cuts s to at most n bytes, at a character boundary.
func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	for n > 0 && !utf8.RuneStart(s[n]) {
		n--
	}
	return s[:n] + "…"
}
