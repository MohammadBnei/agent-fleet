package agentsvc

import (
	"testing"

	"github.com/MohammadBnei/agent-fleet/core/internal/sessions"
)

// A single-shot run with no pod reads waiting on a question, permission or
// plan only (parkedWhere's kinds), never on an open card alone.
func TestStatus_WaitingNeedsAParkDecision(t *testing.T) {
	ss := singleShot
	card := &sessions.Session{Lifecycle: &ss, PendingDecisions: 1}
	if got := status(card).GetState(); got == "waiting" {
		t.Errorf("card only: %q, want not waiting", got)
	}
	parked := &sessions.Session{Lifecycle: &ss, PendingDecisions: 1, ParkDecisions: 1}
	if got := status(parked).GetState(); got != "waiting" {
		t.Errorf("parked: %q, want waiting", got)
	}
}
