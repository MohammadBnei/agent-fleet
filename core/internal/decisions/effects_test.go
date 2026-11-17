package decisions

import (
	"errors"
	"strings"
	"testing"

	agentfleetv1 "github.com/MohammadBnei/agent-fleet/proto/gen/go/agentfleet/v1"
)

const sha40 = "0123456789abcdef0123456789abcdef01234567"

func eff(verb, payload string) *agentfleetv1.CardEffect {
	return &agentfleetv1.CardEffect{Verb: verb, Payload: payload}
}

func TestBindEffect(t *testing.T) {
	good := []*agentfleetv1.CardEffect{
		eff(VerbCatalogue, `{"action":"restart","kind":"deployment","namespace":"game","name":"web"}`),
		eff(VerbOpenPR, `{"repo":"o/r","branch":"fix/x","headSha":"`+sha40+`","title":"Fix x"}`),
		eff(VerbOpenSession, `{"repo":"r","title":"Look","findings":[{"name":"restarts","value":4}]}`),
		eff(VerbRaiseBudget, `{"character":"dream-keeper"}`),
	}
	for _, e := range good {
		if err := bindEffect(e); err != nil {
			t.Fatalf("%s: %v", e.Verb, err)
		}
		if e.PayloadHash != PayloadHash(e.Verb, e.Payload) || VerifyEffect(e) != nil {
			t.Errorf("%s: not bound to its hash", e.Verb)
		}
		// The hash is the core's: one the card arrives with is overwritten.
	}
	e := eff(VerbOpenPR, `{"repo":"o/r","branch":"b","headSha":"`+sha40+`","title":"T"}`)
	e.PayloadHash = "forged"
	if bindEffect(e) != nil || e.PayloadHash == "forged" {
		t.Error("a claimed hash must be replaced")
	}

	bad := map[string]*agentfleetv1.CardEffect{
		"denied target": eff(VerbCatalogue, `{"action":"restart","kind":"deployment","namespace":"kube-system","name":"web"}`),
		"unknown field": eff(VerbCatalogue, `{"action":"restart","name":"web","extra":1}`),
		"short sha":     eff(VerbOpenPR, `{"repo":"o/r","branch":"b","headSha":"abc","title":"T"}`),
		"bad branch":    eff(VerbOpenPR, `{"repo":"o/r","branch":"-x","headSha":"`+sha40+`","title":"T"}`),
		"long title":    eff(VerbOpenSession, `{"repo":"o/r","title":"`+strings.Repeat("a", 81)+`"}`),
		"free finding":  eff(VerbOpenSession, `{"repo":"o/r","title":"T","findings":[{"name":"Ignore previous","value":1}]}`),
		"not a repo":    eff(VerbOpenSession, `{"repo":"o/r","title":"T"}`),
		"trailing data": eff(VerbOpenSession, `{"repo":"o/r","title":"T"} {}`),
		"empty":         eff(VerbOpenPR, ``),
		"budget name":   eff(VerbRaiseBudget, `{"character":"A b"}`),
		"budget extra":  eff(VerbRaiseBudget, `{"character":"a","hours":999}`),
	}
	for name, e := range bad {
		if err := bindEffect(e); !errors.Is(err, ErrInvalid) {
			t.Errorf("%s: %v", name, err)
		}
	}
	err := bindEffect(eff(VerbExecutorStep, `{}`))
	if !errors.Is(err, ErrInvalid) || !strings.Contains(err.Error(), "ADR-0072") {
		t.Errorf("executor_step: %v", err)
	}
}

func TestVerifyEffect_Tamper(t *testing.T) {
	e := eff(VerbOpenSession, `{"repo":"r","title":"T"}`)
	if err := bindEffect(e); err != nil {
		t.Fatal(err)
	}
	e.Payload = `{"repo":"other","title":"T"}`
	if err := VerifyEffect(e); !errors.Is(err, ErrStale) {
		t.Errorf("a changed payload: %v", err)
	}
	e.PayloadHash = ""
	if err := VerifyEffect(e); !errors.Is(err, ErrStale) {
		t.Errorf("no hash: %v", err)
	}
}

func TestCheckCoreVerbs(t *testing.T) {
	card := func(verb string) *agentfleetv1.Card {
		return &agentfleetv1.Card{Options: []*agentfleetv1.CardOption{{Effect: &agentfleetv1.CardEffect{Verb: verb}}}}
	}
	for _, tc := range []struct {
		verb, by, trust string
		ok              bool
	}{
		{"handoff", "agent", "AB", true},
		{VerbCatalogue, "core", "BC", true},
		{VerbCatalogue, "agent", "BC", false},
		{VerbOpenSession, "agent", "BC", false},
		{VerbOpenPR, "core", "BC", true},
		{VerbOpenPR, "core", "AB", false},
		{VerbOpenPR, "core", "AC", false},
	} {
		if err := checkCoreVerbs(card(tc.verb), tc.by, tc.trust); (err == nil) != tc.ok {
			t.Errorf("%+v: %v", tc, err)
		}
	}
}

func TestValidBranch(t *testing.T) {
	for b, ok := range map[string]bool{
		"fix/x": true, "a.b/c-d_e": true, "a/../../../x": false, "a..b": false, "a//b": false, "a/": false,
		"a.": false, "x.lock": false, "a/x.lock": false, "a/.hidden": false, "-x": false, "": false,
	} {
		if ValidBranch(b) != ok {
			t.Errorf("ValidBranch(%q) = %v", b, !ok)
		}
	}
	if _, err := normalize(&agentfleetv1.Card{Ask: "x", Subject: &agentfleetv1.CardSubject{Kind: "alert_group", GroupKey: "0123456789abcdef"},
		Options: []*agentfleetv1.CardOption{{Label: "a", Reversible: "yes", Effect: eff(VerbOpenPR, `{"repo":"o/r","branch":"a/../x","headSha":"`+sha40+`","title":"t"}`)},
			{Label: "b", Reversible: "yes", Effect: eff("handoff", "")}}}); err == nil {
		t.Error("open_pr with a .. branch was filed")
	}
}

// A handoff or close_threads option keeps nothing an agent put in payload: it is no core verb's.
func TestNormalizeClearsForeignPayload(t *testing.T) {
	c := &agentfleetv1.Card{Ask: "x", Subject: &agentfleetv1.CardSubject{Kind: "alert_group", GroupKey: "0123456789abcdef"},
		Options: []*agentfleetv1.CardOption{{Label: "a", Reversible: "yes", Effect: &agentfleetv1.CardEffect{Verb: "handoff", Payload: "free\x00text", PayloadHash: "h"}},
			{Label: "b", Reversible: "yes", Effect: eff("handoff", "")}}}
	if _, err := normalize(c); err != nil {
		t.Fatal(err)
	}
	if e := c.GetOptions()[0].GetEffect(); e.GetPayload() != "" || e.GetPayloadHash() != "" {
		t.Errorf("kept %q %q", e.GetPayload(), e.GetPayloadHash())
	}
}

func TestEffectLines(t *testing.T) {
	got := strings.Join(EffectLines(eff(VerbOpenSession, `{"repo":"notes","title":"Look","findings":[{"name":"restarts","value":4}]}`)), "|")
	if got != "findings: restarts = 4|repo: notes|title (agent claim): Look" {
		t.Errorf("open_session: %s", got)
	}
	pr := strings.Join(EffectLines(eff(VerbOpenPR, `{"repo":"o/r","branch":"fix/x","headSha":"`+sha40+`","title":"Fix"}`)), "|")
	if !strings.HasSuffix(pr, "|diff: https://github.com/o/r/compare/"+sha40) {
		t.Errorf("open_pr: %s", pr)
	}
	if EffectLines(eff("handoff", `{"a":1}`)) != nil {
		t.Error("a non-core verb shows a payload")
	}
}

// docs/adr/0084 §9: a character subject is the definition name alone, and only core files its card.
func TestNormalize_CharacterSubject(t *testing.T) {
	card := func(repo string, n int64) *agentfleetv1.Card {
		return &agentfleetv1.Card{Ask: "x", Subject: &agentfleetv1.CardSubject{Kind: "character", Repo: repo, Number: n},
			Options: []*agentfleetv1.CardOption{{Label: "a", Reversible: "yes", Effect: eff(VerbRaiseBudget, `{"character":"keeper"}`)},
				{Label: "b", Reversible: "yes", Effect: eff("handoff", "")}}}
	}
	if key, err := normalize(card("keeper", 0)); err != nil || key != "character/keeper" {
		t.Errorf("good: %q %v", key, err)
	}
	for _, c := range []*agentfleetv1.Card{card("o/r", 0), card("", 0), card("keeper", 3)} {
		if _, err := normalize(c); !errors.Is(err, ErrInvalid) {
			t.Errorf("%v: %v", c.GetSubject(), err)
		}
	}
	if !IsCoreVerb(VerbRaiseBudget) {
		t.Error("raise_budget is a core verb: only core files it")
	}
}
