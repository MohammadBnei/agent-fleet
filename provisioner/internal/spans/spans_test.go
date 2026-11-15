package spans

import (
	"strings"
	"testing"
)

func TestRedact(t *testing.T) {
	in := "clone https://x-access-token:ghs_abc123@example.com/o/r.git: fatal; Authorization: Bearer abc.def ghp_XYZ9 github_pat_11AA_bb av_sess_q1"
	got := Redact(in)
	for _, bad := range []string{"x-access-token", "ghs_abc123", "abc.def", "ghp_XYZ9", "github_pat_11AA_bb", "av_sess_q1"} {
		if strings.Contains(got, bad) {
			t.Errorf("%q survived: %s", bad, got)
		}
	}
	if !strings.Contains(got, "https://example.com/o/r.git") {
		t.Errorf("url host/path lost: %s", got)
	}
}
