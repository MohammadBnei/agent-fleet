package dashboard

import (
	"context"
	"encoding/json"
	"errors"
	"testing"
	"time"

	"connectrpc.com/connect"

	agentfleetv1 "github.com/MohammadBnei/agent-fleet/proto/gen/go/agentfleet/v1"

	"github.com/MohammadBnei/agent-fleet/core/internal/sessions"
)

type mockProm struct {
	query      func(ctx context.Context, q string) (json.RawMessage, error)
	queryRange func(ctx context.Context, q string, start, end time.Time, step time.Duration) (json.RawMessage, error)
}

func (m *mockProm) Query(ctx context.Context, q string) (json.RawMessage, error) {
	if m.query != nil {
		return m.query(ctx, q)
	}
	return json.RawMessage(`{"resultType":"vector","result":[]}`), nil
}

func (m *mockProm) QueryRange(ctx context.Context, q string, start, end time.Time, step time.Duration) (json.RawMessage, error) {
	if m.queryRange != nil {
		return m.queryRange(ctx, q, start, end, step)
	}
	return json.RawMessage(`{"resultType":"matrix","result":[]}`), nil
}

func TestQueryMetricsValidation(t *testing.T) {
	now := time.Now().UTC()
	tests := []struct {
		name string
		req  *agentfleetv1.QueryMetricsRequest
		code connect.Code
	}{
		{"empty query", &agentfleetv1.QueryMetricsRequest{}, connect.CodeInvalidArgument},
		{"whitespace query", &agentfleetv1.QueryMetricsRequest{Query: "   "}, connect.CodeInvalidArgument},
		{
			"unparseable start",
			&agentfleetv1.QueryMetricsRequest{Query: "up", StartTime: "yesterday", EndTime: now.Format(time.RFC3339)},
			connect.CodeInvalidArgument,
		},
		{
			"end before start",
			&agentfleetv1.QueryMetricsRequest{
				Query:     "up",
				StartTime: now.Format(time.RFC3339),
				EndTime:   now.Add(-time.Hour).Format(time.RFC3339),
			},
			connect.CodeInvalidArgument,
		},
		{
			// Prometheus retention is 7d; a wider window costs a full scan
			// and returns nothing extra.
			"range beyond retention",
			&agentfleetv1.QueryMetricsRequest{
				Query:     "up",
				StartTime: now.Add(-30 * 24 * time.Hour).Format(time.RFC3339),
				EndTime:   now.Format(time.RFC3339),
			},
			connect.CodeInvalidArgument,
		},
		{
			"bad step",
			&agentfleetv1.QueryMetricsRequest{
				Query:     "up",
				StartTime: now.Add(-time.Hour).Format(time.RFC3339),
				EndTime:   now.Format(time.RFC3339),
				Step:      "every-so-often",
			},
			connect.CodeInvalidArgument,
		},
	}

	s := &Server{prom: &mockProm{}}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, err := s.QueryMetrics(context.Background(), connect.NewRequest(tt.req))
			if connect.CodeOf(err) != tt.code {
				t.Fatalf("code = %v (err %v), want %v", connect.CodeOf(err), err, tt.code)
			}
		})
	}
}

// Both times empty means an instant query — the shape a stat tile needs.
// Sending it down the range path would make Prometheus reject it.
func TestQueryMetricsInstantVsRange(t *testing.T) {
	var instant, ranged bool
	s := &Server{prom: &mockProm{
		query: func(context.Context, string) (json.RawMessage, error) {
			instant = true
			return json.RawMessage(`{}`), nil
		},
		queryRange: func(context.Context, string, time.Time, time.Time, time.Duration) (json.RawMessage, error) {
			ranged = true
			return json.RawMessage(`{}`), nil
		},
	}}

	if _, err := s.QueryMetrics(context.Background(), connect.NewRequest(&agentfleetv1.QueryMetricsRequest{Query: "up"})); err != nil {
		t.Fatalf("instant query: %v", err)
	}
	if !instant || ranged {
		t.Fatalf("no times set: instant=%v ranged=%v, want instant only", instant, ranged)
	}

	instant, ranged = false, false
	now := time.Now().UTC()
	_, err := s.QueryMetrics(context.Background(), connect.NewRequest(&agentfleetv1.QueryMetricsRequest{
		Query:     "up",
		StartTime: now.Add(-time.Hour).Format(time.RFC3339),
		EndTime:   now.Format(time.RFC3339),
	}))
	if err != nil {
		t.Fatalf("range query: %v", err)
	}
	if instant || !ranged {
		t.Fatalf("both times set: instant=%v ranged=%v, want range only", instant, ranged)
	}
}

func strptr(s string) *string { return &s }

func TestCellStatus(t *testing.T) {
	running := strptr("POD_PHASE_RUNNING")
	tests := []struct {
		name string
		task sessions.Session
		want string
	}{
		{"running and clean", sessions.Session{PodPhase: running}, "healthy"},
		// Was keyed on status 'failed'/'failed_permanently'. Both are gone
		// (docs/adr/0048), and pod_phase carries the fact more directly: a
		// crashed pod is CRASHED, which is the state a human acts on.
		{"crashed pod", sessions.Session{PodPhase: strptr("POD_PHASE_CRASHED")}, "failing"},
		// A pod that is up with a recorded error may still recover — that's
		// a different colour from one that has actually died.
		{"live but errored", sessions.Session{PodPhase: running, LastError: strptr("boom")}, "degraded"},
		{"still provisioning", sessions.Session{PodPhase: strptr("POD_PHASE_PROVISIONING")}, "idle"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := cellStatus(tt.task); got != tt.want {
				t.Errorf("cellStatus = %q, want %q", got, tt.want)
			}
		})
	}
}

// scalarQuery pulls the number out of Prometheus' instant-query shape,
// where the value is a *string* so NaN/Inf survive JSON. An empty result is
// 0, not an error — that's the normal state of a freshly restarted core,
// and erroring would blank the whole topology.
func TestScalarQuery(t *testing.T) {
	tests := []struct {
		name string
		body string
		want float64
	}{
		{"vector with a sample", `{"resultType":"vector","result":[{"metric":{},"value":[1786700000,"4.25"]}]}`, 4.25},
		{"empty vector", `{"resultType":"vector","result":[]}`, 0},
		{"non-numeric value", `{"resultType":"vector","result":[{"metric":{},"value":[1786700000,"NaN"]}]}`, 0},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			s := &Server{prom: &mockProm{query: func(context.Context, string) (json.RawMessage, error) {
				return json.RawMessage(tt.body), nil
			}}}
			got, err := s.scalarQuery(context.Background(), "whatever")
			if err != nil {
				t.Fatalf("scalarQuery: %v", err)
			}
			if got != tt.want {
				t.Errorf("scalarQuery = %v, want %v", got, tt.want)
			}
		})
	}
}

// Prometheus being unreachable must degrade the topology, not empty it:
// the cells come from Postgres and are still true. Reporting zero rates
// with no explanation would render a busy fleet as an idle one.
func TestAnnotateRatesFailureIsReported(t *testing.T) {
	s := &Server{prom: &mockProm{query: func(context.Context, string) (json.RawMessage, error) {
		return nil, errors.New("connection refused")
	}}}
	nodes := []*agentfleetv1.CellNode{{Id: "core", Metrics: map[string]float64{}}}
	if err := s.annotateRates(context.Background(), nodes, nil); err == nil {
		t.Fatal("annotateRates returned nil error when Prometheus was down")
	}
}
