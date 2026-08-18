package schedules

import (
	"errors"
	"testing"
	"time"
)

func ptr(v int32) *int32 { return &v }

// The core image ships no zoneinfo, so a CRON_TZ prefix would silently mean
// UTC without the time/tzdata import in this package. Asserted directly
// because the CI runner HAS system zoneinfo — every other test here passes
// with or without the import, so only this one can fail when it goes missing.
func TestParisIsLoadable(t *testing.T) {
	if _, err := time.LoadLocation("Europe/Paris"); err != nil {
		t.Fatalf("Europe/Paris must resolve inside the distroless image: %v", err)
	}
}

func TestNextRunCronCrossesDST(t *testing.T) {
	paris, err := time.LoadLocation("Europe/Paris")
	if err != nil {
		t.Fatal(err)
	}
	// 2026-03-29 is when Paris jumps 02:00 -> 03:00. A 09:00 daily schedule
	// must still be 09:00 local the next morning — 23h30m on the wall clock but
	// only 22h30m of elapsed time, which is exactly what a UTC-anchored
	// "add 86400 seconds" gets wrong.
	s := Schedule{Cron: "CRON_TZ=Europe/Paris 0 9 * * *"}
	before := time.Date(2026, 3, 28, 9, 30, 0, 0, paris)

	next, spent, err := nextRun(s, before)
	if err != nil || spent {
		t.Fatalf("nextRun: %v spent=%v", err, spent)
	}
	got := next.In(paris)
	if got.Hour() != 9 || got.Day() != 29 {
		t.Fatalf("want 2026-03-29 09:00 Paris, got %s", got)
	}
	if d := next.Sub(before); d != 22*time.Hour+30*time.Minute {
		t.Fatalf("want 22h30m of real time (the hour Paris skips), got %s", d)
	}
}

func TestNextRunInterval(t *testing.T) {
	now := time.Date(2026, 8, 18, 12, 0, 0, 0, time.UTC)
	next, spent, err := nextRun(Schedule{IntervalSeconds: ptr(604800)}, now)
	if err != nil || spent {
		t.Fatalf("nextRun: %v spent=%v", err, spent)
	}
	if want := now.AddDate(0, 0, 7); !next.Equal(want) {
		t.Fatalf("want %s, got %s", want, next)
	}
}

func TestNextRunOneShotIsSpent(t *testing.T) {
	_, spent, err := nextRun(Schedule{}, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	if !spent {
		t.Fatal("a schedule with no cron and no interval must be spent after firing")
	}
}

func TestValidateCron(t *testing.T) {
	now := time.Now()
	for _, tc := range []struct {
		name, expr string
		wantErr    error
	}{
		{"empty is fine — it means interval or one-shot", "", nil},
		{"weekly", "0 9 * * MON", nil},
		{"with a timezone", "CRON_TZ=Europe/Paris 0 9 * * MON", nil},
		{"descriptor", "@weekly", nil},
		{"garbage", "every monday please", nil},
		// Parses, and then never fires: robfig gives up after five years and
		// returns the zero time. Without this rejection the row is written
		// with next_run_at 0001-01-01 and is due on every single tick forever.
		{"valid but unsatisfiable", "0 0 30 2 *", ErrNoOccurrence},
	} {
		t.Run(tc.name, func(t *testing.T) {
			err := ValidateCron(tc.expr, now)
			switch {
			case tc.wantErr != nil && !errors.Is(err, tc.wantErr):
				t.Fatalf("want %v, got %v", tc.wantErr, err)
			case tc.wantErr == nil && tc.name == "garbage" && err == nil:
				t.Fatal("garbage must be rejected at the trust boundary")
			case tc.wantErr == nil && tc.name != "garbage" && err != nil:
				t.Fatalf("unexpected error: %v", err)
			}
		})
	}
}
