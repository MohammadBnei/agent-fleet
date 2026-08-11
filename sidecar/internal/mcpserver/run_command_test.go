package mcpserver

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"

	"github.com/mark3labs/mcp-go/mcp"

	agentfleetv1 "github.com/MohammadBnei/agent-fleet/proto/gen/go/agentfleet/v1"
)

// okResult is what execmcp actually returns on the wire: a CallToolResult
// whose single text block is the {stdout,stderr,exitCode} JSON.
func okResult(stdout string, exitCode int) string {
	body, _ := json.Marshal(map[string]any{"stdout": stdout, "stderr": "", "exitCode": exitCode})
	out, _ := json.Marshal(map[string]any{
		"content": []map[string]any{{"type": "text", "text": string(body)}},
	})
	return string(out)
}

type mockE2eRunner struct {
	callResults []string // consumed in order, one per CallE2eTool
	callErrs    []error
	callIsError bool
	calls       int

	provisioned  int
	provisionErr error
	env          *agentfleetv1.RequestE2EEnvResponse
}

func (m *mockE2eRunner) CallE2eTool(_ context.Context, _, _ string) (string, bool, error) {
	i := m.calls
	m.calls++
	if i < len(m.callErrs) && m.callErrs[i] != nil {
		return "", false, m.callErrs[i]
	}
	if i < len(m.callResults) {
		return m.callResults[i], m.callIsError, nil
	}
	return okResult("", 0), m.callIsError, nil
}

func (m *mockE2eRunner) RequestE2eEnv(_ context.Context, _ string) (*agentfleetv1.RequestE2EEnvResponse, error) {
	m.provisioned++
	if m.provisionErr != nil {
		return nil, m.provisionErr
	}
	if m.env != nil {
		return m.env, nil
	}
	return &agentfleetv1.RequestE2EEnvResponse{}, nil
}

func TestRunCommandHandler(t *testing.T) {
	transport := errors.New("connection refused")

	tests := []struct {
		name           string
		params         map[string]any
		mock           *mockE2eRunner
		wantCalls      int
		wantProvisions int
		check          func(t *testing.T, result *mcp.CallToolResult)
	}{
		{
			// The common case: a sandbox is already live, so nothing is
			// provisioned and exactly one round trip happens.
			name:           "live sandbox runs the command directly",
			params:         map[string]any{"command": "go test ./..."},
			mock:           &mockE2eRunner{callResults: []string{okResult("ok\n", 0)}},
			wantCalls:      1,
			wantProvisions: 0,
			check: func(t *testing.T, result *mcp.CallToolResult) {
				if result.IsError {
					t.Fatal("expected a successful result")
				}
				if got := textOf(t, result, 0); !strings.Contains(got, "ok") {
					t.Errorf("expected stdout passed through, got %q", got)
				}
			},
		},
		{
			// Guard clause fires before core is ever touched — same
			// convention as files_test.go.
			name:           "empty command is rejected without calling core",
			params:         map[string]any{},
			mock:           &mockE2eRunner{},
			wantCalls:      0,
			wantProvisions: 0,
			check: func(t *testing.T, result *mcp.CallToolResult) {
				if !result.IsError {
					t.Error("expected an error result for a missing command")
				}
			},
		},
		{
			// The headline behavior: no pod yet, so one is started on
			// demand and the command is retried, with the recipe prepended
			// so the agent knows what it landed in.
			name:   "no sandbox provisions one and retries",
			params: map[string]any{"command": "bun install"},
			mock: &mockE2eRunner{
				callErrs:    []error{transport},
				callResults: []string{"", okResult("installed\n", 0)},
				env: &agentfleetv1.RequestE2EEnvResponse{
					ProfileName:      "e2e",
					ResolvedStartCmd: "bunx vite dev --host 0.0.0.0 --port $PORT",
					Tools:            []string{"bun-toolchain"},
				},
			},
			wantCalls:      2,
			wantProvisions: 1,
			check: func(t *testing.T, result *mcp.CallToolResult) {
				if result.IsError {
					t.Fatal("expected the retry to succeed")
				}
				note := textOf(t, result, 0)
				for _, want := range []string{"startedSandbox", "bun-toolchain", "vite dev"} {
					if !strings.Contains(note, want) {
						t.Errorf("recipe note missing %q, got %q", want, note)
					}
				}
				if got := textOf(t, result, 1); !strings.Contains(got, "installed") {
					t.Errorf("command output should follow the note, got %q", got)
				}
			},
		},
		{
			name:   "provisioning failure is reported, not retried",
			params: map[string]any{"command": "make build"},
			mock: &mockE2eRunner{
				callErrs:     []error{transport},
				provisionErr: errors.New("no e2e profile for repo"),
			},
			wantCalls:      1,
			wantProvisions: 1,
			check: func(t *testing.T, result *mcp.CallToolResult) {
				if !result.IsError {
					t.Fatal("expected an error result")
				}
				if got := textOf(t, result, 0); !strings.Contains(got, "no e2e profile") {
					t.Errorf("expected the underlying cause surfaced, got %q", got)
				}
			},
		},
		{
			name:   "still unreachable after provisioning errors out",
			params: map[string]any{"command": "make build"},
			mock: &mockE2eRunner{
				callErrs: []error{transport, transport},
			},
			wantCalls:      2,
			wantProvisions: 1,
			check: func(t *testing.T, result *mcp.CallToolResult) {
				if !result.IsError {
					t.Fatal("expected an error result")
				}
			},
		},
		{
			// A failing build must not look like a missing sandbox —
			// execmcp reports a nonzero exit as an ordinary result, so
			// nothing should be provisioned and nothing retried.
			name:           "nonzero exit does not provision or retry",
			params:         map[string]any{"command": "false"},
			mock:           &mockE2eRunner{callResults: []string{okResult("", 1)}},
			wantCalls:      1,
			wantProvisions: 0,
			check: func(t *testing.T, result *mcp.CallToolResult) {
				if got := textOf(t, result, 0); !strings.Contains(got, `"exitCode":1`) {
					t.Errorf("expected the exit code passed through, got %q", got)
				}
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			req := mcp.CallToolRequest{}
			req.Params.Name = "run_command"
			req.Params.Arguments = tt.params

			result, err := runCommandHandler(tt.mock)(context.Background(), req)
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if tt.mock.calls != tt.wantCalls {
				t.Errorf("CallE2eTool calls = %d, want %d", tt.mock.calls, tt.wantCalls)
			}
			if tt.mock.provisioned != tt.wantProvisions {
				t.Errorf("RequestE2eEnv calls = %d, want %d", tt.mock.provisioned, tt.wantProvisions)
			}
			tt.check(t, result)
		})
	}
}

func textOf(t *testing.T, result *mcp.CallToolResult, i int) string {
	t.Helper()
	if len(result.Content) <= i {
		t.Fatalf("expected at least %d content blocks, got %d", i+1, len(result.Content))
	}
	text, ok := result.Content[i].(mcp.TextContent)
	if !ok {
		t.Fatalf("expected TextContent at %d, got %T", i, result.Content[i])
	}
	return text.Text
}
