package coreserver

import (
	"testing"

	agentfleetv1 "github.com/MohammadBnei/agent-fleet/proto/gen/go/agentfleet/v1"
)

// TestStringToProtoType_RoundTrip covers this package's own copy of the
// write-path mapping — the sidecar's SendMessage RPC call carries the type
// as this enum, and coreserver.SendMessage's protoTypeToString(req.GetType())
// is what actually lands in the `type` column the Discord relay allowlist
// (core/internal/transcript/relay.go) later reads. Before the four SDK
// message types below had enum values, they silently coerced to
// UNSPECIFIED -> "" here, which the relay allowlist explicitly treats as
// Discord-safe -- a real secret-leak path (raw Bash stdout/stderr, tool
// input) reopened through enum coercion rather than a missing filter rule.
func TestStringToProtoType_RoundTrip(t *testing.T) {
	cases := []struct {
		str  string
		enum agentfleetv1.TranscriptEntryType
	}{
		{"discussion", agentfleetv1.TranscriptEntryType_TRANSCRIPT_ENTRY_TYPE_DISCUSSION},
		{"approve", agentfleetv1.TranscriptEntryType_TRANSCRIPT_ENTRY_TYPE_APPROVE},
		{"abort", agentfleetv1.TranscriptEntryType_TRANSCRIPT_ENTRY_TYPE_ABORT},
		{"question", agentfleetv1.TranscriptEntryType_TRANSCRIPT_ENTRY_TYPE_QUESTION},
		{"answer", agentfleetv1.TranscriptEntryType_TRANSCRIPT_ENTRY_TYPE_ANSWER},
		{"tool_call", agentfleetv1.TranscriptEntryType_TRANSCRIPT_ENTRY_TYPE_TOOL_CALL},
		{"system", agentfleetv1.TranscriptEntryType_TRANSCRIPT_ENTRY_TYPE_SYSTEM},
		{"assistant", agentfleetv1.TranscriptEntryType_TRANSCRIPT_ENTRY_TYPE_ASSISTANT},
		{"user", agentfleetv1.TranscriptEntryType_TRANSCRIPT_ENTRY_TYPE_USER},
		{"result", agentfleetv1.TranscriptEntryType_TRANSCRIPT_ENTRY_TYPE_RESULT},
		{"interrupt", agentfleetv1.TranscriptEntryType_TRANSCRIPT_ENTRY_TYPE_INTERRUPT},
	}
	for _, c := range cases {
		if got := stringToProtoType(c.str); got != c.enum {
			t.Errorf("stringToProtoType(%q) = %v, want %v", c.str, got, c.enum)
		}
		if got := protoTypeToString(c.enum); got != c.str {
			t.Errorf("protoTypeToString(%v) = %q, want %q", c.enum, got, c.str)
		}
	}

	if got := stringToProtoType("garbage"); got != agentfleetv1.TranscriptEntryType_TRANSCRIPT_ENTRY_TYPE_UNSPECIFIED {
		t.Errorf("stringToProtoType(garbage) = %v, want UNSPECIFIED", got)
	}
}
