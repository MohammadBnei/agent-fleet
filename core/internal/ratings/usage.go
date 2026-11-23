// Package ratings stores thumbs and scores on sessions and aggregates the
// per-model usage that result transcript entries carry.
package ratings

import (
	"encoding/json"
	"sort"
)

// ModelUse is one model's counters in a result entry's modelUsage.
type ModelUse struct {
	InputTokens         int64   `json:"inputTokens"`
	OutputTokens        int64   `json:"outputTokens"`
	CacheReadTokens     int64   `json:"cacheReadInputTokens"`
	CacheCreationTokens int64   `json:"cacheCreationInputTokens"`
	CostUSD             float64 `json:"costUSD"`
}

// Prompt is every input token the model saw, cached or not.
func (u ModelUse) Prompt() int64 { return u.InputTokens + u.CacheReadTokens + u.CacheCreationTokens }

// Result is the part of a result entry's JSON text this package reads.
type Result struct {
	IsError    bool                `json:"isError"`
	Usage      TopUsage            `json:"usage"`
	ModelUsage map[string]ModelUse `json:"modelUsage"`
	// Model is the pod's model, stamped by the worker; "" on older results.
	Model string `json:"model"`
	// ContextTokens is the size of the last API call's context, which the
	// roll-up in Usage is not; 0 when absent (older results).
	ContextTokens int64 `json:"contextTokens"`
	// ModelUsageDelta is this turn's own share of ModelUsage, from the worker.
	ModelUsageDelta map[string]ModelUse `json:"modelUsageDelta"`
}

// Turn is the model that produced the result: the stamped one, else the
// dominant model of the turn's own delta, else of the cumulative counters.
func (r Result) Turn() string {
	if r.Model != "" {
		return r.Model
	}
	if m := DominantModel(r.ModelUsageDelta); m != "" {
		return m
	}
	return DominantModel(r.ModelUsage)
}

// TopUsage is the result's top-level usage (snake_case, the SDK's own).
type TopUsage struct {
	InputTokens         int64 `json:"input_tokens"`
	CacheReadTokens     int64 `json:"cache_read_input_tokens"`
	CacheCreationTokens int64 `json:"cache_creation_input_tokens"`
}

// Prompt is the input side of the result, cache included.
func (u TopUsage) Prompt() int64 { return u.InputTokens + u.CacheReadTokens + u.CacheCreationTokens }

// ParseResult reads a result entry's text; ok is false when it is not JSON.
func ParseResult(text string) (r Result, ok bool) {
	return r, json.Unmarshal([]byte(text), &r) == nil
}

// DominantModel is the modelUsage key with the most output tokens (ties: the
// smaller name), "" when there is none: the model that produced the result.
func DominantModel(mu map[string]ModelUse) string {
	best, bestOut := "", int64(-1)
	for _, m := range sortedKeys(mu) {
		if mu[m].OutputTokens > bestOut {
			best, bestOut = m, mu[m].OutputTokens
		}
	}
	return best
}

// MostUsed is the model with the most output tokens over a session's results
// (deltas, so a resume's counter restart does not skew it), "" for none.
func MostUsed(results []Result) string {
	totals := map[string]ModelUse{}
	for _, d := range Deltas(results) {
		for m, u := range d {
			t := totals[m]
			t.OutputTokens += u.OutputTokens
			totals[m] = t
		}
	}
	return DominantModel(totals)
}

// Deltas turns a session's results, in seq order, into per-result per-model
// usage. The SDK's modelUsage is cumulative for its process, so a result's own
// share is the difference from the previous result; a counter that went down
// is a resumed process restarting at zero and counts whole.
//
// ponytail: a restart whose first result already exceeds the old total reads
// as continuous and undercounts once; the worker's per-turn deltas end this.
func Deltas(results []Result) []map[string]ModelUse {
	out := make([]map[string]ModelUse, len(results))
	prev := map[string]ModelUse{}
	for i, r := range results {
		d := map[string]ModelUse{}
		for m, cur := range r.ModelUsage {
			p := prev[m]
			if cur.OutputTokens < p.OutputTokens || cur.Prompt() < p.Prompt() {
				p = ModelUse{}
			}
			d[m] = ModelUse{
				InputTokens:         cur.InputTokens - p.InputTokens,
				OutputTokens:        cur.OutputTokens - p.OutputTokens,
				CacheReadTokens:     cur.CacheReadTokens - p.CacheReadTokens,
				CacheCreationTokens: cur.CacheCreationTokens - p.CacheCreationTokens,
				CostUSD:             max(0, cur.CostUSD-p.CostUSD),
			}
			prev[m] = cur
		}
		out[i] = d
	}
	return out
}

func sortedKeys(m map[string]ModelUse) []string {
	ks := make([]string, 0, len(m))
	for k := range m {
		ks = append(ks, k)
	}
	sort.Strings(ks)
	return ks
}
