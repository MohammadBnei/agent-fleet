package agentsvc

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"time"

	"github.com/MohammadBnei/agent-fleet/core/internal/decisions"
	"github.com/MohammadBnei/agent-fleet/core/internal/metrics"
	agentfleetv1 "github.com/MohammadBnei/agent-fleet/proto/gen/go/agentfleet/v1"
)

// ReportBudgets is docs/adr/0084 §9's budget pass over the started characters: it publishes the two gauges the
// alert reads, and for a character whose day's turns are spent with no override it files one card a day whose
// raise_budget option lifts the hold for 24 hours. A failure on one character leaves the rest.
//
// ponytail: the card files when the budget is spent, whether or not an entry waits.
func (s *Server) ReportBudgets(ctx context.Context) error {
	rows, err := s.sessions.StandingRows(ctx)
	if err != nil {
		return err
	}
	// A series is only replaced by a fresh value or dropped with its character, never cleared for a scrape to
	// miss: a gap would restart the alert's `for`.
	var errs []error
	now := map[string]bool{}
	for _, r := range rows {
		now[r.Definition] = true
		if err := s.reportBudget(ctx, r.ID, r.Definition); err != nil {
			errs = append(errs, fmt.Errorf("character %s: %w", r.Definition, err))
		}
	}
	for d := range s.budgetSeen {
		if !now[d] {
			metrics.CharacterTurnsLastHour.DeleteLabelValues(d)
			metrics.CharacterTurnsPerDay.DeleteLabelValues(d)
		}
	}
	s.budgetSeen = now
	return errors.Join(errs...)
}

// budgetZone is the budget day's zone; the card key carries its date.
var budgetZone = func() *time.Location {
	if l, err := time.LoadLocation("Europe/Paris"); err == nil {
		return l
	}
	return time.UTC
}()

func (s *Server) reportBudget(ctx context.Context, id, definition string) error {
	budget, err := s.sessions.TurnBudget(ctx, id)
	if err != nil {
		return err
	}
	today, lastHour, over, err := s.sessions.Turns(ctx, id)
	if err != nil {
		return err
	}
	metrics.CharacterTurnsLastHour.WithLabelValues(definition).Set(float64(lastHour))
	metrics.CharacterTurnsPerDay.WithLabelValues(definition).Set(float64(budget))
	if over || today < budget || s.decisions == nil {
		return nil
	}
	_, err = s.decisions.Draft(ctx, s.transcr, decisions.Draft{
		SessionID: id, From: "system", IdempotencyKey: "budget:" + id + ":" + time.Now().In(budgetZone).Format("2006-01-02"),
		Ask:     fmt.Sprintf("%s has spent its %d turns for today. Its fleet and peer messages wait; yours still go through. Lift the hold for 24 hours?", definition, budget),
		Subject: &agentfleetv1.CardSubject{Kind: "character", Name: definition},
		Options: []*agentfleetv1.CardOption{
			{Label: "Raise the budget for 24 hours", Effect: &agentfleetv1.CardEffect{Verb: decisions.VerbRaiseBudget, Payload: fmt.Sprintf(`{"character":%q}`, definition)}, Reversible: "yes"},
			{Label: "Keep the hold", Effect: &agentfleetv1.CardEffect{Verb: "handoff"}, Reversible: "yes", Recommended: true},
		},
	})
	return err
}

// RunBudgets calls ReportBudgets every interval until ctx ends.
func (s *Server) RunBudgets(ctx context.Context, every time.Duration) {
	tick := time.NewTicker(every)
	defer tick.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-tick.C:
			if err := s.ReportBudgets(ctx); err != nil {
				slog.Error("agentsvc: reporting character budgets", "error", err)
			}
		}
	}
}
