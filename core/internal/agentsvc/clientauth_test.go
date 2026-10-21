package agentsvc

import (
	"net/http"
	"strings"
	"testing"

	"github.com/MohammadBnei/agent-fleet/core/internal/definitions"
)

// docs/adr/0066 §3: a bearer names exactly one enabled client; a short or
// unset token disables its client, never opens it; an in-process client
// (no tokenRef) never matches; two comma-separated tokens both work.
func TestClientAuth_Match(t *testing.T) {
	tokA, tokA2, tokB := strings.Repeat("a", 32), strings.Repeat("c", 40), strings.Repeat("b", 32)
	env := map[string]string{
		"FLEET_CLIENT_TOKEN_A":     tokA + "," + tokA2,
		"FLEET_CLIENT_TOKEN_B":     tokB,
		"FLEET_CLIENT_TOKEN_SHORT": "tooshort",
	}
	c := &clientAuth{getenv: func(k string) string { return env[k] }, clients: func() []definitions.Client {
		return []definitions.Client{
			{Name: "a", Spec: definitions.ClientSpec{TokenRef: "FLEET_CLIENT_TOKEN_A"}},
			{Name: "b", Spec: definitions.ClientSpec{TokenRef: "FLEET_CLIENT_TOKEN_B"}},
			{Name: "short", Spec: definitions.ClientSpec{TokenRef: "FLEET_CLIENT_TOKEN_SHORT"}},
			{Name: "unset", Spec: definitions.ClientSpec{TokenRef: "FLEET_CLIENT_TOKEN_UNSET"}},
			{Name: "bridge"},
		}
	}}
	h := func(v string) http.Header { return http.Header{"Authorization": {v}} }
	for _, tc := range []struct{ auth, want string }{
		{"Bearer " + tokA, "a"},
		{"Bearer " + tokA2, "a"},
		{"Bearer " + tokB, "b"},
		{"Bearer tooshort", ""},
		{"Bearer ", ""},
		{"", ""},
		{tokA, ""},
		{"Bearer " + tokA + "x", ""},
	} {
		if got := c.match(h(tc.auth)); got != tc.want {
			t.Errorf("match(%q) = %q, want %q", tc.auth, got, tc.want)
		}
	}
}
