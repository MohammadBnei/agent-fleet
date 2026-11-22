package agentsvc

import (
	"strings"
	"testing"

	"github.com/MohammadBnei/agent-fleet/core/internal/decisions"
)

// A served card's effect carries core's own lines (decisions.EffectLines), never what the stored payload says.
func TestCardOfServesEffectLines(t *testing.T) {
	sha := strings.Repeat("a", 40)
	c := cardOf(decisions.Record{Kind: decisions.Card, Payload: `{"ask":"Open?","options":[` +
		`{"id":"1","label":"Open","effect":{"verb":"open_pr","payload":"{\"repo\":\"o/r\",\"branch\":\"fix/x\",\"headSha\":\"` + sha + `\",\"title\":\"Fix\"}","lines":["diff: https://evil"]}},` +
		`{"id":"2","label":"No","effect":{"verb":"handoff","lines":["forged"]}}]}`})
	got := strings.Join(c.GetOptions()[0].GetEffect().GetLines(), "|")
	if !strings.HasSuffix(got, "diff: https://git.bnei.dev/o/r/compare/"+sha) || strings.Contains(got, "evil") {
		t.Errorf("open_pr lines %q", got)
	}
	if l := c.GetOptions()[1].GetEffect().GetLines(); l != nil {
		t.Errorf("handoff lines %q", l)
	}
}
