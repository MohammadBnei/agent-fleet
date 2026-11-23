package dashboard

import (
	"context"
	"errors"
	"github.com/MohammadBnei/agent-fleet/core/internal/models"
	"log/slog"
	"slices"
	"strings"
	"time"

	"connectrpc.com/connect"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgconn"

	"github.com/MohammadBnei/agent-fleet/core/internal/errkeys"
	"github.com/MohammadBnei/agent-fleet/core/internal/ratings"
	agentfleetv1 "github.com/MohammadBnei/agent-fleet/proto/gen/go/agentfleet/v1"
)

// ratingStore is *ratings.Store, as an interface so handlers test with a fake.
type ratingStore interface {
	ResultText(ctx context.Context, sessionID string, seq int64) (string, error)
	Results(ctx context.Context, sessionID string) ([]ratings.Result, error)
	LastResult(ctx context.Context, sessionID string) (ratings.Result, bool, error)
	RateTurn(ctx context.Context, sessionID string, seq int64, value int, note, model string) error
	RateSession(ctx context.Context, sessionID string, score int, note, model string) error
	Stats(ctx context.Context, since time.Time) (map[string]*ratings.Agg, error)
	SessionRatings(ctx context.Context, sessionID string) ([]ratings.Thumb, int, string, error)
}

// SetRatings installs the ratings store. Without it the rating RPCs answer Unavailable.
func (s *Server) SetRatings(r ratingStore) { s.ratings = r }

const maxRatingNote = 2000

func (s *Server) ratingsOr() (ratingStore, error) {
	if s.ratings == nil {
		return nil, errkeys.Connect(connect.CodeUnavailable, errkeys.KeyInternal, errors.New("ratings are not configured"))
	}
	return s.ratings, nil
}

func ratingErr(err error) error {
	var pg *pgconn.PgError
	switch {
	case errors.Is(err, ratings.ErrNoResult):
		return refuse(connect.CodeInvalidArgument, "seq is not a result entry of this session")
	case errors.As(err, &pg) && pg.Code == "23503":
		return errkeys.Connect(connect.CodeNotFound, errkeys.KeyInternal, errors.New("session not found"))
	}
	return errkeys.Connect(connect.CodeInternal, errkeys.KeyInternal, err)
}

// RateTurn records a thumb on one result entry, stamped with the model that
// produced it: the modelUsage key with the most output tokens, never sessions.model.
func (s *Server) RateTurn(ctx context.Context, req *connect.Request[agentfleetv1.RateTurnRequest]) (*connect.Response[agentfleetv1.RateTurnResponse], error) {
	rs, err := s.ratingsOr()
	if err != nil {
		return nil, err
	}
	m := req.Msg
	switch {
	case uuid.Validate(m.GetSessionId()) != nil:
		return nil, refuse(connect.CodeInvalidArgument, "invalid session id")
	case m.GetValue() != 1 && m.GetValue() != -1:
		return nil, refuse(connect.CodeInvalidArgument, "value must be +1 or -1")
	case len(m.GetNote()) > maxRatingNote:
		return nil, refuse(connect.CodeInvalidArgument, "note is too long")
	}
	text, err := rs.ResultText(ctx, m.GetSessionId(), m.GetSeq())
	if err != nil {
		return nil, ratingErr(err)
	}
	r, _ := ratings.ParseResult(text)
	model := r.Turn()
	if err := rs.RateTurn(ctx, m.GetSessionId(), m.GetSeq(), int(m.GetValue()), m.GetNote(), model); err != nil {
		return nil, ratingErr(err)
	}
	slog.InfoContext(ctx, "rating", "sessionId", m.GetSessionId(), "model", model, "kind", "thumb", "value", m.GetValue())
	return connect.NewResponse(&agentfleetv1.RateTurnResponse{}), nil
}

// RateSession records the session's 1..5 score, stamped with the model most
// used across its results.
func (s *Server) RateSession(ctx context.Context, req *connect.Request[agentfleetv1.RateSessionRequest]) (*connect.Response[agentfleetv1.RateSessionResponse], error) {
	rs, err := s.ratingsOr()
	if err != nil {
		return nil, err
	}
	m := req.Msg
	switch {
	case uuid.Validate(m.GetSessionId()) != nil:
		return nil, refuse(connect.CodeInvalidArgument, "invalid session id")
	case m.GetScore() < 1 || m.GetScore() > 5:
		return nil, refuse(connect.CodeInvalidArgument, "score must be 1 to 5")
	case len(m.GetNote()) > maxRatingNote:
		return nil, refuse(connect.CodeInvalidArgument, "note is too long")
	}
	results, err := rs.Results(ctx, m.GetSessionId())
	if err != nil {
		return nil, ratingErr(err)
	}
	model := ratings.MostUsed(results)
	if err := rs.RateSession(ctx, m.GetSessionId(), int(m.GetScore()), m.GetNote(), model); err != nil {
		return nil, ratingErr(err)
	}
	slog.InfoContext(ctx, "rating", "sessionId", m.GetSessionId(), "model", model, "kind", "score", "value", m.GetScore())
	return connect.NewResponse(&agentfleetv1.RateSessionResponse{}), nil
}

// ListSessionRatings reads back the session's thumbs and score, so a reload still shows them.
func (s *Server) ListSessionRatings(ctx context.Context, req *connect.Request[agentfleetv1.ListSessionRatingsRequest]) (*connect.Response[agentfleetv1.ListSessionRatingsResponse], error) {
	rs, err := s.ratingsOr()
	if err != nil {
		return nil, err
	}
	if uuid.Validate(req.Msg.GetSessionId()) != nil {
		return nil, refuse(connect.CodeInvalidArgument, "invalid session id")
	}
	thumbs, score, note, err := rs.SessionRatings(ctx, req.Msg.GetSessionId())
	if err != nil {
		return nil, errkeys.Connect(connect.CodeInternal, errkeys.KeyInternal, err)
	}
	out := &agentfleetv1.ListSessionRatingsResponse{Score: int32(score), Note: note}
	for _, t := range thumbs {
		out.Thumbs = append(out.Thumbs, &agentfleetv1.TurnThumb{Seq: t.Seq, Value: int32(t.Value)})
	}
	return connect.NewResponse(out), nil
}

// ListModelStats aggregates the last `days` days per model. Route follows the
// id prefix. A gateway model's USD comes from the price map (the SDK's own
// cost is wrong for ids it does not know); a subscription model bills nothing
// and lists at the SDK's cost.
func (s *Server) ListModelStats(ctx context.Context, req *connect.Request[agentfleetv1.ListModelStatsRequest]) (*connect.Response[agentfleetv1.ListModelStatsResponse], error) {
	rs, err := s.ratingsOr()
	if err != nil {
		return nil, err
	}
	days := int(req.Msg.GetDays())
	if days <= 0 {
		days = 30
	}
	days = min(days, 365)
	aggs, err := rs.Stats(ctx, time.Now().Add(-time.Duration(days)*24*time.Hour))
	if err != nil {
		return nil, errkeys.Connect(connect.CodeInternal, errkeys.KeyInternal, err)
	}
	return connect.NewResponse(&agentfleetv1.ListModelStatsResponse{Stats: modelStats(aggs)}), nil
}

func modelStats(aggs map[string]*ratings.Agg) []*agentfleetv1.ModelStat {
	out := make([]*agentfleetv1.ModelStat, 0, len(aggs))
	for model, a := range aggs {
		st := &agentfleetv1.ModelStat{
			Model: model, Route: "subscription", Sessions: a.Sessions,
			InputTokens: a.InputTokens, OutputTokens: a.OutputTokens,
			ThumbsUp: a.ThumbsUp, ThumbsDown: a.ThumbsDown, Scores: a.Scores, ErrorResults: a.ErrorResults,
			ListUsd: a.SDKCostUSD,
		}
		if a.Scores > 0 {
			st.AvgScore = float64(a.ScoreSum) / float64(a.Scores)
		}
		if p, ok := models.Gateway(model); ok {
			st.Route = "gateway"
			st.ListUsd = float64(a.InputTokens)*p.InUSD/1e6 + float64(a.OutputTokens)*p.OutUSD/1e6
			st.BilledUsd = st.ListUsd
		}
		out = append(out, st)
	}
	slices.SortFunc(out, func(a, b *agentfleetv1.ModelStat) int { return strings.Compare(a.Model, b.Model) })
	return out
}
