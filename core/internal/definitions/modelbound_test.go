package definitions

import "testing"

func TestModelBound(t *testing.T) {
	alert := &Spec{Model: &Model{Default: "z-ai-glm-5-3", Allow: []string{"z-ai-glm-5-3"}}}
	free := &Spec{} // no model: Claude Code picks, only the ceiling bounds an override
	for _, tc := range []struct {
		name  string
		eff   *Spec
		model string
		ok    bool
	}{
		{"allowed", alert, "z-ai-glm-5-3", true},
		{"alert-responder refuses a claude id", alert, "claude-opus-5", false},
		{"alert-responder refuses another gateway id", alert, "kimi-k2-7-code", false},
		{"outside the ceiling", free, "claude-opus-4-8", false},
		{"no model set, in ceiling", free, "claude-sonnet-5", true},
		{"empty", free, "", false},
	} {
		if got := ModelBound(tc.eff, tc.model); (got == "") != tc.ok {
			t.Errorf("%s: ModelBound = %q", tc.name, got)
		}
	}
}
