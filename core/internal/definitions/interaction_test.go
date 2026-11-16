package definitions

import (
	"strings"
	"testing"
)

// docs/adr/0070 §2: AskUserQuestion is in every effective sidecarTools.
// Release two dropped interaction; load_test.go's "interaction key unknown"
// case covers the strict decode refusing it.
func TestLoad_AskUserQuestionInEverySidecarTools(t *testing.T) {
	noAsk := strings.Replace(baseYAML, "sidecarTools: [AskUserQuestion]", "sidecarTools: [draft_card]", 1)
	res, errs := load(t, map[string]string{
		"definitions/base.yaml": noAsk,
		"definitions/aaa.yaml":  childYAML("aaa", nil),
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
}
