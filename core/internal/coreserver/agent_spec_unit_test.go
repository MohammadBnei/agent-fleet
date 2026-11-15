package coreserver

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"testing"

	"google.golang.org/protobuf/proto"

	agentfleetv1 "github.com/MohammadBnei/agent-fleet/proto/gen/go/agentfleet/v1"

	"github.com/MohammadBnei/agent-fleet/core/internal/definitions"
)

func specSHA(t *testing.T, a *agentfleetv1.AgentSpec) string {
	t.Helper()
	b, err := proto.MarshalOptions{Deterministic: true}.Marshal(a)
	if err != nil {
		t.Fatal(err)
	}
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:])
}

func mustSpec(t *testing.T, eff *definitions.Spec, lifecycle string) *agentfleetv1.AgentSpec {
	t.Helper()
	a, err := agentSpec(eff, lifecycle)
	if err != nil {
		t.Fatal(err)
	}
	return a
}

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
		if got := mustSpec(t, &definitions.Spec{Embed: tc.embed}, "").GetScriptAsk(); got != tc.want {
			t.Errorf("embed %+v: scriptAsk %v, want %v", tc.embed, got, tc.want)
		}
	}
}

// residentEffective is a resident definition's effective spec, and
// residentGoldenSHA the agent_spec_sha the code of main BEFORE docs/adr/0070
// slice 1 computed for it (recorded by running that code on this fixture). A
// live pod echoes that sha, so it must not move: a different value refuses
// every running definition pod at ADR-0066 §8's gate after the deploy. Do not
// regenerate it from the current code.
const (
	residentEffective = `{"limits":{"concurrency":1,"maxTurns":60},"mode":{"default":"default","allow":["default","plan"]},"tools":{"allow":["Read","Bash(git add:*)"],"deny":["Edit(.git/**)"]},"sidecarTools":["send_message","wait_for_messages"],"model":{"default":"claude-opus-5","allow":["claude-opus-5"]},"thinking":{"default":"medium"},"systemPrompt":{"append":"Be brief."},"embed":{"origins":["https://notes.bnei.dev"]},"lifecycle":{"allow":["resident","single-shot"]}}`
	residentGoldenSHA = "3772a389a28bf1ee1aa0d1a062674f32dbdccb73d5cd4b1a02d1be923613f464"
)

func TestAgentSpec_ResidentShaIsByteIdenticalToBeforeSlice1(t *testing.T) {
	var eff definitions.Spec
	if err := json.Unmarshal([]byte(residentEffective), &eff); err != nil {
		t.Fatal(err)
	}
	// "" is what a resident session's row reads as; a spec that also carries
	// an output schema must not leak it into a resident session either.
	withSchema := eff
	withSchema.Output = &definitions.Output{Schema: map[string]any{"type": "object"}}
	for name, e := range map[string]*definitions.Spec{"no schema": &eff, "schema, resident": &withSchema} {
		a := mustSpec(t, e, "")
		if a.GetLifecycle() != "" || a.GetOutputSchema() != "" {
			t.Errorf("%s: resident spec carries lifecycle %q schema %q", name, a.GetLifecycle(), a.GetOutputSchema())
		}
		if got := specSHA(t, a); got != residentGoldenSHA {
			t.Errorf("%s: resident agent_spec_sha %s, want the pre-slice %s", name, got, residentGoldenSHA)
		}
	}
	// Resident spelled out reads as resident too.
	if got := specSHA(t, mustSpec(t, &eff, "resident")); got != residentGoldenSHA {
		t.Errorf("explicit resident: sha %s, want %s", got, residentGoldenSHA)
	}
}

func TestAgentSpec_SingleShotCarriesLifecycleAndSchema(t *testing.T) {
	eff := definitions.Spec{Output: &definitions.Output{Schema: map[string]any{
		"type": "object", "properties": map[string]any{"n": map[string]any{"type": "integer"}},
	}}}
	a := mustSpec(t, &eff, "single-shot")
	if a.GetLifecycle() != "single-shot" || a.GetOutputSchema() != `{"properties":{"n":{"type":"integer"}},"type":"object"}` {
		t.Errorf("single-shot: lifecycle %q schema %q", a.GetLifecycle(), a.GetOutputSchema())
	}
	if specSHA(t, a) == specSHA(t, mustSpec(t, &eff, "")) {
		t.Error("single-shot spec must differ from the resident one")
	}
	// No output.schema: free-text results, lifecycle still told.
	free := mustSpec(t, &definitions.Spec{}, "single-shot")
	if free.GetLifecycle() != "single-shot" || free.GetOutputSchema() != "" {
		t.Errorf("no schema: lifecycle %q schema %q", free.GetLifecycle(), free.GetOutputSchema())
	}
}
