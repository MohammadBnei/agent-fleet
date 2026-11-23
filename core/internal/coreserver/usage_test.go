package coreserver

import (
	"context"
	"log/slog"
	"math"
	"sync"
	"sync/atomic"
	"testing"
)

func TestUsageRows(t *testing.T) {
	body := `{"subtype":"success","modelUsageDelta":{
	  "claude-opus-5":{"inputTokens":10,"outputTokens":5,"cacheReadInputTokens":100,"costUSD":0.42},
	  "zai-org-glm-5":{"inputTokens":1000000,"outputTokens":500000,"cacheReadInputTokens":0,"costUSD":99}}}`
	rows := usageRows(body)
	if len(rows) != 2 || rows[0].Model != "claude-opus-5" || rows[1].Model != "zai-org-glm-5" {
		t.Fatalf("rows = %+v", rows)
	}
	if r := rows[0]; r.Route != "subscription" || r.BilledUSD != 0 || r.ListUSD != 0.42 || r.CacheRead != 100 {
		t.Errorf("subscription row = %+v", r)
	}
	if r := rows[1]; r.Route != "gateway" || math.Abs(r.BilledUSD-2.6) > 1e-9 || r.ListUSD != r.BilledUSD {
		t.Errorf("gateway row = %+v (SDK costUSD must be ignored)", r)
	}
	for _, b := range []string{`{}`, `not json`, `{"modelUsageDelta":{}}`} {
		if got := usageRows(b); len(got) != 0 {
			t.Errorf("%q: %+v", b, got)
		}
	}
}

func TestUsageRowsPriceCacheReadsAtTheCachedRate(t *testing.T) {
	body := `{"modelUsageDelta":{"deepseek-v4-1-flash":{"inputTokens":158084,"outputTokens":19734,"cacheReadInputTokens":2409122,"cacheCreationInputTokens":0,"costUSD":2.67}}}`
	rows := usageRows(body)
	if len(rows) != 1 || math.Abs(rows[0].BilledUSD-0.0891) > 0.001 {
		t.Errorf("rows = %+v, want billed about 0.089", rows)
	}
}

type countHandler struct{ n *atomic.Int64 }

func (h countHandler) Enabled(context.Context, slog.Level) bool { return true }
func (h countHandler) Handle(_ context.Context, r slog.Record) error {
	if r.Message == "usage" {
		h.n.Add(1)
	}
	return nil
}
func (h countHandler) WithAttrs([]slog.Attr) slog.Handler { return h }
func (h countHandler) WithGroup(string) slog.Handler      { return h }

func resetLogged(id string) {
	loggedMu.Lock()
	delete(loggedSeqs, id)
	loggedMu.Unlock()
}

// Concurrent retries of one seq log once; a lower seq arriving later is a real result.
func TestLogUsageDedupesTheExactSeq(t *testing.T) {
	var n atomic.Int64
	old := slog.Default()
	slog.SetDefault(slog.New(countHandler{&n}))
	defer slog.SetDefault(old)
	resetLogged("race")
	body := `{"modelUsageDelta":{"claude-opus-5":{"inputTokens":1}}}`
	var wg sync.WaitGroup
	for i := 0; i < 50; i++ {
		wg.Add(1)
		go func() { defer wg.Done(); logUsage(context.Background(), "race", body, 7) }()
	}
	wg.Wait()
	if n.Load() != 1 {
		t.Fatalf("logged %d times for one seq, want 1", n.Load())
	}
	logUsage(context.Background(), "race", body, 3) // lower, later: logs
	logUsage(context.Background(), "race", body, 3) // its retry: skipped
	if n.Load() != 2 {
		t.Fatalf("logged %d, want 2", n.Load())
	}
	for seq := int64(10); seq < 300; seq++ { // the set stays bounded
		logUsage(context.Background(), "race", `{}`, seq)
	}
	loggedMu.Lock()
	size := len(loggedSeqs["race"])
	loggedMu.Unlock()
	if size > 64 {
		t.Fatalf("set grew to %d", size)
	}
}
