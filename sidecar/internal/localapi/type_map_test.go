package localapi

import (
	"testing"

	agentfleetv1 "github.com/MohammadBnei/agent-fleet/proto/gen/go/agentfleet/v1"
)

// TestStringToProtoType_RoundTrip covers messageHandler's write-path
// mapping (worker/src/planning.ts's logSdkMessage sends these exact type
// strings) and humanMessagesHandler's read-path reverse. Before the four
// SDK message types below had enum values, they silently coerced to
// UNSPECIFIED -> "" downstream in coreserver, which the Discord relay
// allowlist (core/internal/transcript/relay.go) explicitly treats as
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
		{"system", agentfleetv1.TranscriptEntryType_TRANSCRIPT_ENTRY_TYPE_SYSTEM},
		{"assistant", agentfleetv1.TranscriptEntryType_TRANSCRIPT_ENTRY_TYPE_ASSISTANT},
		{"user", agentfleetv1.TranscriptEntryType_TRANSCRIPT_ENTRY_TYPE_USER},
		{"result", agentfleetv1.TranscriptEntryType_TRANSCRIPT_ENTRY_TYPE_RESULT},
		{"permission_mode", agentfleetv1.TranscriptEntryType_TRANSCRIPT_ENTRY_TYPE_PERMISSION_MODE},
	}
	for _, c := range cases {
		if got := stringToProtoType(c.str); got != c.enum {
			t.Errorf("stringToProtoType(%q) = %v, want %v", c.str, got, c.enum)
		}
	}

	if got := stringToProtoType("garbage"); got != agentfleetv1.TranscriptEntryType_TRANSCRIPT_ENTRY_TYPE_UNSPECIFIED {
		t.Errorf("stringToProtoType(garbage) = %v, want UNSPECIFIED", got)
	}
}

func TestProtoTypeToString_RoundTrip(t *testing.T) {
	cases := []struct {
		enum agentfleetv1.TranscriptEntryType
		str  string
	}{
		{agentfleetv1.TranscriptEntryType_TRANSCRIPT_ENTRY_TYPE_DISCUSSION, "discussion"},
		{agentfleetv1.TranscriptEntryType_TRANSCRIPT_ENTRY_TYPE_APPROVE, "approve"},
		{agentfleetv1.TranscriptEntryType_TRANSCRIPT_ENTRY_TYPE_ABORT, "abort"},
		{agentfleetv1.TranscriptEntryType_TRANSCRIPT_ENTRY_TYPE_QUESTION, "question"},
		{agentfleetv1.TranscriptEntryType_TRANSCRIPT_ENTRY_TYPE_ANSWER, "answer"},
		{agentfleetv1.TranscriptEntryType_TRANSCRIPT_ENTRY_TYPE_SYSTEM, "system"},
		{agentfleetv1.TranscriptEntryType_TRANSCRIPT_ENTRY_TYPE_ASSISTANT, "assistant"},
		{agentfleetv1.TranscriptEntryType_TRANSCRIPT_ENTRY_TYPE_USER, "user"},
		{agentfleetv1.TranscriptEntryType_TRANSCRIPT_ENTRY_TYPE_RESULT, "result"},
		{agentfleetv1.TranscriptEntryType_TRANSCRIPT_ENTRY_TYPE_PERMISSION_MODE, "permission_mode"},
	}
	for _, c := range cases {
		if got := protoTypeToString(c.enum); got != c.str {
			t.Errorf("protoTypeToString(%v) = %q, want %q", c.enum, got, c.str)
		}
	}
}
