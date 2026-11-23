package cardevents

import "testing"

func TestSubjectFor(t *testing.T) {
	for _, c := range []struct{ subject, want string }{
		{"forge/acme/web/pr/1", "forge/acme/web/pr/1"},
		{"alert/0123456789abcdef", "alert/0123456789abcdef"},
		{"character/ada", "agent/character/ada"},
		{"", "fleet/session/s1"},
	} {
		if got := subjectFor(row{subject: c.subject, session: "s1"}); got != c.want {
			t.Errorf("subjectFor(%q) = %q, want %q", c.subject, got, c.want)
		}
	}
}
