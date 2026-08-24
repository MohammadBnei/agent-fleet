package localapi

import (
	"testing"

	agentfleetv1 "github.com/MohammadBnei/agent-fleet/proto/gen/go/agentfleet/v1"
)

// These two maps are localapi's half of the wire enum: stringToProtoType is
// the worker's write path (messageHandler; worker/src/session.ts's
// logSdkMessage sends these exact type strings) and protoTypeToString is the
// read path the /human-messages SSE feed hands back.
//
// They must be TOTAL. A string with no case coerces to UNSPECIFIED, which
// coreserver maps back to "" — and "" is NOT stored. The transcript's
// CHECK constraint is `type IN (...) OR type IS NULL`, and "" is neither, so
// the INSERT is rejected outright (SQLSTATE 23514) and the message is LOST.
// Verified against a real Postgres: 'approve' and "" are both refused, NULL
// and 'tool_call' are accepted.
//
// So the failure is loud rather than silent — the database is a working
// backstop here — but it surfaces as an opaque check-constraint violation
// from deep inside Append, on a path whose actual defect is one missing case
// in a switch statement two components away.
//
// Three cases were missing when this was written — "question", "answer" and
// "tool_call" on the write side, "tool_call" on the read side — and the tests
// that were meant to guard them did not fail, because they enumerated the
// same list by hand that the map did. A hand-written case list cannot catch a
// forgotten case: it tests what someone remembered, which is the identical
// failure mode as the code it is checking. Hence the walk below over the
// enum's own descriptor, which is the only list that cannot drift.
func writableTypes() []agentfleetv1.TranscriptEntryType {
	values := agentfleetv1.TranscriptEntryType(0).Descriptor().Values()
	var out []agentfleetv1.TranscriptEntryType
	for i := range values.Len() {
		v := agentfleetv1.TranscriptEntryType(values.Get(i).Number())
		// UNSPECIFIED is the zero value, not a type: it is what a MISSING
		// case produces, so it cannot also be a case.
		if v == agentfleetv1.TranscriptEntryType_TRANSCRIPT_ENTRY_TYPE_UNSPECIFIED {
			continue
		}
		out = append(out, v)
	}
	return out
}

// Fails on the next enum value added to transcript.proto until someone
// handles it here — which is the entire point. Adding a value is a decision
// about six switch statements, and this is the one that says so out loud.
func TestTypeMapsCoverEveryEnumValue(t *testing.T) {
	for _, want := range writableTypes() {
		str := protoTypeToString(want)
		if str == "" {
			t.Errorf("protoTypeToString(%v) = \"\": every enum value needs a case, or the SSE feed hands the worker an untyped entry", want)
			continue
		}
		if got := stringToProtoType(str); got != want {
			t.Errorf("round trip %v -> %q -> %v: the two maps disagree", want, str, got)
		}
	}
}

// The reverse direction, so a string the maps produce is never one they
// cannot read back.
func TestStringToProtoTypeRejectsOnlyGarbage(t *testing.T) {
	if got := stringToProtoType("garbage"); got != agentfleetv1.TranscriptEntryType_TRANSCRIPT_ENTRY_TYPE_UNSPECIFIED {
		t.Errorf("stringToProtoType(garbage) = %v, want UNSPECIFIED", got)
	}
	if got := stringToProtoType(""); got != agentfleetv1.TranscriptEntryType_TRANSCRIPT_ENTRY_TYPE_UNSPECIFIED {
		t.Errorf("stringToProtoType(\"\") = %v, want UNSPECIFIED", got)
	}
}
