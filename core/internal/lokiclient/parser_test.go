package lokiclient

import (
	"encoding/json"
	"testing"
)

func TestParseLogLine(t *testing.T) {
	tests := []struct {
		name           string
		line           string
		wantLevel      string
		wantMsg        string
		wantFieldsJSON string
	}{
		{
			name:           "slog JSON format",
			line:           `{"time":"2024-01-15T10:30:00Z","level":"info","msg":"task claimed","task_id":"abc-123"}`,
			wantLevel:      "info",
			wantMsg:        "task claimed",
			wantFieldsJSON: `{"task_id":"abc-123"}`,
		},
		{
			name:           "error level",
			line:           `{"level":"error","msg":"connection failed","error":"timeout"}`,
			wantLevel:      "error",
			wantMsg:        "connection failed",
			wantFieldsJSON: `{"error":"timeout"}`,
		},
		{
			name:           "minimal JSON",
			line:           `{"level":"debug","msg":"started"}`,
			wantLevel:      "debug",
			wantMsg:        "started",
			wantFieldsJSON: "",
		},
		{
			name:           "not JSON - plain text",
			line:           "plain text log line",
			wantLevel:      "",
			wantMsg:        "plain text log line",
			wantFieldsJSON: "",
		},
		{
			name:           "JSON without level/msg",
			line:           `{"foo":"bar","baz":123}`,
			wantLevel:      "",
			wantMsg:        "",
			wantFieldsJSON: `{"baz":123,"foo":"bar"}`,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			gotLevel, gotMsg, gotFieldsJSON := parseLogLine(tt.line)

			if gotLevel != tt.wantLevel {
				t.Errorf("parseLogLine() level = %v, want %v", gotLevel, tt.wantLevel)
			}
			if gotMsg != tt.wantMsg {
				t.Errorf("parseLogLine() msg = %v, want %v", gotMsg, tt.wantMsg)
			}

			// Compare JSON fields (order may vary)
			if tt.wantFieldsJSON != "" {
				var want, got map[string]interface{}
				if err := json.Unmarshal([]byte(tt.wantFieldsJSON), &want); err != nil {
					t.Fatalf("invalid test want JSON: %v", err)
				}
				if err := json.Unmarshal([]byte(gotFieldsJSON), &got); err != nil {
					t.Fatalf("invalid got JSON: %v", err)
				}

				wantJSON, _ := json.Marshal(want)
				gotJSON, _ := json.Marshal(got)
				if string(wantJSON) != string(gotJSON) {
					t.Errorf("parseLogLine() fieldsJSON = %v, want %v", string(gotJSON), string(wantJSON))
				}
			} else if gotFieldsJSON != "" {
				t.Errorf("parseLogLine() fieldsJSON = %v, want empty", gotFieldsJSON)
			}
		})
	}
}
