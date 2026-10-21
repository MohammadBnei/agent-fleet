package coreserver

import (
	"testing"

	"github.com/MohammadBnei/agent-fleet/core/internal/definitions"
)

// docs/adr/0068 §4: the worker's script-write ask is on exactly when the
// definition lists an island host.
func TestAgentSpec_ScriptAskFollowsEmbedOrigins(t *testing.T) {
	for _, tc := range []struct {
		embed *definitions.Embed
		want  bool
	}{
		{nil, false},
		{&definitions.Embed{}, false},
		{&definitions.Embed{Origins: []string{"https://notes.bnei.dev"}}, true},
	} {
		if got := agentSpec(&definitions.Spec{Embed: tc.embed}).GetScriptAsk(); got != tc.want {
			t.Errorf("embed %+v: scriptAsk %v, want %v", tc.embed, got, tc.want)
		}
	}
}
