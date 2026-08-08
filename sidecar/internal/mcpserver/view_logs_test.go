package mcpserver

import (
	"context"
	"strings"
	"testing"

	"github.com/mark3labs/mcp-go/mcp"
)

// mockCoreClient is a test double for coreclient.Client
type mockCoreClient struct {
	viewLogsFunc func(ctx context.Context, component, appName, namespace, level, duration string, limit int32, startTime, endTime string) (string, error)
}

func (m *mockCoreClient) ViewLogs(ctx context.Context, component, appName, namespace, level, duration string, limit int32, startTime, endTime string) (string, error) {
	if m.viewLogsFunc != nil {
		return m.viewLogsFunc(ctx, component, appName, namespace, level, duration, limit, startTime, endTime)
	}
	return "", nil
}

func TestViewLogsHandler(t *testing.T) {
	tests := []struct {
		name        string
		params      map[string]any
		mockResult  string
		wantErr     bool
		checkResult func(t *testing.T, result *mcp.CallToolResult)
	}{
		{
			name: "successful worker logs",
			params: map[string]any{
				"component": "worker",
				"level":     "error",
				"duration":  "1h",
				"limit":     float64(50),
			},
			mockResult: "Timestamp | Level | Message\n---\n2024-01-01 10:00:00 | error | test error\n",
			wantErr:    false,
			checkResult: func(t *testing.T, result *mcp.CallToolResult) {
				if len(result.Content) == 0 {
					t.Fatal("expected content, got empty")
				}
				text, ok := result.Content[0].(mcp.TextContent)
				if !ok {
					t.Fatalf("expected TextContent, got %T", result.Content[0])
				}
				if !strings.Contains(text.Text, "test error") {
					t.Error("output should contain 'test error'")
				}
			},
		},
		{
			name: "deployed app logs",
			params: map[string]any{
				"component": "app",
				"app_name":  "dream-analyst",
				"namespace": "default",
			},
			mockResult: "Timestamp | Level | Message\n---\n2024-01-01 10:00:00 | info | app started\n",
			wantErr:    false,
			checkResult: func(t *testing.T, result *mcp.CallToolResult) {
				if len(result.Content) == 0 {
					t.Fatal("expected content, got empty")
				}
				text, ok := result.Content[0].(mcp.TextContent)
				if !ok {
					t.Fatalf("expected TextContent, got %T", result.Content[0])
				}
				if !strings.Contains(text.Text, "app started") {
					t.Error("output should contain 'app started'")
				}
			},
		},
		{
			name:    "missing component",
			params:  map[string]any{},
			wantErr: false, // Returns error result, not error
			checkResult: func(t *testing.T, result *mcp.CallToolResult) {
				if !result.IsError {
					t.Error("expected error result")
				}
			},
		},
		{
			name: "defaults applied",
			params: map[string]any{
				"component": "sidecar",
			},
			mockResult: "No logs found\n",
			wantErr:    false,
			checkResult: func(t *testing.T, result *mcp.CallToolResult) {
				if len(result.Content) == 0 {
					t.Fatal("expected content, got empty")
				}
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			mock := &mockCoreClient{
				viewLogsFunc: func(ctx context.Context, component, appName, namespace, level, duration string, limit int32, startTime, endTime string) (string, error) {
					// Verify defaults are applied correctly
					if component == "sidecar" {
						if namespace != "agent-fleet" {
							t.Errorf("expected default namespace 'agent-fleet', got %s", namespace)
						}
						if duration != "1h" {
							t.Errorf("expected default duration '1h', got %s", duration)
						}
						if limit != 50 {
							t.Errorf("expected default limit 50, got %d", limit)
						}
					}
					return tt.mockResult, nil
				},
			}

			handler := viewLogsHandler(mock)

			// Create CallToolRequest with test parameters
			req := mcp.CallToolRequest{}
			req.Params.Name = "view_logs"
			req.Params.Arguments = tt.params

			result, err := handler(context.Background(), req)

			if tt.wantErr {
				if err == nil {
					t.Fatal("expected error, got nil")
				}
				return
			}

			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}

			if tt.checkResult != nil {
				tt.checkResult(t, result)
			}
		})
	}
}
