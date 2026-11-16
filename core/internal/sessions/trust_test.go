package sessions

import (
	"errors"
	"testing"
)

// docs/adr/0086 §2's tier table.
func TestTrustSetFor(t *testing.T) {
	for _, tc := range []struct {
		tier      Tier
		untrusted string
		want      string
		err       error
	}{
		{TierOwner, "", "BC", nil},
		{TierFleet, "", "BC", nil},
		{TierOwner, "B", "BC", nil}, // the letter is read only for untrusted triggers
		{TierUntrusted, "B", "AB", nil},
		{TierUntrusted, "C", "AC", nil},
		{TierUntrusted, "", "", ErrNoUntrusted}, // no letter: core files a card instead
		{TierUntrusted, "BC", "", ErrNoUntrusted},
	} {
		got, err := TrustSetFor(tc.tier, tc.untrusted)
		if got != tc.want || !errors.Is(err, tc.err) {
			t.Errorf("TrustSetFor(%d, %q) = %q, %v; want %q, %v", tc.tier, tc.untrusted, got, err, tc.want, tc.err)
		}
	}
	// An untrusted set never holds all three letters unsupervised.
	for _, l := range []string{"B", "C"} {
		if s, _ := TrustSetFor(TierUntrusted, l); HoldsTrust(s, "A") && HoldsTrust(s, "B") && HoldsTrust(s, "C") {
			t.Errorf("untrusted %s holds ABC", l)
		}
	}
}
