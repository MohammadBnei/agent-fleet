package schedules

import (
	"errors"
	"fmt"
	"time"

	// The core image is distroless/static-debian12, which ships no zoneinfo.
	// Without this, `CRON_TZ=Europe/Paris 0 9 * * MON` resolves to UTC — it
	// does not error, it just fires at the wrong hour half the year. The
	// import belongs in this package rather than in cmd/core, because this is
	// where LoadLocation is actually reached: CI runners have system zoneinfo,
	// so a test here can only fail if the import is missing from the package
	// under test.
	_ "time/tzdata"

	"github.com/robfig/cron/v3"
)

// ErrNoOccurrence is returned for a cron expression that parses but can never
// fire — `0 0 30 2 *` is the readable version, a mistyped leap-day rule the
// realistic one. robfig searches five years ahead and then returns the zero
// time, so without this the schedule would be written with next_run_at
// 0001-01-01, be due on every single tick, and never stop.
var ErrNoOccurrence = errors.New("cron expression has no occurrence in the next five years")

// nextRun returns when a schedule should fire after `now`, and whether it is
// spent (has no further runs and should be disabled).
//
// `now` must be POSTGRES's now(), not time.Now(): the due-check runs in SQL
// and this runs in Go, so anchoring on the local clock lets skew between core
// and the database produce a "next" time that is already past — the row is
// then due again on the very next tick, forever. Two core replicas would also
// disagree about the same schedule.
func nextRun(s Schedule, now time.Time) (time.Time, bool, error) {
	switch {
	case s.Cron != "":
		sched, err := cron.ParseStandard(s.Cron)
		if err != nil {
			return time.Time{}, false, fmt.Errorf("parse cron %q: %w", s.Cron, err)
		}
		next := sched.Next(now)
		if next.IsZero() {
			return time.Time{}, false, ErrNoOccurrence
		}
		return next, false, nil

	case s.IntervalSeconds != nil:
		return now.Add(time.Duration(*s.IntervalSeconds) * time.Second), false, nil

	default:
		// One-shot: it just fired, so there is no next.
		return now, true, nil
	}
}

// ValidateCron is the trust boundary. Called from the dashboard handlers so
// the human who typed the expression gets the error, rather than a cadence
// that quietly never fires and a last_status nobody reads.
func ValidateCron(expr string, now time.Time) error {
	if expr == "" {
		return nil
	}
	_, _, err := nextRun(Schedule{Cron: expr}, now)
	return err
}
