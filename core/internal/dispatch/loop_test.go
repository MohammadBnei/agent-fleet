package dispatch

import (
	"testing"
	"time"
)

// TestLoop_NudgeIsNonBlockingAndBuffered covers reliability-findings.md
// #5's nudge mechanism: Nudge() must never block the caller (it's called
// from tasks.Store.CreateTask, inline with a DB write) and a burst of
// nudges before Run's select drains any of them must collapse into at
// most one pending tick, not pile up or block.
func TestLoop_NudgeIsNonBlockingAndBuffered(t *testing.T) {
	l := New(nil, nil, nil, nil, nil, 1, 3, 30*time.Second, 30*time.Minute)

	done := make(chan struct{})
	go func() {
		l.Nudge()
		l.Nudge()
		l.Nudge()
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("Nudge() blocked — expected a non-blocking buffered send")
	}

	select {
	case <-l.nudge:
	default:
		t.Fatal("expected a pending nudge on the channel after calling Nudge()")
	}

	// The buffer holds exactly one slot — after draining it above, a
	// second read should find nothing pending.
	select {
	case <-l.nudge:
		t.Fatal("expected only one pending nudge to be buffered, found a second")
	default:
	}
}
