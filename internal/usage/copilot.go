package usage

import (
	"bytes"
	"encoding/json"
	"io"
	"os"
	"strings"
	"time"
)

// copilotUsageMarker prefilters Copilot event lines: most are messages, tool
// calls and hook runs.
var copilotUsageMarker = []byte(`"session.usage_record"`)

// copilotUsageLine is the part of a Copilot session.usage_record event usage
// needs.
type copilotUsageLine struct {
	Type    string `json:"type"`
	AgentID string `json:"agentId"` // set for a subagent's requests
	Data    struct {
		Usage struct {
			Model            string `json:"model"`
			InputTokens      int64  `json:"inputTokens"`
			OutputTokens     int64  `json:"outputTokens"`
			CacheReadTokens  int64  `json:"cacheReadTokens"`
			CacheWriteTokens int64  `json:"cacheWriteTokens"`
			ReasoningTokens  int64  `json:"reasoningTokens"`
		} `json:"usage"`
	} `json:"data"`
}

// copilotLine folds in one event line if it's a request's usage.
func (st *fileState) copilotLine(line []byte) {
	if !bytes.Contains(line, copilotUsageMarker) {
		return
	}
	var l copilotUsageLine
	if json.Unmarshal(line, &l) != nil || l.Type != "session.usage_record" {
		return
	}
	u := l.Data.Usage
	model := u.Model
	if model == "" {
		model = "unknown"
	}
	st.addTotals(Totals{Model: model, Speed: "standard", Subagent: l.AgentID != "", Requests: 1,
		InputTokens:                max(u.InputTokens-u.CacheReadTokens-u.CacheWriteTokens, 0),
		CacheReadInputTokens:       u.CacheReadTokens,
		CacheCreation5mInputTokens: u.CacheWriteTokens,
		OutputTokens:               u.OutputTokens,
		ReasoningOutputTokens:      u.ReasoningTokens})
}

// copilotResponseTail is how much of the end of a Copilot session's events
// CopilotResponse reads: the turn's reply is among the last events when the
// turn ends.
const copilotResponseTail = 1 << 20

// copilotResponseWait is how long CopilotResponse waits for the reply to
// reach the file, polling every copilotResponsePoll: Copilot writes its events
// a little after it runs the turn's Stop hook.
var (
	copilotResponseWait = time.Second
	copilotResponsePoll = 50 * time.Millisecond
)

// CopilotResponse is the main agent's reply in the turn that just ended, read
// from a Copilot session's events when the turn ends: Copilot's Stop payload
// doesn't carry it. "" if the turn has none (it ended on a tool call), or the
// file can't be read.
func CopilotResponse(eventsPath string) string {
	if !strings.HasSuffix(eventsPath, ".jsonl") {
		return ""
	}
	deadline := time.Now().Add(copilotResponseWait)
	for {
		reply, ok := copilotTurnReply(eventsPath)
		if ok || time.Now().After(deadline) {
			return reply
		}
		time.Sleep(copilotResponsePoll)
	}
}

// copilotTurnReply is the main agent's last message with text in the turn
// that just ended, read from the end of the events file: the turn ends where
// Copilot ran its Stop hook (a hook.start of an agentStop hook), and began at
// the developer's last message. ok is false if the turn's end isn't written
// yet.
func copilotTurnReply(eventsPath string) (reply string, ok bool) {
	f, err := os.Open(eventsPath)
	if err != nil {
		return "", true // nothing to wait for
	}
	defer f.Close()
	if fi, err := f.Stat(); err == nil && fi.Size() > copilotResponseTail {
		if _, err := f.Seek(fi.Size()-copilotResponseTail, io.SeekStart); err != nil {
			return "", true
		}
	}
	data, err := io.ReadAll(f)
	if err != nil {
		return "", true
	}
	lines := bytes.Split(data, []byte("\n"))
	ended := false
	for i := len(lines) - 1; i >= 0; i-- {
		line := lines[i]
		if !bytes.Contains(line, []byte(`"user.message"`)) && !bytes.Contains(line, []byte(`"assistant.message"`)) &&
			!bytes.Contains(line, []byte(`"agentStop"`)) {
			continue
		}
		var l struct {
			Type    string `json:"type"`
			AgentID string `json:"agentId"`
			Data    struct {
				Content  string `json:"content"`
				HookType string `json:"hookType"`
			} `json:"data"`
		}
		if json.Unmarshal(line, &l) != nil || l.AgentID != "" {
			continue // a torn first line, or a subagent's
		}
		switch {
		case l.Type == "hook.start" && l.Data.HookType == "agentStop":
			ended = true
		case l.Type == "user.message":
			return "", ended // a turn without a reply, or its end isn't written yet
		case l.Type == "assistant.message" && ended && strings.TrimSpace(l.Data.Content) != "":
			return l.Data.Content, true
		}
	}
	return "", ended
}
