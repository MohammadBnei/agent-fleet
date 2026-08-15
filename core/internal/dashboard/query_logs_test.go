package dashboard

import (
	"context"
	"testing"
	"time"

	"connectrpc.com/connect"

	"github.com/MohammadBnei/agent-fleet/core/internal/lokiclient"
	agentfleetv1 "github.com/MohammadBnei/agent-fleet/proto/gen/go/agentfleet/v1"
)

// mockLokiClient is a test double for lokiclient.Client
type mockLokiClient struct {
	queryFunc func(ctx context.Context, req lokiclient.QueryRequest) ([]lokiclient.LogEntry, error)
}

func (m *mockLokiClient) Query(ctx context.Context, req lokiclient.QueryRequest) ([]lokiclient.LogEntry, error) {
	if m.queryFunc != nil {
		return m.queryFunc(ctx, req)
	}
	return nil, nil
}

func TestQueryLogs(t *testing.T) {
	now := time.Now()
	start := now.Add(-1 * time.Hour)

	tests := []struct {
		name      string
		request   *agentfleetv1.QueryLogsRequest
		mockLogs  []lokiclient.LogEntry
		wantCount int32
		wantErr   bool
		errCode   connect.Code
	}{
		{
			name: "successful query",
			request: &agentfleetv1.QueryLogsRequest{
				Component: "worker",
				Namespace: "agent-fleet",
				Level:     "error",
				StartTime: start.Format(time.RFC3339),
				EndTime:   now.Format(time.RFC3339),
				Limit:     100,
			},
			mockLogs: []lokiclient.LogEntry{
				{
					Timestamp:  now.Add(-5 * time.Minute),
					Level:      "error",
					Msg:        "test error",
					Component:  "worker",
					PodName:    "worker-abc-123",
					Namespace:  "agent-fleet",
					FieldsJSON: `{"key":"value"}`,
				},
			},
			wantCount: 1,
			wantErr:   false,
		},
		{
			name: "query with defaults",
			request: &agentfleetv1.QueryLogsRequest{
				Component: "core",
				StartTime: start.Format(time.RFC3339),
				EndTime:   now.Format(time.RFC3339),
			},
			mockLogs:  []lokiclient.LogEntry{},
			wantCount: 0,
			wantErr:   false,
		},
		{
			name: "invalid start time",
			request: &agentfleetv1.QueryLogsRequest{
				Component: "worker",
				StartTime: "not-a-time",
				EndTime:   now.Format(time.RFC3339),
			},
			wantErr: true,
			errCode: connect.CodeInvalidArgument,
		},
		{
			name: "invalid end time",
			request: &agentfleetv1.QueryLogsRequest{
				Component: "worker",
				StartTime: start.Format(time.RFC3339),
				EndTime:   "not-a-time",
			},
			wantErr: true,
			errCode: connect.CodeInvalidArgument,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			mock := &mockLokiClient{
				queryFunc: func(ctx context.Context, req lokiclient.QueryRequest) ([]lokiclient.LogEntry, error) {
					return tt.mockLogs, nil
				},
			}

			s := &Server{loki: mock}
			req := connect.NewRequest(tt.request)

			resp, err := s.QueryLogs(context.Background(), req)

			if tt.wantErr {
				if err == nil {
					t.Fatal("expected error, got nil")
				}
				if tt.errCode != 0 {
					connectErr, ok := err.(*connect.Error)
					if !ok {
						t.Fatalf("expected connect.Error, got %T", err)
					}
					if connectErr.Code() != tt.errCode {
						t.Errorf("wrong error code: got %v, want %v", connectErr.Code(), tt.errCode)
					}
				}
				return
			}

			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}

			if resp.Msg.TotalCount != tt.wantCount {
				t.Errorf("wrong count: got %d, want %d", resp.Msg.TotalCount, tt.wantCount)
			}

			if int32(len(resp.Msg.Entries)) != tt.wantCount {
				t.Errorf("wrong entries length: got %d, want %d", len(resp.Msg.Entries), tt.wantCount)
			}

			// Verify first entry if we have logs
			if tt.wantCount > 0 && len(tt.mockLogs) > 0 {
				got := resp.Msg.Entries[0]
				want := tt.mockLogs[0]

				if got.Level != want.Level {
					t.Errorf("wrong level: got %s, want %s", got.Level, want.Level)
				}
				if got.Msg != want.Msg {
					t.Errorf("wrong msg: got %s, want %s", got.Msg, want.Msg)
				}
				if got.Component != want.Component {
					t.Errorf("wrong component: got %s, want %s", got.Component, want.Component)
				}
			}
		})
	}
}

// The dashboard's LogDrawer sends only session_id and limit — no time range,
// because it asks for "the newest 200 lines" and has no range picker. Parsing
// the empty strings unconditionally made every open fail with
// invalid_argument, so the drawer had never returned a line.
func TestQueryLogsDefaultsTheTimeRange(t *testing.T) {
	var got lokiclient.QueryRequest
	mock := &mockLokiClient{
		queryFunc: func(_ context.Context, req lokiclient.QueryRequest) ([]lokiclient.LogEntry, error) {
			got = req
			return nil, nil
		},
	}
	s := &Server{loki: mock}

	before := time.Now()
	_, err := s.QueryLogs(context.Background(), connect.NewRequest(&agentfleetv1.QueryLogsRequest{
		SessionId: "abc123",
		Limit:     200,
	}))
	if err != nil {
		t.Fatalf("query with no time range must succeed, got %v", err)
	}
	if !got.End.After(got.Start) {
		t.Fatalf("defaulted window is not a window: start %v, end %v", got.Start, got.End)
	}
	if got.Start.After(before.Add(-23 * time.Hour)) {
		t.Errorf("defaulted start should look back ~24h, got %v", got.Start)
	}
}
