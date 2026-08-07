package transcript

import "testing"

// Literal port of the fixture set implied by worker/src/session.ts's
// comments — this is the single highest-value test in the whole rewrite,
// since a regex-behavior drift here silently breaks the human approval gate.
func TestIsApproval(t *testing.T) {
	cases := []struct {
		name string
		e    Entry
		want bool
	}{
		{"type approve short-circuits true", Entry{Type: "approve", Text: "whatever"}, true},
		{"type abort short-circuits false", Entry{Type: "abort", Text: "approved"}, false},
		{"text: approved", Entry{Text: "approved"}, true},
		{"text: approve", Entry{Text: "I approve"}, true},
		{"text: LGTM", Entry{Text: "LGTM"}, true},
		{"text: lgtm lowercase", Entry{Text: "lgtm, go for it"}, true},
		{"text: ship it", Entry{Text: "ship it"}, true},
		{"text: go ahead", Entry{Text: "go ahead"}, true},
		{"text: unrelated discussion", Entry{Text: "what about the edge case"}, false},
		{"text: substring should not match (approx)", Entry{Text: "approximately done"}, false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := IsApproval(c.e); got != c.want {
				t.Errorf("IsApproval(%+v) = %v, want %v", c.e, got, c.want)
			}
		})
	}
}

func TestIsAbort(t *testing.T) {
	cases := []struct {
		name string
		e    Entry
		want bool
	}{
		{"type abort short-circuits true", Entry{Type: "abort", Text: "whatever"}, true},
		{"type approve short-circuits false", Entry{Type: "approve", Text: "stop"}, false},
		{"text: stop", Entry{Text: "stop"}, true},
		{"text: abort", Entry{Text: "abort this"}, true},
		{"text: cancel", Entry{Text: "please cancel"}, true},
		{"text: kill", Entry{Text: "kill it"}, true},
		{"text: unrelated discussion", Entry{Text: "looks good so far"}, false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := IsAbort(c.e); got != c.want {
				t.Errorf("IsAbort(%+v) = %v, want %v", c.e, got, c.want)
			}
		})
	}
}
