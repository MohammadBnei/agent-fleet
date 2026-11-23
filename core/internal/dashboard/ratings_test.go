package dashboard

import (
	"context"
	"strings"
	"testing"
	"time"

	"connectrpc.com/connect"

	"github.com/MohammadBnei/agent-fleet/core/internal/ratings"
	agentfleetv1 "github.com/MohammadBnei/agent-fleet/proto/gen/go/agentfleet/v1"
)

const sid = "6f1c1a52-0b0e-4a43-8f0a-1f3d5a9e7c11"

type fakeRatings struct {
	resultText string
	resultErr  error
	results    []ratings.Result
	turnModel  string
	turnValue  int
	scoreModel string
	scoreValue int
	aggs       map[string]*ratings.Agg
	rated      bool
}

func (f *fakeRatings) ResultText(context.Context, string, int64) (string, error) {
	return f.resultText, f.resultErr
}
func (f *fakeRatings) Results(context.Context, string) ([]ratings.Result, error) {
	return f.results, nil
}
func (f *fakeRatings) LastResult(context.Context, string) (ratings.Result, bool, error) {
	return ratings.Result{}, false, nil
}
func (f *fakeRatings) RateTurn(_ context.Context, _ string, _ int64, v int, _, model string) error {
	f.rated, f.turnValue, f.turnModel = true, v, model
	return nil
}
func (f *fakeRatings) RateSession(_ context.Context, _ string, v int, _, model string) error {
	f.rated, f.scoreValue, f.scoreModel = true, v, model
	return nil
}
func (f *fakeRatings) SessionRatings(context.Context, string) ([]ratings.Thumb, int, string, error) {
	return []ratings.Thumb{{Seq: 4, Value: -1}}, 3, "ok", nil
}
func (f *fakeRatings) Stats(context.Context, time.Time) (map[string]*ratings.Agg, error) {
	return f.aggs, nil
}

func TestRateTurnStampsTheModelFromTheResultAndRefusesBadInput(t *testing.T) {
	f := &fakeRatings{resultText: `{"modelUsage":{"claude-haiku-4-5":{"outputTokens":3},"kimi-k2-7-code":{"outputTokens":700}}}`}
	s := &Server{ratings: f}
	call := func(m *agentfleetv1.RateTurnRequest) error {
		_, err := s.RateTurn(context.Background(), connect.NewRequest(m))
		return err
	}
	if err := call(&agentfleetv1.RateTurnRequest{SessionId: sid, Seq: 4, Value: -1}); err != nil {
		t.Fatal(err)
	}
	if f.turnModel != "kimi-k2-7-code" || f.turnValue != -1 {
		t.Fatalf("stamped %q value %d", f.turnModel, f.turnValue)
	}
	for name, m := range map[string]*agentfleetv1.RateTurnRequest{
		"zero value":  {SessionId: sid, Seq: 4, Value: 0},
		"big value":   {SessionId: sid, Seq: 4, Value: 2},
		"bad session": {SessionId: "x", Seq: 4, Value: 1},
		"long note":   {SessionId: sid, Seq: 4, Value: 1, Note: strings.Repeat("a", maxRatingNote+1)},
	} {
		f.rated = false
		if got := connect.CodeOf(call(m)); got != connect.CodeInvalidArgument || f.rated {
			t.Errorf("%s: code %v rated %v", name, got, f.rated)
		}
	}
	f.resultErr = ratings.ErrNoResult
	if got := connect.CodeOf(call(&agentfleetv1.RateTurnRequest{SessionId: sid, Seq: 9, Value: 1})); got != connect.CodeInvalidArgument {
		t.Errorf("non-result seq: code %v", got)
	}
}

func TestRateSessionUsesTheMostUsedModelAndBoundsTheScore(t *testing.T) {
	f := &fakeRatings{results: []ratings.Result{
		{ModelUsage: map[string]ratings.ModelUse{"claude-opus-5": {OutputTokens: 10}}},
		{ModelUsage: map[string]ratings.ModelUse{"zai-org-glm-5": {OutputTokens: 500}}},
	}}
	s := &Server{ratings: f}
	rate := func(score int32) error {
		_, err := s.RateSession(context.Background(), connect.NewRequest(&agentfleetv1.RateSessionRequest{SessionId: sid, Score: score}))
		return err
	}
	if err := rate(5); err != nil || f.scoreModel != "zai-org-glm-5" || f.scoreValue != 5 {
		t.Fatalf("err %v model %q value %d", err, f.scoreModel, f.scoreValue)
	}
	for _, bad := range []int32{0, 6, -1} {
		if connect.CodeOf(rate(bad)) != connect.CodeInvalidArgument {
			t.Errorf("score %d accepted", bad)
		}
	}
}

func TestRatingRPCsAnswerUnavailableWithoutAStore(t *testing.T) {
	s := &Server{}
	_, err := s.ListModelStats(context.Background(), connect.NewRequest(&agentfleetv1.ListModelStatsRequest{}))
	if connect.CodeOf(err) != connect.CodeUnavailable {
		t.Fatalf("err = %v", err)
	}
}

func TestModelStatsRoutePriceAndAverage(t *testing.T) {
	got := modelStats(map[string]*ratings.Agg{
		"claude-opus-5": {Sessions: 2, InputTokens: 10, OutputTokens: 5, SDKCostUSD: 1.5, ThumbsUp: 1},
		"zai-org-glm-5": {Sessions: 1, InputTokens: 2_000_000, OutputTokens: 1_000_000, ScoreSum: 9, Scores: 2, ErrorResults: 1},
	})
	if len(got) != 2 || got[0].Model != "claude-opus-5" {
		t.Fatalf("order/len: %v", got)
	}
	sub, gw := got[0], got[1]
	if sub.Route != "subscription" || sub.BilledUsd != 0 || sub.ListUsd != 1.5 {
		t.Errorf("subscription: %+v", sub)
	}
	// 2M in at 1.00 + 1M out at 3.20
	if gw.Route != "gateway" || gw.BilledUsd != 5.2 || gw.ListUsd != 5.2 || gw.AvgScore != 4.5 || gw.ErrorResults != 1 {
		t.Errorf("gateway: %+v", gw)
	}
}

func TestListSessionRatingsReadsBack(t *testing.T) {
	s := &Server{ratings: &fakeRatings{}}
	r, err := s.ListSessionRatings(context.Background(), connect.NewRequest(&agentfleetv1.ListSessionRatingsRequest{SessionId: sid}))
	if err != nil {
		t.Fatal(err)
	}
	if len(r.Msg.Thumbs) != 1 || r.Msg.Thumbs[0].Seq != 4 || r.Msg.Thumbs[0].Value != -1 || r.Msg.Score != 3 || r.Msg.Note != "ok" {
		t.Fatalf("resp = %+v", r.Msg)
	}
	if _, err := s.ListSessionRatings(context.Background(), connect.NewRequest(&agentfleetv1.ListSessionRatingsRequest{SessionId: "x"})); err == nil {
		t.Fatal("bad session id passed")
	}
}
