package mcpserver

import (
	"testing"

	agentfleetv1 "github.com/MohammadBnei/agent-fleet/proto/gen/go/agentfleet/v1"
)

// This map is deliberately NOT total, and that is the thing worth pinning.
//
// localapi's copy is the worker's general write path and must cover the whole
// enum. This one is the AGENT's write path — it is reached only from the MCP
// tools an agent can call (send_message, wait_for_messages, AskUserQuestion),
// so it is an authorization boundary as much as a mapping. Completing it
// "for consistency" would let an agent post a permission_response and answer
// its own permission prompt, or write a tool_call telemetry row attributing
// changes it did not make.
//
// So a missing case here is a feature. It is written down as a test because
// the audit that produced this file read the gap as drift and nearly closed
// it, and the next reader has no way to tell the two apart from the switch
// statement alone.
var agentWritable = map[agentfleetv1.TranscriptEntryType]string{
	agentfleetv1.TranscriptEntryType_TRANSCRIPT_ENTRY_TYPE_DISCUSSION: "discussion",
	agentfleetv1.TranscriptEntryType_TRANSCRIPT_ENTRY_TYPE_APPROVE:    "approve",
	agentfleetv1.TranscriptEntryType_TRANSCRIPT_ENTRY_TYPE_ABORT:      "abort",
	agentfleetv1.TranscriptEntryType_TRANSCRIPT_ENTRY_TYPE_QUESTION:   "question",
	agentfleetv1.TranscriptEntryType_TRANSCRIPT_ENTRY_TYPE_ANSWER:     "answer",
}

func TestAgentCanWriteExactlyTheseTypes(t *testing.T) {
	values := agentfleetv1.TranscriptEntryType(0).Descriptor().Values()
	for i := range values.Len() {
		v := agentfleetv1.TranscriptEntryType(values.Get(i).Number())
		if v == agentfleetv1.TranscriptEntryType_TRANSCRIPT_ENTRY_TYPE_UNSPECIFIED {
			continue
		}
		want, allowed := agentWritable[v]
		got := protoTypeToString(v)
		if allowed && got != want {
			t.Errorf("protoTypeToString(%v) = %q, want %q", v, got, want)
		}
		// The load-bearing half: everything else must stay unmapped.
		if !allowed && got != "" {
			t.Errorf("protoTypeToString(%v) = %q — an agent must not be able to write this type; if that is now intended, say so here", v, got)
		}
		if allowed {
			if back := stringToProtoType(want); back != v {
				t.Errorf("stringToProtoType(%q) = %v, want %v", want, back, v)
			}
		}
	}
	// And nothing outside the set can be written by NAME either — the string
	// side is the one an agent actually reaches.
	for _, s := range []string{"permission_response", "permission_request", "tool_call", "system", "result"} {
		if got := stringToProtoType(s); got != agentfleetv1.TranscriptEntryType_TRANSCRIPT_ENTRY_TYPE_UNSPECIFIED {
			t.Errorf("stringToProtoType(%q) = %v — an agent must not be able to write this type", s, got)
		}
	}
}
