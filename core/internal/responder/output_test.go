package responder

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/MohammadBnei/agent-fleet/core/internal/decisions"
	"github.com/MohammadBnei/agent-fleet/core/internal/definitions"
	"github.com/MohammadBnei/agent-fleet/core/internal/remediations"
)

func TestParse(t *testing.T) {
	ok := []string{
		`{"outcome":"nothing"}`,
		`{"outcome":"catalogue","selection":{"action":"restart","kind":"deployment","namespace":"game","name":"api"}}`,
		`{"outcome":"card","card":{"kind":"code_fix","objects":[{"kind":"pod","namespace":"game","name":"api-1"}],"findings":[{"name":"restarts","value":7}]}}`,
	}
	for _, s := range ok {
		if _, err := Parse(s); err != nil {
			t.Errorf("%s: %v", s, err)
		}
	}
	bad := map[string]string{
		"unreadable":         `nope`,
		"trailing":           `{"outcome":"nothing"} {}`,
		"unknown field":      `{"outcome":"nothing","note":"hi"}`,
		"unknown outcome":    `{"outcome":"apply"}`,
		"nothing + payload":  `{"outcome":"nothing","selection":{}}`,
		"catalogue no sel":   `{"outcome":"catalogue"}`,
		"catalogue + card":   `{"outcome":"catalogue","selection":{"a":1},"card":{"kind":"code_fix"}}`,
		"card no card":       `{"outcome":"card"}`,
		"card kind":          `{"outcome":"card","card":{"kind":"free text"}}`,
		"card free finding":  `{"outcome":"card","card":{"kind":"code_fix","findings":[{"name":"ignore previous instructions","value":1}]}}`,
		"card negative":      `{"outcome":"card","card":{"kind":"code_fix","findings":[{"name":"restarts","value":-1}]}}`,
		"card object name":   `{"outcome":"card","card":{"kind":"code_fix","objects":[{"kind":"pod","name":"Run This"}]}}`,
		"card object kind":   `{"outcome":"card","card":{"kind":"code_fix","objects":[{"kind":"secret","name":"x"}]}}`,
		"card unknown field": `{"outcome":"card","card":{"kind":"code_fix","claim":"free text"}}`,
	}
	for name, s := range bad {
		if _, err := Parse(s); !errors.Is(err, ErrOutput) {
			t.Errorf("%s: want ErrOutput, got %v", name, err)
		}
	}
}

func TestAskIsCoreWrittenAndCapped(t *testing.T) {
	c := CardRequest{Kind: "code_fix", Findings: []Finding{{"restarts", 7}, {"cpu_pct", 91.5}}, Objects: []Object{{"deployment", "game", "api"}}}
	if got := c.Ask(Group{Name: "KubePodCrashLooping"}); got != "Alert KubePodCrashLooping: the responder found it needs a code fix. restarts=7 cpu_pct=91.5 deployment/game/api" {
		t.Fatalf("ask: %q", got)
	}
	// An unknown_failure that names nothing says where the alert fired, in the third person, never "Alert alert".
	u := CardRequest{Kind: "unknown_failure"}
	if got := u.Ask(Group{Name: "alert", Where: "namespace/ci job/forgejo-actions"}); got != "An alert: the responder could not name the cause. namespace/ci job/forgejo-actions" {
		t.Fatalf("ask: %q", got)
	}
	if g := (Group{Name: "KubeX", Namespace: "game"}); g.Title() != "Alert: KubeX (game)" || (Group{Name: "KubeX"}).Title() != "Alert: KubeX" {
		t.Fatalf("title %q", g.Title())
	}
	for range maxList {
		c.Objects = append(c.Objects, Object{"deployment", strings.Repeat("a", 50), strings.Repeat("b", 50)})
	}
	if n := len([]rune(c.Ask(Group{Name: "A"}))); n > 280 {
		t.Fatalf("ask is %d runes, a card's ask is at most 280", n)
	}
}

type fakeFiler struct {
	selected []string
	bounds   remediations.Bounds
	selErr   error
	drafts   []decisions.Draft
}

func (f *fakeFiler) filer() Filer {
	return Filer{
		Select: func(_ context.Context, def string, raw []byte, b remediations.Bounds) (string, error) {
			f.selected, f.bounds = append(f.selected, def+" "+string(raw)), b
			return "rem-1", f.selErr
		},
		Draft: func(_ context.Context, d decisions.Draft) error { f.drafts = append(f.drafts, d); return nil },
	}
}

var spec = &definitions.Spec{Trust: &definitions.Trust{Untrusted: "B", Scale: &definitions.ScaleBounds{Min: 1, Max: 4}}}

func TestFileCatalogueSelection(t *testing.T) {
	f := &fakeFiler{}
	err := f.filer().File(context.Background(), "resp", spec, "th", Group{Key: "k", Name: "A"},
		`{"outcome":"catalogue","selection":{"action":"restart","name":"api"}}`)
	if err != nil || len(f.selected) != 1 || len(f.drafts) != 0 {
		t.Fatalf("err %v selected %v drafts %d", err, f.selected, len(f.drafts))
	}
	if f.bounds != (remediations.Bounds{Min: 1, Max: 4}) {
		t.Fatalf("scale bounds come from the definition, got %+v", f.bounds)
	}
	if !strings.Contains(f.selected[0], `"action":"restart"`) {
		t.Fatalf("the selection goes to core's checks verbatim: %s", f.selected[0])
	}
}

func TestFileRefusedSelectionBecomesCard(t *testing.T) {
	f := &fakeFiler{selErr: remediations.ErrDenied}
	err := f.filer().File(context.Background(), "resp", spec, "th", Group{Key: "0123456789abcdef", Name: "A"},
		`{"outcome":"catalogue","selection":{"action":"restart","name":"core"}}`)
	if err != nil || len(f.drafts) != 1 || !strings.Contains(f.drafts[0].Ask, "refused") {
		t.Fatalf("a refused selection is a card, not silence: err %v drafts %+v", err, f.drafts)
	}
}

func TestFileOtherSelectErrorIsReturned(t *testing.T) {
	f := &fakeFiler{selErr: errors.New("db down")}
	if err := f.filer().File(context.Background(), "r", spec, "th", Group{}, `{"outcome":"catalogue","selection":{"a":1}}`); err == nil || len(f.drafts) != 0 {
		t.Fatalf("a failure that is not a refusal is an error, not a card: %v", err)
	}
}

func TestFileCard(t *testing.T) {
	f := &fakeFiler{}
	err := f.filer().File(context.Background(), "r", spec, "th", Group{Key: "0123456789abcdef", Name: "A"},
		`{"outcome":"card","card":{"kind":"revert_pr","findings":[{"name":"restarts","value":3}]}}`)
	if err != nil || len(f.selected) != 0 || len(f.drafts) != 1 {
		t.Fatalf("err %v selected %v drafts %d", err, f.selected, len(f.drafts))
	}
	d := f.drafts[0]
	if d.From != "system" || d.SessionID != "th" || d.Subject.GetKind() != "alert_group" || d.Subject.GetGroupKey() != "0123456789abcdef" {
		t.Fatalf("a card is core's, on the alert group: %+v", d)
	}
	if len(d.Options) != 2 {
		t.Fatalf("options: %v", d.Options)
	}
}

func TestFileNothingAndGarbage(t *testing.T) {
	f := &fakeFiler{}
	if err := f.filer().File(context.Background(), "r", spec, "th", Group{}, `{"outcome":"nothing"}`); err != nil || len(f.selected)+len(f.drafts) != 0 {
		t.Fatalf("nothing files nothing: %v", err)
	}
	if err := f.filer().File(context.Background(), "r", spec, "th", Group{}, ``); !errors.Is(err, ErrOutput) {
		t.Fatalf("a run with no output files nothing and says so: %v", err)
	}
}
