package deliver

import (
	"compress/gzip"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/swarmia/swarmia-cli/internal/payload"
	"github.com/swarmia/swarmia-cli/internal/report"
)

var now = time.Date(2026, 10, 1, 12, 0, 0, 0, time.UTC)

// server is a fake ingest endpoint recording what it receives. reply decides
// the response for each request; by default every session is accepted.
type server struct {
	mu       sync.Mutex
	requests []Request
	tokens   []string // each request's bearer token
	reply    func(req Request, w http.ResponseWriter)
	srv      *httptest.Server
}

func newServer(t *testing.T) *server {
	s := &server{}
	s.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost || r.URL.Path != "/cli/sessions" {
			t.Errorf("unexpected %s %s", r.Method, r.URL.Path)
		}
		token, ok := strings.CutPrefix(r.Header.Get("Authorization"), "Bearer ")
		if !ok {
			t.Errorf("Authorization = %q", r.Header.Get("Authorization"))
		}
		if r.Header.Get("Content-Encoding") != "gzip" || r.Header.Get("Content-Type") != "application/json" {
			t.Errorf("headers: %v", r.Header)
		}
		if !strings.HasPrefix(r.Header.Get("User-Agent"), "swarmia-cli/") {
			t.Errorf("User-Agent = %q", r.Header.Get("User-Agent"))
		}
		zr, err := gzip.NewReader(r.Body)
		if err != nil {
			t.Fatal(err)
		}
		var req Request
		if err := json.NewDecoder(zr).Decode(&req); err != nil {
			t.Fatal(err)
		}
		s.mu.Lock()
		s.requests = append(s.requests, req)
		s.tokens = append(s.tokens, token)
		reply := s.reply
		s.mu.Unlock()
		if reply == nil {
			reply = acceptAll
		}
		reply(req, w)
	}))
	t.Cleanup(s.srv.Close)
	return s
}

func acceptAll(req Request, w http.ResponseWriter) {
	var resp Response
	for _, p := range req.Sessions {
		resp.Results = append(resp.Results, Result{SessionID: p.SessionID, Status: "accepted"})
	}
	json.NewEncoder(w).Encode(resp)
}

func (s *server) sessionsSent() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	var ids []string
	for _, r := range s.requests {
		for _, p := range r.Sessions {
			ids = append(ids, p.SessionID)
		}
	}
	return ids
}

// target is the server with token "tok".
func (s *server) target() payload.Target {
	return payload.Target{Endpoint: s.srv.URL + "/cli", Token: "tok"}
}

// opts delivers sessions, all in the repository /repo, to the server.
func (s *server) opts(t *testing.T, at time.Time) Options {
	return Options{CLIVersion: "test", StateDir: t.TempDir(), Now: at, Context: contextFor(map[string]payload.Target{"/repo": s.target()})}
}

// contextFor is a payload.Context in which each repository root, named after
// its directory, is configured with a target.
func contextFor(targets map[string]payload.Target) payload.Context {
	return payload.Context{Repo: func(root string) payload.RepoInfo {
		t, ok := targets[root]
		if !ok {
			return payload.RepoInfo{}
		}
		return payload.RepoInfo{Slug: "github.com/acme" + root, Target: t, Prompts: true}
	}}
}

// sess is a session started in /repo.
func sess(id string, ended bool) *report.Session {
	return &report.Session{ID: id, Agent: "claude-code", StartedAt: now, EndedAt: now, Ended: ended, Root: "/repo",
		Prompts: []report.Prompt{{At: now, Text: "hello " + id}}}
}

// delivery is what s records about its delivery to the server.
func (s *server) delivery(sn *report.Session) *report.Delivery {
	return sn.Deliveries[s.target().ID()]
}

// backoffOf is the server's backoff in the delivery state in dir.
func (s *server) backoffOf(dir string) backoff {
	if b := loadState(dir).Targets[s.target().ID()]; b != nil {
		return *b
	}
	return backoff{}
}

func TestNoTokenSendsNothing(t *testing.T) {
	srv := newServer(t)
	o := srv.opts(t, now)
	o.Context = contextFor(map[string]payload.Target{"/repo": {Endpoint: srv.target().Endpoint}})
	out, err := Deliver([]*report.Session{sess("a", true)}, o)
	if err != nil || out.Skipped != NoToken || len(srv.sessionsSent()) != 0 {
		t.Errorf("out=%+v err=%v sent=%v", out, err, srv.sessionsSent())
	}
}

func TestSendsChangedSessionsOnce(t *testing.T) {
	srv := newServer(t)
	a, b := sess("a", true), sess("b", false)
	o := srv.opts(t, now)

	out, err := Deliver([]*report.Session{a, b}, o)
	if err != nil || out.Accepted != 2 || len(srv.requests) != 1 {
		t.Fatalf("first run: out=%+v err=%v requests=%d", out, err, len(srv.requests))
	}
	if srv.requests[0].SchemaVersion != SchemaVersion || srv.delivery(a) == nil || srv.delivery(a).Rejected != "" {
		t.Errorf("request/delivery: %+v %+v", srv.requests[0].SchemaVersion, srv.delivery(a))
	}

	// Unchanged: nothing to send.
	if out, _ := Deliver([]*report.Session{a, b}, o); out.Sent != 0 {
		t.Errorf("unchanged sessions resent: %+v", out)
	}

	// Both change. b is still going and was sent moments ago: throttled. a
	// has ended: sent.
	a.Prompts = append(a.Prompts, report.Prompt{At: now, Text: "more"})
	b.Prompts = append(b.Prompts, report.Prompt{At: now, Text: "more"})
	o.Now = now.Add(time.Minute)
	Deliver([]*report.Session{a, b}, o)
	if got := srv.sessionsSent(); strings.Join(got, ",") != "a,b,a" {
		t.Errorf("sent = %v, want a,b then a", got)
	}
	// After the resend interval, b goes too.
	o.Now = now.Add(MinResendInterval + time.Minute)
	Deliver([]*report.Session{a, b}, o)
	if got := srv.sessionsSent(); strings.Join(got, ",") != "a,b,a,b" {
		t.Errorf("sent = %v, want b resent after the interval", got)
	}
}

func TestRejectedSessionsAreNotRetriedUntilChanged(t *testing.T) {
	srv := newServer(t)
	srv.reply = func(req Request, w http.ResponseWriter) {
		var resp Response
		for _, p := range req.Sessions {
			r := Result{SessionID: p.SessionID, Status: "accepted"}
			if p.SessionID == "bad" {
				r = Result{SessionID: p.SessionID, Status: "rejected", Error: "repository not in organization"}
			}
			resp.Results = append(resp.Results, r)
		}
		json.NewEncoder(w).Encode(resp)
	}
	bad, good := sess("bad", true), sess("good", true)
	o := srv.opts(t, now)
	out, _ := Deliver([]*report.Session{bad, good}, o)
	if out.Rejected != 1 || out.Accepted != 1 || srv.delivery(bad).Rejected != "repository not in organization" {
		t.Errorf("out=%+v delivery=%+v", out, srv.delivery(bad))
	}
	if out, _ := Deliver([]*report.Session{bad, good}, o); out.Sent != 0 {
		t.Errorf("rejected session retried unchanged: %+v", out)
	}
}

// If the server does return per-session results, a session it left out is
// retried on the next run.
func TestMissingResultIsRetried(t *testing.T) {
	srv := newServer(t)
	srv.reply = func(req Request, w http.ResponseWriter) { w.Write([]byte(`{"results":[]}`)) }
	a := sess("a", true)
	o := srv.opts(t, now)
	Deliver([]*report.Session{a}, o)
	if srv.delivery(a) != nil {
		t.Fatalf("marked delivered without a result: %+v", srv.delivery(a))
	}
	srv.reply = nil
	Deliver([]*report.Session{a}, o)
	if srv.delivery(a) == nil || len(srv.sessionsSent()) != 2 {
		t.Errorf("not retried: delivery=%+v sent=%v", srv.delivery(a), srv.sessionsSent())
	}
}

func TestServerErrorsBackOff(t *testing.T) {
	srv := newServer(t)
	srv.reply = func(req Request, w http.ResponseWriter) { http.Error(w, "boom", http.StatusInternalServerError) }
	a := sess("a", true)
	o := srv.opts(t, now)
	if _, err := Deliver([]*report.Session{a}, o); err == nil || !strings.Contains(err.Error(), "HTTP 500") {
		t.Fatalf("err = %v", err)
	}
	if srv.delivery(a) != nil {
		t.Error("failed attempt marked the session delivered")
	}
	// Inside the backoff window: skipped without a request.
	srv.reply = nil
	o.Now = now.Add(30 * time.Second)
	out, _ := Deliver([]*report.Session{a}, o)
	if out.Skipped == "" || len(srv.requests) != 1 {
		t.Errorf("didn't back off: %+v, %d requests", out, len(srv.requests))
	}
	// After it, it goes through and the backoff resets.
	o.Now = now.Add(2 * time.Minute)
	if out, err := Deliver([]*report.Session{a}, o); err != nil || out.Accepted != 1 {
		t.Errorf("after backoff: %+v %v", out, err)
	}
	if b := srv.backoffOf(o.StateDir); b.Failures != 0 {
		t.Errorf("backoff not reset: %+v", b)
	}
}

func TestAuthFailureBacksOffLongAndRetryAfterIsHonoured(t *testing.T) {
	for _, tc := range []struct {
		status     int
		retryAfter string
		want       time.Duration
	}{
		{http.StatusUnauthorized, "", authBackoff},
		{http.StatusTooManyRequests, "120", 2 * time.Minute},
		{http.StatusServiceUnavailable, "", time.Minute},
	} {
		srv := newServer(t)
		srv.reply = func(req Request, w http.ResponseWriter) {
			if tc.retryAfter != "" {
				w.Header().Set("Retry-After", tc.retryAfter)
			}
			w.WriteHeader(tc.status)
		}
		o := srv.opts(t, now)
		Deliver([]*report.Session{sess("a", true)}, o)
		if got := srv.backoffOf(o.StateDir).NextAttempt.Sub(now); got != tc.want {
			t.Errorf("HTTP %d: next attempt in %v, want %v", tc.status, got, tc.want)
		}
	}
}

// 413 splits the batch; a session too large on its own is rejected.
func TestTooLargeSplitsBatches(t *testing.T) {
	srv := newServer(t)
	srv.reply = func(req Request, w http.ResponseWriter) {
		for _, p := range req.Sessions {
			if p.SessionID == "huge" || len(req.Sessions) > 2 {
				w.WriteHeader(http.StatusRequestEntityTooLarge)
				return
			}
		}
		acceptAll(req, w)
	}
	var sessions []*report.Session
	for i := range 4 {
		sessions = append(sessions, sess(fmt.Sprint("s", i), true))
	}
	huge := sess("huge", true)
	sessions = append(sessions, huge)

	out, err := Deliver(sessions, srv.opts(t, now))
	if err != nil {
		t.Fatal(err)
	}
	if out.Accepted != 4 || out.Rejected != 1 || srv.delivery(huge).Rejected != "payload too large" {
		t.Errorf("out=%+v huge=%+v", out, srv.delivery(huge))
	}
}

func TestForceIgnoresBackoffAndThrottle(t *testing.T) {
	srv := newServer(t)
	a := sess("a", false)
	o := srv.opts(t, now)
	Deliver([]*report.Session{a}, o)
	a.Prompts = append(a.Prompts, report.Prompt{At: now, Text: "more"})
	saveState(o.StateDir, state{Targets: map[string]*backoff{srv.target().ID(): {Failures: 3, NextAttempt: now.Add(time.Hour), LastError: "x"}}})
	o.Force, o.Now = true, now.Add(time.Second)
	if out, err := Deliver([]*report.Session{a}, o); err != nil || out.Accepted != 1 {
		t.Errorf("forced send: %+v %v", out, err)
	}
}

// The asynchronous endpoint answers with a bare 202: every session in the
// batch counts as received.
func TestBare2xxMarksBatchReceived(t *testing.T) {
	for _, status := range []int{http.StatusOK, http.StatusAccepted, http.StatusNoContent} {
		srv := newServer(t)
		srv.reply = func(req Request, w http.ResponseWriter) { w.WriteHeader(status) }
		a, b := sess("a", true), sess("b", true)
		out, err := Deliver([]*report.Session{a, b}, srv.opts(t, now))
		if err != nil || out.Accepted != 2 || srv.delivery(a) == nil || srv.delivery(b) == nil {
			t.Errorf("HTTP %d: out=%+v err=%v", status, out, err)
		}
	}
}

// The server refuses this CLI version with 426: nothing more is sent from it,
// sessions aren't marked, other versions sharing the state still send, and a
// manual send asks again.
func TestUpgradeRequiredStopsThisVersion(t *testing.T) {
	srv := newServer(t)
	srv.reply = func(req Request, w http.ResponseWriter) {
		if req.CLIVersion != "test" {
			acceptAll(req, w)
			return
		}
		w.WriteHeader(http.StatusUpgradeRequired)
		w.Write([]byte(`{"message":"swarmia-cli 0.1.0 sends broken payloads;\n\u001b[31mupgrade @swarmia/cli"}`))
	}
	a := sess("a", true)
	o := srv.opts(t, now)
	_, err := Deliver([]*report.Session{a}, o)
	if !errors.Is(err, ErrUpgradeRequired) {
		t.Fatalf("err = %v, want ErrUpgradeRequired", err)
	}
	if srv.delivery(a) != nil {
		t.Error("refused session marked delivered")
	}
	u, ok := UpgradeFor(o.StateDir, "test")
	if want := "swarmia-cli 0.1.0 sends broken payloads; [31mupgrade @swarmia/cli"; !ok || !u.Required || u.Message != want {
		t.Errorf("upgrade = %+v, %v; want required with %q (no escape characters)", u, ok, want)
	}
	if b := srv.backoffOf(o.StateDir); b.Failures != 0 || !b.NextAttempt.IsZero() {
		t.Errorf("426 counted as a failure to back off from: %+v", b)
	}

	// Later runs of this version send nothing, without asking.
	o.Now = now.Add(24 * time.Hour)
	if out, err := Deliver([]*report.Session{a}, o); err != nil || !strings.Contains(out.Skipped, "upgrade @swarmia/cli") || len(srv.requests) != 1 {
		t.Errorf("refused version: %+v, %v, %d requests; want skipped without a request", out, err, len(srv.requests))
	}

	// Another version, sharing the state, sends, and keeps the refusal.
	other := o
	other.CLIVersion = "newer"
	if out, err := Deliver([]*report.Session{sess("b", true)}, other); err != nil || out.Accepted != 1 {
		t.Errorf("newer version: %+v, %v", out, err)
	}
	if u, _ := UpgradeFor(o.StateDir, "test"); !u.Required {
		t.Error("newer version dropped the older one's refusal")
	}

	// A manual send asks again; if the server now accepts, the refusal goes.
	srv.reply = nil
	o.Force = true
	if out, err := Deliver([]*report.Session{a}, o); err != nil || out.Accepted != 1 {
		t.Errorf("forced: %+v, %v", out, err)
	}
	if _, ok := UpgradeFor(o.StateDir, "test"); ok {
		t.Error("refusal kept after the server accepted this version")
	}
}

// An upgrade notice in a 2xx reply is recorded and doesn't stop delivery; a
// reply without one clears it.
func TestUpgradeNoticeIsRecorded(t *testing.T) {
	srv := newServer(t)
	srv.reply = func(req Request, w http.ResponseWriter) {
		w.Write([]byte(`{"upgrade":{"message":"swarmia-cli 0.1.0 won't be accepted after 2026-12-01"}}`))
	}
	o := srv.opts(t, now)
	if out, err := Deliver([]*report.Session{sess("a", true)}, o); err != nil || out.Accepted != 1 {
		t.Fatalf("with a notice: %+v, %v", out, err)
	}
	if u, ok := UpgradeFor(o.StateDir, "test"); !ok || u.Required || !strings.Contains(u.Message, "2026-12-01") {
		t.Errorf("upgrade = %+v, %v; want the notice, not required", u, ok)
	}
	srv.reply = func(req Request, w http.ResponseWriter) {} // a bare 200
	o.Now = now.Add(time.Hour)
	if out, err := Deliver([]*report.Session{sess("b", true)}, o); err != nil || out.Accepted != 1 {
		t.Fatalf("bare 200: %+v, %v", out, err)
	}
	if u, ok := UpgradeFor(o.StateDir, "test"); ok {
		t.Errorf("notice kept after a reply without one: %+v", u)
	}
}

func TestDisplayable(t *testing.T) {
	for in, want := range map[string]string{
		"":                          "fallback",
		" \x1b]0;title\x07 \n ":     "]0;title",
		"upgrade\r\nnow\tplease":    "upgrade now please",
		strings.Repeat("ä", 200):    strings.Repeat("ä", maxMessage/2) + "…",
		"plain message, unchanged.": "plain message, unchanged.",
	} {
		if got := displayable(in, "fallback"); got != want {
			t.Errorf("displayable(%q) = %q, want %q", in, got, want)
		}
	}
}

// twoTargets has a session in /repo (token "tok") that also worked in
// /client (token "other") and /oss (no token).
func twoTargets(srv *server) (*report.Session, payload.Target, func(*testing.T) Options) {
	other := payload.Target{Endpoint: srv.target().Endpoint, Token: "other"}
	s := sess("a", true)
	s.Tools = []report.Tool{
		{At: now, Name: "Edit", Root: "/client", File: "/client/x.go"},
		{At: now, Name: "Edit", Root: "/oss", File: "/oss/y.go"},
	}
	opts := func(t *testing.T) Options {
		o := srv.opts(t, now)
		o.Context = contextFor(map[string]payload.Target{
			"/repo": srv.target(), "/client": other, "/oss": {Endpoint: srv.target().Endpoint},
		})
		return o
	}
	return s, other, opts
}

// Each target gets its part of a session, with its own token; a part with no
// token is never sent. Each part's delivery is recorded on its own.
func TestPartsGoToTheirTargets(t *testing.T) {
	srv := newServer(t)
	s, other, opts := twoTargets(srv)
	o := opts(t)
	out, err := Deliver([]*report.Session{s}, o)
	if err != nil || out.Sent != 2 || len(srv.requests) != 2 {
		t.Fatalf("out=%+v err=%v requests=%d", out, err, len(srv.requests))
	}
	got := map[string]string{}
	for i, req := range srv.requests {
		var repos []string
		for _, r := range req.Sessions[0].Repos {
			repos = append(repos, r.Repo)
		}
		got[srv.tokens[i]] = strings.Join(repos, ",")
	}
	if want := map[string]string{"tok": "github.com/acme/repo", "other": "github.com/acme/client"}; fmt.Sprint(got) != fmt.Sprint(want) {
		t.Errorf("repos by token = %v, want %v", got, want)
	}
	if srv.delivery(s) == nil || s.Deliveries[other.ID()] == nil || len(s.Deliveries) != 2 {
		t.Errorf("deliveries = %+v", s.Deliveries)
	}
	if out, _ := Deliver([]*report.Session{s}, o); out.Sent != 0 {
		t.Errorf("unchanged parts resent: %+v", out)
	}
	if n := Pending([]*report.Session{s}, o); n != 0 {
		t.Errorf("pending = %d, want 0", n)
	}
}

// A target that fails backs off on its own: the others are delivered, and
// later runs don't retry it until its backoff ends.
func TestFailingTargetDoesNotHoldUpOthers(t *testing.T) {
	srv := newServer(t)
	srv.reply = func(req Request, w http.ResponseWriter) {
		srv.mu.Lock()
		token := srv.tokens[len(srv.tokens)-1]
		srv.mu.Unlock()
		if token == "tok" { // the session's first target
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		acceptAll(req, w)
	}
	s, other, opts := twoTargets(srv)
	o := opts(t)
	out, err := Deliver([]*report.Session{s}, o)
	if err == nil || !strings.Contains(err.Error(), "HTTP 401") || out.Accepted != 1 {
		t.Fatalf("out=%+v err=%v, want one accepted and the 401", out, err)
	}
	if srv.delivery(s) != nil || s.Deliveries[other.ID()] == nil {
		t.Errorf("deliveries = %+v, want only other's", s.Deliveries)
	}
	if b := srv.backoffOf(o.StateDir); b.NextAttempt.Sub(now) != authBackoff {
		t.Errorf("tok's backoff = %+v", b)
	}
	if b := loadState(o.StateDir).Targets[other.ID()]; b != nil {
		t.Errorf("other backs off too: %+v", b)
	}

	// Both parts change; within the backoff only other's goes.
	s.Prompts = append(s.Prompts, report.Prompt{At: now, Text: "more"})
	o.Now = now.Add(time.Hour)
	if out, err := Deliver([]*report.Session{s}, o); err != nil || out.Sent != 1 || srv.tokens[len(srv.tokens)-1] != "other" {
		t.Errorf("out=%+v err=%v last token %q", out, err, srv.tokens[len(srv.tokens)-1])
	}
}
