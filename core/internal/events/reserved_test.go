package events

import "testing"

func TestTypeReaches(t *testing.T) {
	for p, want := range map[string]bool{
		"":                               true,
		"dev.":                           true,
		"dev.bnei.":                      true,
		"dev.bnei.fleet.":                true,
		"dev.bnei.fleet.session.":        true,
		"dev.bnei.fleet.session.waiting": true,
		"dev.bnei.game.needs_you.":       true,
		"dev.bnei.github.":               false,
		"dev.bnei.github.pull_request":   false,
		"dev.bnei.alert.":                false,
		"dev.bnei.agent.":                false,
		"dev.bnei.fleetx.":               false,
	} {
		if got := TypeReaches(p); got != want {
			t.Errorf("TypeReaches(%q) = %v, want %v", p, got, want)
		}
	}
}

func TestSubjectReaches(t *testing.T) {
	for g, want := range map[string]bool{
		"*":                     false, // matches all, like no subjects: the types fence it
		"f*":                    true,
		"fleet/*":               true,
		"fleet/client/game/t1":  true,
		"game/x":                true,
		"?ame/x":                false,
		"github/*":              false,
		"github/MohammadBnei/*": false,
		"alert/abc":             false,
		"fleetx/a":              false,
		"gam":                   false, // no wildcard: matches only "gam"
	} {
		if got := SubjectReaches(g); got != want {
			t.Errorf("SubjectReaches(%q) = %v, want %v", g, got, want)
		}
	}
}

func TestReserved(t *testing.T) {
	if !Reserved("dev.bnei.fleet.session.waiting", "x/1") || !Reserved("dev.bnei.agent.handoff", "fleet/client/game/t") ||
		Reserved("dev.bnei.agent.handoff", "agent/x") {
		t.Fatal("Reserved")
	}
}
