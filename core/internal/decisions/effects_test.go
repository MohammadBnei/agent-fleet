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
