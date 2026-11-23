package coreserver

import (
	"context"
	"encoding/json"
	"log/slog"
	"sort"
	"sync"

	"github.com/MohammadBnei/agent-fleet/core/internal/models"
)

// usageRow is one model's line in a worker result's modelUsage.
type usageRow struct {
	Model                    string
	Route                    string
	Input, Output, CacheRead int64
	BilledUSD, ListUSD       float64
}

// usageRows reads a worker "result" entry body into one row per model, sorted by model.
// The rows are the turn's own share: the worker sends modelUsageDelta next to the SDK's
// process-cumulative modelUsage. Nil for a body without the delta (an older worker).
func usageRows(body string) []usageRow {
	var b struct {
		Delta map[string]struct {
			InputTokens              int64   `json:"inputTokens"`
			OutputTokens             int64   `json:"outputTokens"`
			CacheReadInputTokens     int64   `json:"cacheReadInputTokens"`
			CacheCreationInputTokens int64   `json:"cacheCreationInputTokens"`
			CostUSD                  float64 `json:"costUSD"`
		} `json:"modelUsageDelta"`
	}
	if json.Unmarshal([]byte(body), &b) != nil {
		return nil
	}
	var out []usageRow
	for m, u := range b.Delta {
		r := usageRow{Model: m, Route: models.Route(m), Input: u.InputTokens, Output: u.OutputTokens, CacheRead: u.CacheReadInputTokens}
		r.BilledUSD = models.BilledUSD(m, u.InputTokens, u.OutputTokens, u.CacheReadInputTokens, u.CacheCreationInputTokens)
		r.ListUSD = u.CostUSD // subscription: the SDK's price for that model
		if r.Route == models.RouteGateway {
			r.ListUSD = r.BilledUSD // the SDK cost is wrong for unknown ids
		}
		out = append(out, r)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Model < out[j].Model })
	return out
}

// loggedSeqs holds the result seqs whose usage is logged, per session. A pushResult
// retry gets the existing seq back from Append and must not log twice; any other
// seq, lower or higher than one seen, is a real result.
// ponytail: in memory and capped (a session's set resets at 64 seqs, the whole map at
// 4096 sessions), so a core restart or a retry arriving after that many results can
// log one result again. The Store has no "existing row" signal to key off instead.
var (
	loggedMu   sync.Mutex
	loggedSeqs = map[string]map[int64]struct{}{}
)

// logUsage writes one "usage" line per model for a result entry: the line Grafana reads.
func logUsage(ctx context.Context, sessionID, body string, seq int64) {
	loggedMu.Lock()
	set := loggedSeqs[sessionID]
	if set == nil {
		if len(loggedSeqs) >= 4096 {
			clear(loggedSeqs)
		}
		set = map[int64]struct{}{}
		loggedSeqs[sessionID] = set
	} else if len(set) >= 64 {
		clear(set)
	}
	_, dup := set[seq]
	set[seq] = struct{}{}
	loggedMu.Unlock()
	if dup {
		return
	}
	for _, r := range usageRows(body) {
		slog.InfoContext(ctx, "usage", "sessionId", sessionID, "model", r.Model, "route", r.Route,
			"inputTokens", r.Input, "outputTokens", r.Output, "cacheReadTokens", r.CacheRead,
			"billedUsd", r.BilledUSD, "listUsd", r.ListUSD)
	}
}
