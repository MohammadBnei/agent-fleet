//go:build integration

package ratings

import (
	"context"
	"testing"
	"time"

	"github.com/MohammadBnei/agent-fleet/core/internal/dbtest"
)

func TestStoreRatingsUpsertAndStats(t *testing.T) {
	ctx := context.Background()
	pool := dbtest.NewPool(t)
	var sid string
	if err := pool.QueryRow(ctx, `INSERT INTO sessions (repo, description) VALUES ('r', 'd') RETURNING id::text`).Scan(&sid); err != nil {
		t.Fatal(err)
	}
	add := func(seq int, typ, text string) {
		t.Helper()
		if _, err := pool.Exec(ctx, `INSERT INTO transcript (session_id, seq, "from", text, type, idempotency_key) VALUES ($1, $2, 'agent', $3, $4, gen_random_uuid()::text)`, sid, seq, text, typ); err != nil {
			t.Fatal(err)
		}
	}
	add(1, "result", `{"modelUsage":{"claude-opus-5":{"inputTokens":100,"outputTokens":10,"costUSD":1}}}`)
	add(2, "result", `{"modelUsage":{"zai-org-glm-5":{"inputTokens":400,"outputTokens":50}}}`)
	add(3, "result", `{"isError":true,"modelUsage":{"zai-org-glm-5":{"inputTokens":500,"outputTokens":60}}}`)
	add(4, "assistant", `{}`)
	// The worker's own per-turn share wins over the cumulative counters.
	add(5, "result", `{"modelUsage":{"kimi-k2-7-code":{"inputTokens":999,"outputTokens":999}},"modelUsageDelta":{"kimi-k2-7-code":{"inputTokens":7,"outputTokens":3}}}`)

	s := NewStore(pool)
	if _, err := s.ResultText(ctx, sid, 4); err != ErrNoResult {
		t.Fatalf("non-result seq: %v", err)
	}
	if err := s.RateTurn(ctx, sid, 2, 1, "", "zai-org-glm-5"); err != nil {
		t.Fatal(err)
	}
	if err := s.RateTurn(ctx, sid, 2, -1, "worse", "zai-org-glm-5"); err != nil { // upsert, not a second row
		t.Fatal(err)
	}
	for _, sc := range []int{3, 4} {
		if err := s.RateSession(ctx, sid, sc, "", "zai-org-glm-5"); err != nil {
			t.Fatal(err)
		}
	}
	var n int
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM session_ratings WHERE session_id = $1`, sid).Scan(&n); err != nil || n != 2 {
		t.Fatalf("rows = %d (%v), want 1 thumb + 1 score", n, err)
	}
	if err := s.RateTurn(ctx, sid, 2, 7, "", ""); err == nil {
		t.Fatal("thumb value 7 passed the CHECK")
	}

	aggs, err := s.Stats(ctx, time.Now().Add(-time.Hour))
	if err != nil {
		t.Fatal(err)
	}
	g := aggs["zai-org-glm-5"]
	if g == nil || g.OutputTokens != 60 || g.InputTokens != 500 || g.Sessions != 1 || g.ErrorResults != 1 {
		t.Fatalf("glm = %+v", g)
	}
	if g.ThumbsDown != 1 || g.ThumbsUp != 0 || g.Scores != 1 || g.ScoreSum != 4 {
		t.Fatalf("ratings = %+v", g)
	}
	th, score, note, err := s.SessionRatings(ctx, sid)
	if err != nil || len(th) != 1 || th[0] != (Thumb{2, -1}) || score != 4 || note != "" {
		t.Fatalf("read-back = %v %d %q %v", th, score, note, err)
	}
	if k := aggs["kimi-k2-7-code"]; k == nil || k.InputTokens != 7 || k.OutputTokens != 3 {
		t.Fatalf("kimi = %+v (delta not used)", k)
	}
	if o := aggs["claude-opus-5"]; o == nil || o.SDKCostUSD != 1 {
		t.Fatalf("opus = %+v", o)
	}
}

func TestTranscriptAcceptsModelSwitchType(t *testing.T) {
	ctx := context.Background()
	pool := dbtest.NewPool(t)
	var sid string
	if err := pool.QueryRow(ctx, `INSERT INTO sessions (repo, description) VALUES ('r', 'd') RETURNING id::text`).Scan(&sid); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `INSERT INTO transcript (session_id, seq, "from", text, type, idempotency_key) VALUES ($1, 1, 'fleet', '{}', 'model_switch', 'k')`, sid); err != nil {
		t.Fatalf("model_switch rejected by the CHECK: %v", err)
	}
}
