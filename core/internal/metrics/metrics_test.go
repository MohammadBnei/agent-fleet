package metrics

import (
	"errors"
	"testing"

	"connectrpc.com/connect"
	"github.com/prometheus/client_golang/prometheus/testutil"
)

// A (repo, status) pair that empties out must stop reporting, not freeze at
// its last non-zero value — otherwise "3 tasks pending" stays on the
// dashboard forever after they all complete, because a gauge only ever
// hears about label combinations that still exist.
func TestSetTasksCurrentClearsVanishedPairs(t *testing.T) {
	SetTasksCurrent(map[[2]string]int{
		{"agent-fleet", "pending"}: 3,
		{"vos-monolith", "done"}:   1,
	})
	if got := testutil.ToFloat64(TasksCurrent.WithLabelValues("agent-fleet", "pending")); got != 3 {
		t.Fatalf("pending = %v, want 3", got)
	}

	// Second snapshot: the pending tasks are gone entirely.
	SetTasksCurrent(map[[2]string]int{
		{"vos-monolith", "done"}: 1,
	})
	if n := testutil.CollectAndCount(TasksCurrent); n != 1 {
		t.Fatalf("series after refresh = %d, want 1 (the stale pending pair was kept)", n)
	}
}

// connect.CodeOf(nil) is CodeUnknown, not "no error". Using it unguarded
// labels every successful RPC as an error code and makes the dashboard's
// error rate read 100%.
func TestConnectCode(t *testing.T) {
	if got := connectCode(nil); got != "ok" {
		t.Errorf("connectCode(nil) = %q, want %q", got, "ok")
	}
	err := connect.NewError(connect.CodeInvalidArgument, errors.New("bad"))
	if got := connectCode(err); got != "invalid_argument" {
		t.Errorf("connectCode(invalid arg) = %q, want %q", got, "invalid_argument")
	}
}
