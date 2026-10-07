// Package usage totals the tokens a coding agent's model calls consumed in a
// session, read from the agent's own transcript files. Hooks don't carry token
// counts; transcripts do.
//
// Only usage fields are parsed — never message content — and nothing but the
// totals is kept. Reading is incremental: each file keeps a byte offset, so a
// long session's transcript is read once overall, not once per run.
//
// Claude Code writes a transcript per session, plus one per subagent under
// <transcript>/subagents/. Each API response is written once per content
// block, so the same (message id, request id) appears on several consecutive
// lines with identical usage; only the first counts (the same rule as
// ccusage).
//
// Codex writes a "rollout" per session, with a token_count event after each
// model response holding the session's cumulative usage so far; the model is
// in the turn_context lines before it. A response's usage is the difference
// between consecutive totals. Codex counts cached input within input tokens,
// and reasoning within output tokens; Totals follows Claude's convention, input
// tokens excluding cache reads.
//
// Cost isn't computed here: prices change and depend on contracts, so the
// server derives cost from these counts.
package usage

import (
	"bytes"
	"encoding/json"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"
)

// Totals is one bucket of usage: a model at a speed, in the main session or in
// its subagents.
type Totals struct {
	Model    string `json:"model"`
	Speed    string `json:"speed,omitempty"` // "standard" or "fast" (priced differently)
	Subagent bool   `json:"subagent,omitempty"`

	Requests                   int64 `json:"requests"`
	InputTokens                int64 `json:"input_tokens"`
	OutputTokens               int64 `json:"output_tokens"`
	CacheReadInputTokens       int64 `json:"cache_read_input_tokens"`
	CacheCreation5mInputTokens int64 `json:"cache_creation_5m_input_tokens"`
	CacheCreation1hInputTokens int64 `json:"cache_creation_1h_input_tokens"`
	ReasoningOutputTokens      int64 `json:"reasoning_output_tokens,omitempty"` // part of OutputTokens (Codex)
	WebSearchRequests          int64 `json:"web_search_requests,omitempty"`
	WebFetchRequests           int64 `json:"web_fetch_requests,omitempty"`
}

func (t *Totals) add(o Totals) {
	t.Requests += o.Requests
	t.InputTokens += o.InputTokens
	t.OutputTokens += o.OutputTokens
	t.CacheReadInputTokens += o.CacheReadInputTokens
	t.CacheCreation5mInputTokens += o.CacheCreation5mInputTokens
	t.CacheCreation1hInputTokens += o.CacheCreation1hInputTokens
	t.ReasoningOutputTokens += o.ReasoningOutputTokens
	t.WebSearchRequests += o.WebSearchRequests
	t.WebFetchRequests += o.WebFetchRequests
}

// Tracker accumulates usage across a session's transcript files.
type Tracker struct {
	Files map[string]*fileState `json:"files"`
}

type fileState struct {
	Offset  int64              `json:"offset"`
	LastKey string             `json:"last_key,omitempty"`
	Totals  map[string]*Totals `json:"totals,omitempty"`
	// Codex: the model of the current turn, and the last cumulative total.
	Model     string      `json:"model,omitempty"`
	LastTotal *codexUsage `json:"last_total,omitempty"`
}

// Update reads whatever was appended to the session's transcript (and its
// subagents' transcripts) since the last call. A file that shrank was
// rewritten, so it is re-read from the start.
func (t *Tracker) Update(transcriptPath string) error {
	if !filepath.IsAbs(transcriptPath) || !strings.HasSuffix(transcriptPath, ".jsonl") {
		return nil // not a transcript path we recognise
	}
	if t.Files == nil {
		t.Files = map[string]*fileState{}
	}
	files := []string{transcriptPath}
	sub, _ := filepath.Glob(filepath.Join(strings.TrimSuffix(transcriptPath, ".jsonl"), "subagents", "*.jsonl"))
	files = append(files, sub...)
	var firstErr error
	for i, path := range files {
		st := t.Files[path]
		if st == nil {
			st = &fileState{}
			t.Files[path] = st
		}
		if err := st.read(path, i > 0); err != nil && firstErr == nil && !os.IsNotExist(err) {
			firstErr = err
		}
	}
	return firstErr
}

// Totals returns the usage summed over all files, one entry per (model,
// speed, subagent), sorted.
func (t *Tracker) Totals() []Totals {
	merged := map[string]*Totals{}
	for _, st := range t.Files {
		for k, v := range st.Totals {
			if m, ok := merged[k]; ok {
				m.add(*v)
			} else {
				c := *v
				merged[k] = &c
			}
		}
	}
	out := make([]Totals, 0, len(merged))
	for _, v := range merged {
		out = append(out, *v)
	}
	sort.Slice(out, func(a, b int) bool {
		if out[a].Subagent != out[b].Subagent {
			return !out[a].Subagent
		}
		if out[a].Model != out[b].Model {
			return out[a].Model < out[b].Model
		}
		return out[a].Speed < out[b].Speed
	})
	return out
}

// entry is the part of a transcript line usage needs.
type entry struct {
	Type      string `json:"type"`
	RequestID string `json:"requestId"`
	Message   struct {
		ID    string `json:"id"`
		Model string `json:"model"`
		Usage *struct {
			InputTokens              int64  `json:"input_tokens"`
			OutputTokens             int64  `json:"output_tokens"`
			CacheReadInputTokens     int64  `json:"cache_read_input_tokens"`
			CacheCreationInputTokens int64  `json:"cache_creation_input_tokens"`
			Speed                    string `json:"speed"`
			CacheCreation            *struct {
				Ephemeral5m int64 `json:"ephemeral_5m_input_tokens"`
				Ephemeral1h int64 `json:"ephemeral_1h_input_tokens"`
			} `json:"cache_creation"`
			ServerToolUse *struct {
				WebSearchRequests int64 `json:"web_search_requests"`
				WebFetchRequests  int64 `json:"web_fetch_requests"`
			} `json:"server_tool_use"`
		} `json:"usage"`
	} `json:"message"`
}

// usageMarker prefilters lines before JSON parsing: most transcript lines are
// tool results and attachments without usage. The others mark Codex's lines.
var (
	usageMarker       = []byte(`"usage"`)
	tokenCountMarker  = []byte(`"token_count"`)
	turnContextMarker = []byte(`"turn_context"`)
)

// codexUsage is Codex's token usage record.
type codexUsage struct {
	InputTokens           int64 `json:"input_tokens"`
	CachedInputTokens     int64 `json:"cached_input_tokens"`
	OutputTokens          int64 `json:"output_tokens"`
	ReasoningOutputTokens int64 `json:"reasoning_output_tokens"`
	TotalTokens           int64 `json:"total_tokens"`
}

// codexLine is the part of a Codex rollout line usage needs.
type codexLine struct {
	Type    string `json:"type"`
	Payload struct {
		Type  string `json:"type"`
		Model string `json:"model"`
		Info  *struct {
			TotalTokenUsage *codexUsage `json:"total_token_usage"`
		} `json:"info"`
	} `json:"payload"`
}

// codex folds in one rollout line if it's a turn context (the model) or a
// token count (the cumulative usage), and reports whether it was a Codex line.
func (st *fileState) codex(line []byte) bool {
	var l codexLine
	if json.Unmarshal(line, &l) != nil {
		return false
	}
	switch l.Type {
	case "turn_context":
		if l.Payload.Model != "" {
			st.Model = l.Payload.Model
		}
		return true
	case "event_msg":
	default:
		return false // e.g. a Claude Code line that mentions token_count
	}
	if l.Payload.Type != "token_count" || l.Payload.Info == nil || l.Payload.Info.TotalTokenUsage == nil {
		return true // e.g. a rate-limit update without usage
	}
	cur := *l.Payload.Info.TotalTokenUsage
	last := codexUsage{}
	if st.LastTotal != nil {
		last = *st.LastTotal
	}
	if cur.TotalTokens < last.TotalTokens {
		last = codexUsage{} // the counter restarted
	}
	if cur == last {
		return true // repeated, no new response
	}
	st.LastTotal = &cur
	model := st.Model
	if model == "" {
		model = "unknown"
	}
	cached := cur.CachedInputTokens - last.CachedInputTokens
	t := Totals{Model: model, Speed: "standard", Requests: 1,
		InputTokens:           cur.InputTokens - last.InputTokens - cached,
		CacheReadInputTokens:  cached,
		OutputTokens:          cur.OutputTokens - last.OutputTokens,
		ReasoningOutputTokens: cur.ReasoningOutputTokens - last.ReasoningOutputTokens}
	st.addTotals(t)
	return true
}

func (st *fileState) addTotals(t Totals) {
	bucket := t.Model + "\x00" + t.Speed
	if t.Subagent {
		bucket += "\x00sub"
	}
	if b, ok := st.Totals[bucket]; ok {
		b.add(t)
	} else {
		st.Totals[bucket] = &t
	}
}

func (st *fileState) read(path string, subagent bool) error {
	f, err := os.Open(path)
	if err != nil {
		return err
	}
	defer f.Close()
	fi, err := f.Stat()
	if err != nil {
		return err
	}
	if fi.Size() < st.Offset {
		*st = fileState{} // rewritten: start over
	}
	if fi.Size() == st.Offset {
		return nil
	}
	if _, err := f.Seek(st.Offset, io.SeekStart); err != nil {
		return err
	}
	data, err := io.ReadAll(f)
	if err != nil {
		return err
	}
	// Only complete lines; a partial last line is read next time.
	end := bytes.LastIndexByte(data, '\n')
	if end < 0 {
		return nil
	}
	if st.Totals == nil {
		st.Totals = map[string]*Totals{}
	}
	for _, line := range bytes.Split(data[:end], []byte("\n")) {
		if (bytes.Contains(line, tokenCountMarker) || bytes.Contains(line, turnContextMarker)) && st.codex(line) {
			continue
		}
		if !bytes.Contains(line, usageMarker) {
			continue
		}
		var e entry
		if json.Unmarshal(line, &e) != nil || e.Type != "assistant" || e.Message.Usage == nil {
			continue
		}
		key := e.Message.ID + ":" + e.RequestID
		if key == st.LastKey {
			continue // another content block of the same response
		}
		st.LastKey = key
		if e.Message.Model == "" || strings.HasPrefix(e.Message.Model, "<") {
			continue // e.g. <synthetic> placeholders for errors
		}
		u := e.Message.Usage
		speed := u.Speed
		if speed == "" {
			speed = "standard" // older transcripts omit it; fast mode always sets it
		}
		t := Totals{Model: e.Message.Model, Speed: speed, Subagent: subagent, Requests: 1,
			InputTokens: u.InputTokens, OutputTokens: u.OutputTokens, CacheReadInputTokens: u.CacheReadInputTokens}
		if c := u.CacheCreation; c != nil {
			t.CacheCreation5mInputTokens, t.CacheCreation1hInputTokens = c.Ephemeral5m, c.Ephemeral1h
		} else {
			t.CacheCreation5mInputTokens = u.CacheCreationInputTokens // the default cache duration
		}
		if s := u.ServerToolUse; s != nil {
			t.WebSearchRequests, t.WebFetchRequests = s.WebSearchRequests, s.WebFetchRequests
		}
		st.addTotals(t)
	}
	st.Offset += int64(end + 1)
	return nil
}
