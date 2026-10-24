// Package deliver sends session payloads to Swarmia's ingest endpoint.
//
// The endpoint is asynchronous: it queues what it receives and answers with a
// bare 2xx, so success means "received", not "processed". (A 200 body with
// per-session results is still honoured, should the endpoint ever return one.)
//
// Each session is sent as its complete current payload; the server keeps the
// latest version per session. A session that worked in repositories
// configured with different tokens is split into one part per delivery target
// (see payload.Build), each sent, tracked and backed off on its own. A part is
// sent again only when its payload changes (more activity, a commit pushed),
// at most every MinResendInterval unless the session has just ended. Failures
// back off; nothing is lost, since the sessions stay in the local store until
// retention.
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
	CLIVersion string
	Context    payload.Context // builds each session's parts, and says where they go
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

// state is the delivery bookkeeping kept across runs.
type state struct {
	// Targets holds each delivery target's backoff, by payload.Target.ID.
	Targets map[string]*backoff `json:"targets,omitempty"`
	// Upgrades holds what the server said about CLI versions, by version:
	// worktrees with different versions share this state.
	Upgrades map[string]Upgrade `json:"upgrades,omitempty"`
}

// backoff is one target's failed attempts.
type backoff struct {
	Failures    int       `json:"failures"`
	NextAttempt time.Time `json:"next_attempt"`
	LastError   string    `json:"last_error,omitempty"`
}

// NoToken is Outcome.Skipped when no repository any session worked in has a
// token configured.
const NoToken = "no token configured"

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

// Deliver sends the parts of sessions whose payloads changed since they were
// last delivered, updating each session's Deliveries. Sessions are only
// modified for attempts that reached the server and got an answer about them.
// A target that fails backs off without holding up the others; the errors are
// returned together.
func Deliver(sessions []*report.Session, opts Options) (Outcome, error) {
	var out Outcome
	due, targets, err := dueParts(sessions, opts)
	if err != nil {
		return out, err
	}
	if len(targets) == 0 {
		out.Skipped = NoToken
		return out, nil
	}
	st := loadState(opts.StateDir)
	if u := st.Upgrades[opts.CLIVersion]; u.Required && !opts.Force && opts.Now.Sub(u.At) <= upgradeMemory {
		out.Skipped = fmt.Sprintf("%v (%s): %s", ErrUpgradeRequired, opts.CLIVersion, u.Message)
		return out, nil
	}

	client := opts.Client
	if client == nil {
		client = &http.Client{Timeout: requestTimeout}
	}
	if st.Targets == nil {
		st.Targets = map[string]*backoff{}
	}
	var errs []error
	for _, t := range targets {
		id := t.ID()
		bo := st.Targets[id]
		if bo == nil {
			bo = &backoff{}
		}
		if !opts.Force && opts.Now.Before(bo.NextAttempt) {
			out.Skipped = fmt.Sprintf("backing off until %s after: %s", bo.NextAttempt.Local().Format(time.DateTime), bo.LastError)
			continue
		}
		err := deliverTo(client, t, due[id], opts, &st, &out)
		var refused *upgradeRequiredError
		switch {
		case errors.As(err, &refused):
			// Not a failure to back off from: nothing more is sent from this
			// version, and its sessions are sent as they are once upgraded.
			st.setUpgrade(opts.CLIVersion, &Upgrade{Required: true, Message: refused.message, At: opts.Now}, opts.Now)
			saveState(opts.StateDir, st)
			return out, errors.Join(append(errs, fmt.Errorf("%w (%s): %s", ErrUpgradeRequired, opts.CLIVersion, refused.message))...)
		case err != nil:
			bo.fail(err, opts.Now)
			st.Targets[id] = bo
			errs = append(errs, err)
		default:
			delete(st.Targets, id)
		}
	}
	saveState(opts.StateDir, st)
	return out, errors.Join(errs...)
}

// deliverTo sends one target's due parts in batches, recording what the
// server answered.
func deliverTo(client *http.Client, t payload.Target, due []pending, opts Options, st *state, out *Outcome) error {
	limit := maxBatchSessions // halved on 413
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

		results, notice, err := post(client, t, opts, payloads)
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
			st.setUpgrade(opts.CLIVersion, u, opts.Now)
		}
		if err != nil {
			return err
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
			if p.session.Deliveries == nil {
				p.session.Deliveries = map[string]*report.Delivery{}
			}
			p.session.Deliveries[t.ID()] = d
		}
		start = end
	}
	return nil
}

type pending struct {
	session *report.Session
	payload payload.Payload
	hash    string
	size    int
}

// dueParts picks the parts to send, by target ID: new ones, and changed ones
// whose session has ended or that weren't sent within MinResendInterval (any
// changed one when forced). targets lists every target with a token that some
// session has a part for, due or not.
func dueParts(sessions []*report.Session, opts Options) (map[string][]pending, []payload.Target, error) {
	due := map[string][]pending{}
	var targets []payload.Target
	seen := map[string]bool{}
	for _, s := range sessions {
		for _, part := range payload.Build(s, opts.Context) {
			if part.Target.Token == "" {
				continue // built for `report --json`, never sent
			}
			id := part.Target.ID()
			if !seen[id] {
				seen[id] = true
				targets = append(targets, part.Target)
			}
			data, err := json.Marshal(part.Payload)
			if err != nil {
				return nil, nil, err
			}
			hash := hashOf(data)
			if d := s.Deliveries[id]; d != nil {
				if d.Hash == hash {
					continue // unchanged since last delivered
				}
				if !opts.Force && !s.Ended && opts.Now.Sub(d.SentAt) < MinResendInterval {
					continue // changed, but sent recently and still going
				}
			}
			due[id] = append(due[id], pending{s, part.Payload, hash, len(data)})
		}
	}
	return due, targets, nil
}

// Pending counts the parts Deliver would send now.
func Pending(sessions []*report.Session, opts Options) int {
	due, _, _ := dueParts(sessions, opts)
	n := 0
	for _, d := range due {
		n += len(d)
	}
	return n
}

// errTooLarge is a 413 reply: the caller splits the batch, or rejects a lone
// session that is too large by itself.
var errTooLarge = errors.New("request too large (413)")

// upgradeRequiredError is a 426 reply: the server refuses this CLI version.
type upgradeRequiredError struct{ message string }

func (e *upgradeRequiredError) Error() string { return "upgrade required (426): " + e.message }

// post sends one batch and decodes the per-session results, and any upgrade
// notice for this CLI version.
func post(client *http.Client, t payload.Target, opts Options, sessions []payload.Payload) ([]Result, *UpgradeNotice, error) {
	body, err := json.Marshal(Request{SchemaVersion: SchemaVersion, CLIVersion: opts.CLIVersion, SentAt: opts.Now.UTC(), Sessions: sessions})
	if err != nil {
		return nil, nil, err
	}
	var gz bytes.Buffer
	w := gzip.NewWriter(&gz)
	w.Write(body)
	w.Close()

	req, err := http.NewRequest(http.MethodPost, strings.TrimRight(t.Endpoint, "/")+"/sessions", &gz)
	if err != nil {
		return nil, nil, err
	}
	req.Header.Set("Authorization", "Bearer "+t.Token)
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
func (st *backoff) fail(err error, now time.Time) {
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
