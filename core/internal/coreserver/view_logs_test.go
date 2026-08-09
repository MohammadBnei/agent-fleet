package coreserver

import (
	"context"
	"strings"
	"testing"
	"time"

	agentfleetv1 "github.com/MohammadBnei/agent-fleet/proto/gen/go/agentfleet/v1"
	"github.com/MohammadBnei/agent-fleet/core/internal/lokiclient"
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

func TestViewLogs(t *testing.T) {
	now := time.Now()
	start := now.Add(-1 * time.Hour)

	tests := []struct {
		name        string
		request     *agentfleetv1.ViewLogsRequest
		mockLogs    []lokiclient.LogEntry
		wantErr     bool
		checkOutput func(t *testing.T, output string)
	}{
		{
			name: "successful query with duration",
			request: &agentfleetv1.ViewLogsRequest{
				Component: "worker",
				Namespace: "agent-fleet",
				Level:     "error",
				Duration:  "1h",
				Limit:     50,
			},
			mockLogs: []lokiclient.LogEntry{
				{
					Timestamp: now.Add(-5 * time.Minute),
					Level:     "error",
					Msg:       "test error message",
					Component: "worker",
					PodName:   "worker-abc-123",
					Namespace: "agent-fleet",
				},
				{
					Timestamp: now.Add(-10 * time.Minute),
					Level:     "error",
					Msg:       "another error",
					Component: "worker",
					PodName:   "worker-abc-123",
					Namespace: "agent-fleet",
				},
			},
			wantErr: false,
			checkOutput: func(t *testing.T, output string) {
				if !strings.Contains(output, "test error message") {
					t.Error("output should contain 'test error message'")
				}
				if !strings.Contains(output, "another error") {
					t.Error("output should contain 'another error'")
				}
				if !strings.Contains(output, "worker-abc-123") {
					t.Error("output should contain pod name")
				}
			},
		},
		{
			name: "explicit start and end timestamps",
			request: &agentfleetv1.ViewLogsRequest{
				Component: "sidecar",
				StartTime: start.Format(time.RFC3339),
				EndTime:   now.Format(time.RFC3339),
				Limit:     100,
			},
			mockLogs: []lokiclient.LogEntry{
				{
					Timestamp: start.Add(30 * time.Minute),
					Level:     "info",
					Msg:       "specific timerange log",
					Component: "sidecar",
					PodName:   "worker-xyz",
					Namespace: "agent-fleet",
				},
			},
			wantErr: false,
			checkOutput: func(t *testing.T, output string) {
				if !strings.Contains(output, "specific timerange log") {
					t.Error("output should contain log from specific timerange")
				}
			},
		},
		{
			name: "invalid start timestamp",
			request: &agentfleetv1.ViewLogsRequest{
				Component: "worker",
				StartTime: "not-a-timestamp",
			},
			wantErr: true,
		},
		{
			name: "invalid end timestamp",
			request: &agentfleetv1.ViewLogsRequest{
				Component: "worker",
				EndTime:   "invalid",
			},
			wantErr: true,
		},
		{
			name: "empty result",
			request: &agentfleetv1.ViewLogsRequest{
				Component: "core",
				Duration:  "30m",
			},
			mockLogs: []lokiclient.LogEntry{},
			wantErr:  false,
			checkOutput: func(t *testing.T, output string) {
				if !strings.Contains(output, "No logs found") {
					t.Error("output should indicate no logs found")
				}
			},
		},
		{
			name: "missing component",
			request: &agentfleetv1.ViewLogsRequest{
				Duration: "1h",
			},
			wantErr: true,
		},
		{
			name: "deployed app logs",
			request: &agentfleetv1.ViewLogsRequest{
				Component: "app",
				AppName:   "dream-analyst",
				Namespace: "default",
				Duration:  "1h",
			},
			mockLogs: []lokiclient.LogEntry{
				{
					Timestamp: now,
					Level:     "info",
					Msg:       "app started",
					Component: "app",
					PodName:   "dream-analyst-xyz",
					Namespace: "default",
				},
			},
			wantErr: false,
			checkOutput: func(t *testing.T, output string) {
				if !strings.Contains(output, "app started") {
					t.Error("output should contain app log message")
				}
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			mock := &mockLokiClient{
				queryFunc: func(ctx context.Context, req lokiclient.QueryRequest) ([]lokiclient.LogEntry, error) {
					// Verify query request parameters
					if tt.request.GetComponent() != "" && req.Component != tt.request.GetComponent() {
						t.Errorf("wrong component: got %s, want %s", req.Component, tt.request.GetComponent())
					}
					if tt.request.GetNamespace() != "" && req.Namespace != tt.request.GetNamespace() {
						t.Errorf("wrong namespace: got %s, want %s", req.Namespace, tt.request.GetNamespace())
					}
					return tt.mockLogs, nil
				},
			}

			s := &Server{loki: mock}

			resp, err := s.ViewLogs(context.Background(), tt.request)

			if tt.wantErr {
				if err == nil {
					t.Fatal("expected error, got nil")
				}
				return
			}

			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}

			if tt.checkOutput != nil {
				tt.checkOutput(t, resp.LogsText)
			}
		})
	}
}

func TestParseDuration(t *testing.T) {
	tests := []struct {
		input    string
		fallback time.Duration
		want     time.Duration
	}{
		{"1h", time.Hour, time.Hour},
		{"30m", time.Hour, 30 * time.Minute},
		{"6h", time.Hour, 6 * time.Hour},
		{"24h", time.Hour, 24 * time.Hour},
		{"invalid", time.Hour, time.Hour},
		{"", 2 * time.Hour, 2 * time.Hour},
	}

	for _, tt := range tests {
		t.Run(tt.input, func(t *testing.T) {
			got := parseDuration(tt.input, tt.fallback)
			if got != tt.want {
				t.Errorf("parseDuration(%q, %v) = %v, want %v", tt.input, tt.fallback, got, tt.want)
			}
		})
	}
}
