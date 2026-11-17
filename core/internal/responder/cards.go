package responder

import (
	"context"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	agentfleetv1 "github.com/MohammadBnei/agent-fleet/proto/gen/go/agentfleet/v1"
)

// Cards is the typed read a standing character makes of the first responders' work (docs/adr/0084 §5): the owner's
// alert_group cards and the remediations the responders selected. Typed fields only: a card's ask is core's own
// sentence (CardRequest.Ask), a remediation's action, target and state are enums and object ids core checked. The
// agent's claim, free labels, annotation values and alert text are in neither table.
type Cards struct{ Pool *pgxpool.Pool }

// List is the rows created at or after since, newest first, at most limit.
func (c Cards) List(ctx context.Context, since time.Time, limit int) ([]*agentfleetv1.ResponderCard, error) {
	rows, err := c.Pool.Query(ctx, `
		SELECT id, kind, state, ask, action, target, responder, created_at FROM (
			SELECT d.id::text AS id, 'card' AS kind, d.state AS state, d.ask AS ask, '' AS action, '' AS target,
			       '' AS responder, d.created_at AS created_at
			  FROM decisions d WHERE d.kind = 'card' AND d.subject LIKE 'alert/%' AND d.created_at >= $1
			UNION ALL
			SELECT r.id::text, 'remediation', r.state, '', r.action, r.target, r.responder, r.created_at
			  FROM remediations r WHERE r.created_at >= $1
		) u ORDER BY created_at DESC, id DESC LIMIT $2`, since, limit)
	if err != nil {
		return nil, fmt.Errorf("list responder cards: %w", err)
	}
	defer rows.Close()
	out := []*agentfleetv1.ResponderCard{}
	for rows.Next() {
		var (
			card    agentfleetv1.ResponderCard
			created time.Time
		)
		if err := rows.Scan(&card.Id, &card.Kind, &card.State, &card.Ask, &card.Action, &card.Target, &card.Responder, &created); err != nil {
			return nil, err
		}
		card.CreatedAt = created.UTC().Format(time.RFC3339)
		out = append(out, &card)
	}
	return out, rows.Err()
}
