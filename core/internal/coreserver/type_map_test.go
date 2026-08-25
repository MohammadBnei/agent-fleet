package coreserver

import (
	"testing"

	agentfleetv1 "github.com/MohammadBnei/agent-fleet/proto/gen/go/agentfleet/v1"
)

// TestStringToProtoType_RoundTrip covers this package's own copy of the
// write-path mapping — the sidecar's SendMessage RPC call carries the type
// as this enum, and coreserver.SendMessage's protoTypeToString(req.GetType())
// is what actually lands in the `type` column the Discord relay allowlist
// column. The leak this originally guarded — UNSPECIFIED coercing to "",
// which the Discord relay allowlist treated as safe to post — is gone with
// the relay itself (docs/adr/0048); Discord is outbound-only and relays no
// transcript content.
//
// The mapping still matters, for a plainer reason: "" is not a storable
// type. transcript's CHECK is `type IN (...) OR type IS NULL`, and "" is
// neither, so a missing case does not write a mislabelled row — the INSERT
// is rejected and the message is lost. See TestTypeMapsCoverEveryEnumValue
// below, which is the guard that actually holds; the hand-written cases here
// cannot catch a forgotten one.
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

// The hand-written cases above test what someone remembered to list, which is
// the same failure mode as the switch statements they check — a forgotten
// case is invisible to both. localapi's copy had three missing for exactly
// that reason. This walks the enum's own descriptor instead, which is the one
// list that cannot drift, and fails on the next value added to
// transcript.proto until it is handled here.
//
// It matters most in THIS package: coreserver.SendMessage's
// protoTypeToString(req.GetType()) is what actually lands in the `type`
// column, and "" is not a storable value — the CHECK constraint refuses it,
// so a missing case costs the whole message rather than just its label.
func TestTypeMapsCoverEveryEnumValue(t *testing.T) {
	values := agentfleetv1.TranscriptEntryType(0).Descriptor().Values()
	for i := range values.Len() {
		want := agentfleetv1.TranscriptEntryType(values.Get(i).Number())
		// UNSPECIFIED is what a MISSING case produces, so it cannot also be
		// a case.
		if want == agentfleetv1.TranscriptEntryType_TRANSCRIPT_ENTRY_TYPE_UNSPECIFIED {
			continue
		}
		str := protoTypeToString(want)
		if str == "" {
			t.Errorf("protoTypeToString(%v) = \"\": this value would be stored as an untyped transcript row", want)
			continue
		}
		if got := stringToProtoType(str); got != want {
			t.Errorf("round trip %v -> %q -> %v: the two maps disagree", want, str, got)
		}
	}
}
