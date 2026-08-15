package config

import "testing"

// An unparseable selector must mean "schedule anywhere", never "match nothing".
//
// A selector matching no node leaves sessions Pending with no error anyone
// sees — the pod is never created and the session simply looks hung. Degrading
// a typo to the previous unconstrained behaviour is the safe direction; the
// alternative fails silently and looks like a broken fleet.
func TestNodeSelectorMap(t *testing.T) {
	for _, tc := range []struct {
		name string
		in   string
		want map[string]string
	}{
		{"the real one", "agent-fleet.dev/session-node=true", map[string]string{"agent-fleet.dev/session-node": "true"}},
		{"tolerates whitespace", "  key = value  ", map[string]string{"key": "value"}},
		{"unset means unconstrained", "", nil},
		{"no separator is not a selector", "no-equals-sign", nil},
		{"an empty key is not a selector", "=orphan-value", nil},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got := Config{SessionNodeSelector: tc.in}.NodeSelectorMap()
			if len(got) != len(tc.want) {
				t.Fatalf("NodeSelectorMap(%q) = %v, want %v", tc.in, got, tc.want)
			}
			for k, v := range tc.want {
				if got[k] != v {
					t.Errorf("NodeSelectorMap(%q)[%s] = %q, want %q", tc.in, k, got[k], v)
				}
			}
		})
	}
}
