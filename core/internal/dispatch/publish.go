package dispatch

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"strings"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"google.golang.org/protobuf/types/known/timestamppb"

	"github.com/MohammadBnei/agent-fleet/core/internal/events"
	bridgev1 "github.com/MohammadBnei/ukubi-bridge/proto/gen/go/bridge/v1"
)

const (
	// AgentTypePrefix is the root PublishEvent publishes under, and one of
	// the roots the dispatcher reads.
	AgentTypePrefix = "dev.bnei.agent."
	// MaxDepth is PublishEvent's chain bound, maxPromptDepth's (docs/adr/0041).
	MaxDepth = 3
	// maxPublishText bounds what an agent hands on.
	maxPublishText = 16 << 10
)

// ErrPublishRefused is events.ErrPublishRefused: a request PublishEvent will
// never take (its type, subject, size or depth).
var ErrPublishRefused = events.ErrPublishRefused

// Publisher is the bridge's Publish (bridgeclient.Client).
type Publisher interface {
	Publish(ctx context.Context, e *bridgev1.CloudEvent) error
}

// Publish is PublishEvent (docs/adr/0078 §8). The caller has checked that the
// session is a definition session granting publish_event; this sets what the
// agent cannot: the actor, the causation (the bridge event behind the
// session's latest waking entry), hops (one more than that event's) and
// trust (that event's, and false once the session is tainted, or when no
// event caused it).
type PublishResult struct {
	ID      string
	Trusted bool
	Hops    int32
}

func Publish(ctx context.Context, pool *pgxpool.Pool, pub Publisher, sessionID, definition, typ, subject, text, key string) (PublishResult, error) {
	switch {
	case !strings.HasPrefix(typ, AgentTypePrefix) || len(typ) == len(AgentTypePrefix) || len(typ) > 200:
		return PublishResult{}, fmt.Errorf("%w: type must be under %s", ErrPublishRefused, AgentTypePrefix)
	case events.Reserved(typ, subject):
		return PublishResult{}, fmt.Errorf("%w: %s on %s is a human-facing type or subject", ErrPublishRefused, typ, subject)
	case subject == "" || len(subject) > 512:
		return PublishResult{}, fmt.Errorf("%w: subject is required and at most 512 bytes", ErrPublishRefused)
	case text == "" || len(text) > maxPublishText:
		return PublishResult{}, fmt.Errorf("%w: text is required and at most %d bytes", ErrPublishRefused, maxPublishText)
	case len(key) > 200:
		return PublishResult{}, fmt.Errorf("%w: idempotency_key is at most 200 bytes", ErrPublishRefused)
	}

	var causation string
	err := pool.QueryRow(ctx, `
		SELECT causation_id FROM transcript
		 WHERE session_id = $1 AND NOT context_only AND coalesce(causation_id, '') <> ''
		 ORDER BY seq DESC LIMIT 1`, sessionID).Scan(&causation)
	if err != nil && !errors.Is(err, pgx.ErrNoRows) {
		return PublishResult{}, err
	}
	hops, trusted := int32(1), false
	if causation != "" {
		var causeHops int32
		var causeTainted bool
		err := pool.QueryRow(ctx, `
			SELECT hops, tainted FROM dispatch_decisions
			 WHERE source || ':' || event_id = $1 AND definition = $2
			 ORDER BY decided_at DESC LIMIT 1`, causation, definition).Scan(&causeHops, &causeTainted)
		switch {
		case errors.Is(err, pgx.ErrNoRows):
			// The cause's decision is pruned or was never this definition's:
			// nothing vouches for it.
		case err != nil:
			return PublishResult{}, err
		default:
			hops, trusted = causeHops+1, !causeTainted
		}
	}
	if hops > MaxDepth {
		return PublishResult{}, fmt.Errorf("%w: this event would be %d hops from a person, above %d", ErrPublishRefused, hops, MaxDepth)
	}
	if trusted {
		var tainted bool
		if err := pool.QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM transcript WHERE session_id = $1 AND (context_only OR untrusted))`,
			sessionID).Scan(&tainted); err != nil {
			return PublishResult{}, err
		}
		trusted = !tainted
	}

	id := uuid.NewString()
	if key != "" {
		sum := sha256.Sum256([]byte(sessionID + "\x00" + key))
		id = hex.EncodeToString(sum[:16])
	}
	data, _ := json.Marshal(map[string]string{"text": text, "definition": definition})
	err = pub.Publish(ctx, &bridgev1.CloudEvent{
		Id: id, Type: typ, Subject: subject, Time: timestamppb.Now(), Data: data,
		Actor: "agent", ActorLogin: "agent:" + definition, Trusted: trusted,
		Hops: hops, CausationId: causation,
	})
	if err != nil {
		return PublishResult{}, err
	}
	return PublishResult{ID: id, Trusted: trusted, Hops: hops}, nil
}
