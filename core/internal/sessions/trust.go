package sessions

import (
	"errors"
	"strings"
)

// Tier is the trust of what triggered a session (docs/adr/0086 §2, matching docs/adr/0082 §2).
type Tier int

const (
	TierOwner     Tier = iota // T0: the owner, an owner surface, an owner-created schedule
	TierFleet                 // T1: a fleet event or Invoke whose cause and publisher lack A
	TierUntrusted             // an alert, a T2/T3 subject, or an event/Invoke from a session holding A
)

// ErrNoUntrusted: an untrusted trigger needs the definition's trust.untrusted letter (B or C); without one core
// files a card instead of creating the session.
var ErrNoUntrusted = errors.New("definition has no trust.untrusted letter")

// TrustSetFor is the set a session gets at create (docs/adr/0086 §2). untrusted is the definition's
// trust.untrusted, read only for TierUntrusted.
//
// ponytail: no definition ceiling yet (ADR-0062's trust block lands with the definition slice); T0 and T1 get BC.
func TrustSetFor(t Tier, untrusted string) (string, error) {
	if t != TierUntrusted {
		return "BC", nil
	}
	if untrusted != "B" && untrusted != "C" {
		return "", ErrNoUntrusted
	}
	return "A" + untrusted, nil
}

// HoldsTrust reports whether set holds letter.
func HoldsTrust(set, letter string) bool { return strings.Contains(set, letter) }
