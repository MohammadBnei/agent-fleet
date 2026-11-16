package definitions

import (
	"strings"
	"testing"
)

// docs/adr/0070 §2, release one: AskUserQuestion is in every effective
// sidecarTools, and interaction is optional.
func TestLoad_AskUserQuestionAndOptionalInteraction(t *testing.T) {
	noAsk := strings.Replace(baseYAML, "sidecarTools: [AskUserQuestion]", "sidecarTools: [draft_card]", 1)
	noInteraction := strings.Replace(noAsk, "  interaction: { allow: [ask] }\n", "", 1)
	res, errs := load(t, map[string]string{
		"definitions/base.yaml": noInteraction,
		"definitions/aaa.yaml":    childYAML("aaa", nil),
	})
	if len(errs) != 0 {
		t.Fatal(errs)
	}
	a := res["aaa"]
	if a.Err != nil {
		t.Fatal(a.Err)
	}
	if got := strings.Join(a.Effective.SidecarTools, ","); got != "draft_card,AskUserQuestion" {
		t.Fatalf("sidecarTools = %s", got)
	}
	if a.Effective.Interaction != nil {
		t.Fatal("interaction should stay absent")
	}
}
