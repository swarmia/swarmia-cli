package usage

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
)

// piInheritedEntries identifies the copied entries in a fork, without reading
// their message contents. If the parent is unavailable we cannot distinguish
// copied usage reliably, so the caller must not advance its offset.
func piInheritedEntries(path string) (map[string]bool, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	var header struct {
		Type   string `json:"type"`
		Parent string `json:"parentSession"`
	}
	if err := json.NewDecoder(f).Decode(&header); err != nil {
		return nil, err
	}
	if header.Type != "session" || header.Parent == "" {
		return nil, nil
	}
	if !filepath.IsAbs(header.Parent) || !strings.HasSuffix(header.Parent, ".jsonl") || filepath.Clean(header.Parent) == filepath.Clean(path) {
		return nil, fmt.Errorf("invalid Pi parent session path")
	}
	parent, err := os.Open(header.Parent)
	if err != nil {
		return nil, fmt.Errorf("reading Pi parent session: %w", err)
	}
	defer parent.Close()
	ids := make(map[string]bool)
	dec := json.NewDecoder(parent)
	for {
		var entry struct {
			Type string `json:"type"`
			ID   string `json:"id"`
		}
		err := dec.Decode(&entry)
		if err == io.EOF {
			break
		}
		if err != nil {
			return nil, fmt.Errorf("reading Pi parent entries: %w", err)
		}
		if entry.Type != "session" && entry.ID != "" {
			ids[entry.ID] = true
		}
	}
	return ids, nil
}

type piUsage struct {
	Input      int64 `json:"input"`
	Output     int64 `json:"output"`
	CacheRead  int64 `json:"cacheRead"`
	CacheWrite int64 `json:"cacheWrite"`
	Reasoning  int64 `json:"reasoning"`
}

func (st *fileState) piLine(line []byte, inherited map[string]bool) {
	if !bytes.Contains(line, usageMarker) {
		return
	}
	var e struct {
		Type    string   `json:"type"`
		ID      string   `json:"id"`
		Kind    string   `json:"kind"`
		Model   string   `json:"model"`
		Usage   *piUsage `json:"usage"`
		Message struct {
			Role  string   `json:"role"`
			Model string   `json:"model"`
			Usage *piUsage `json:"usage"`
		} `json:"message"`
	}
	if json.Unmarshal(line, &e) != nil || inherited[e.ID] {
		return
	}
	var model string
	var u *piUsage
	switch {
	case e.Type == "message" && e.Message.Role == "assistant":
		model, u = e.Message.Model, e.Message.Usage
	case e.Type == "usage" && e.Kind == "cache_warm":
		// Pi records each cache warm as a separate response with a model.
		model, u = e.Model, e.Usage
	default:
		// Tool and compaction usage can aggregate calls from different models
		// and lacks model/request counts. Do not invent per-model attribution.
		return
	}
	if model == "" || u == nil {
		return
	}
	if u.Input == 0 && u.Output == 0 && u.CacheRead == 0 && u.CacheWrite == 0 {
		return
	}
	st.addTotals(Totals{Model: model, Speed: "standard", Requests: 1,
		InputTokens: u.Input, OutputTokens: u.Output, CacheReadInputTokens: u.CacheRead,
		CacheCreation5mInputTokens: u.CacheWrite, ReasoningOutputTokens: u.Reasoning})
}
