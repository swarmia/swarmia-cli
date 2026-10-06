package deliver

import (
	"compress/gzip"
	"encoding/json"
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
	reply    func(req Request, w http.ResponseWriter)
	srv      *httptest.Server
}

func newServer(t *testing.T) *server {
	s := &server{}
	s.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost || r.URL.Path != "/cli/sessions" {
			t.Errorf("unexpected %s %s", r.Method, r.URL.Path)
		}
		if got := r.Header.Get("Authorization"); got != "Bearer tok" {
			t.Errorf("Authorization = %q", got)
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

func (s *server) opts(t *testing.T, at time.Time) Options {
	return Options{Endpoint: s.srv.URL + "/cli", Token: "tok", CLIVersion: "test", StateDir: t.TempDir(), Now: at,
		Context: payload.Context{Prompts: true}}
}

func sess(id string, ended bool) *report.Session {
	return &report.Session{ID: id, Agent: "claude-code", StartedAt: now, EndedAt: now, Ended: ended,
		Prompts: []report.Prompt{{At: now, Text: "hello " + id}}}
}

func TestNoTokenSendsNothing(t *testing.T) {
	srv := newServer(t)
	o := srv.opts(t, now)
	o.Token = ""
	out, err := Deliver([]*report.Session{sess("a", true)}, o)
	if err != nil || out.Skipped == "" || len(srv.sessionsSent()) != 0 {
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
	if srv.requests[0].SchemaVersion != SchemaVersion || a.Delivery == nil || a.Delivery.Rejected != "" {
		t.Errorf("request/delivery: %+v %+v", srv.requests[0].SchemaVersion, a.Delivery)
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
	if out.Rejected != 1 || out.Accepted != 1 || bad.Delivery.Rejected != "repository not in organization" {
		t.Errorf("out=%+v delivery=%+v", out, bad.Delivery)
	}
	if out, _ := Deliver([]*report.Session{bad, good}, o); out.Sent != 0 {
		t.Errorf("rejected session retried unchanged: %+v", out)
	}
}

// A session the server didn't answer for is retried on the next run.
func TestMissingResultIsRetried(t *testing.T) {
	srv := newServer(t)
	srv.reply = func(req Request, w http.ResponseWriter) { json.NewEncoder(w).Encode(Response{}) }
	a := sess("a", true)
	o := srv.opts(t, now)
	Deliver([]*report.Session{a}, o)
	if a.Delivery != nil {
		t.Fatalf("marked delivered without a result: %+v", a.Delivery)
	}
	srv.reply = nil
	Deliver([]*report.Session{a}, o)
	if a.Delivery == nil || len(srv.sessionsSent()) != 2 {
		t.Errorf("not retried: delivery=%+v sent=%v", a.Delivery, srv.sessionsSent())
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
	if a.Delivery != nil {
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
	if st := loadState(o.StateDir); st.Failures != 0 {
		t.Errorf("backoff not reset: %+v", st)
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
		if got := loadState(o.StateDir).NextAttempt.Sub(now); got != tc.want {
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
	if out.Accepted != 4 || out.Rejected != 1 || huge.Delivery.Rejected != "payload too large" {
		t.Errorf("out=%+v huge=%+v", out, huge.Delivery)
	}
}

func TestForceIgnoresBackoffAndThrottle(t *testing.T) {
	srv := newServer(t)
	a := sess("a", false)
	o := srv.opts(t, now)
	Deliver([]*report.Session{a}, o)
	a.Prompts = append(a.Prompts, report.Prompt{At: now, Text: "more"})
	saveState(o.StateDir, state{Failures: 3, NextAttempt: now.Add(time.Hour), LastError: "x"})
	o.Force, o.Now = true, now.Add(time.Second)
	if out, err := Deliver([]*report.Session{a}, o); err != nil || out.Accepted != 1 {
		t.Errorf("forced send: %+v %v", out, err)
	}
}
