package dispatch

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"regexp"
	"strings"
	"time"

	"connectrpc.com/connect"
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

// Tunables; tests shrink them.
var (
	// MaxPublishes bounds how many events one session publishes per
	// PublishWindow: depth bounds a chain, this bounds its width.
	MaxPublishes  = 10
	PublishWindow = time.Hour
)

var (
	// A type and a subject end up as lines of the receiver's message and as a
	// thread key, so neither may carry whitespace or control characters
	// (they could forge an envelope line). Subjects are threads the bridge
	// lets bridge-core publish on and the dispatcher routes: github/ and
	// agent/.
	typeRe    = regexp.MustCompile(`^dev\.bnei\.agent\.[a-z0-9][a-z0-9._-]*$`)
	subjectRe = regexp.MustCompile(`^(github|agent)/[A-Za-z0-9._/@:+-]+$`)
)

// ErrPublishRefused is events.ErrPublishRefused: a request PublishEvent will
// never take (its type, subject, size, depth, budget or a reused key).
var ErrPublishRefused = events.ErrPublishRefused

// Publisher is the bridge's Publish (bridgeclient.Client).
type Publisher interface {
	Publish(ctx context.Context, e *bridgev1.CloudEvent) error
}

type PublishResult struct {
	ID      string
	Trusted bool
	Hops    int32
}

// Publish is PublishEvent (docs/adr/0078 §8). The caller has checked that the
// session is a definition session granting publish_event; this sets what the
// agent cannot: the actor, the causation (the bridge event behind the
// session's latest waking entry written by the dispatcher), hops (one more
// than that event's) and trust (that event's, and false once the session is
// tainted, or when no bridge event caused it).
func Publish(ctx context.Context, pool *pgxpool.Pool, pub Publisher, sessionID, definition, typ, subject, text, key string) (PublishResult, error) {
	switch {
	case len(typ) > 200 || !typeRe.MatchString(typ):
		return PublishResult{}, fmt.Errorf("%w: type must be %s followed by lowercase letters, digits, '.', '_' or '-'", ErrPublishRefused, AgentTypePrefix)
	case len(subject) > 512 || !subjectRe.MatchString(subject):
		return PublishResult{}, fmt.Errorf("%w: subject must be a github/ or agent/ thread of at most 512 bytes, without spaces", ErrPublishRefused)
	case events.Reserved(typ, subject):
		return PublishResult{}, fmt.Errorf("%w: %s on %s is a human-facing type or subject", ErrPublishRefused, typ, subject)
	case text == "" || len(text) > maxPublishText:
		return PublishResult{}, fmt.Errorf("%w: text is required and at most %d bytes", ErrPublishRefused, maxPublishText)
	case len(key) > 200:
		return PublishResult{}, fmt.Errorf("%w: idempotency_key is at most 200 bytes", ErrPublishRefused)
	}

	// Only an entry the dispatcher wrote (as client bridge, also for an
	// admin-opened proposal) names a bridge event: any other client may put
	// what it likes in causation_id.
	var causation string
	err := pool.QueryRow(ctx, `
		SELECT causation_id FROM transcript
		 WHERE session_id = $1 AND NOT context_only AND author_sub = $2 AND coalesce(causation_id, '') <> ''
		 ORDER BY seq DESC LIMIT 1`, sessionID, "client:"+Client).Scan(&causation)
	if err != nil && !errors.Is(err, pgx.ErrNoRows) {
		return PublishResult{}, err
	}
	hops, trusted := int32(1), false
	// Causation is <source>:<id>; no source the bridge sets holds a ':'.
	if source, id, ok := strings.Cut(causation, ":"); ok {
		var causeHops int32
		var causeTainted bool
		err := pool.QueryRow(ctx, `
			SELECT hops, tainted FROM dispatch_decisions WHERE source = $1 AND event_id = $2 AND definition = $3`,
			source, id, definition).Scan(&causeHops, &causeTainted)
		switch {
		case errors.Is(err, pgx.ErrNoRows):
			// Pruned, or never this definition's: nothing vouches for it.
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

	// The request's identity, and the budget. A key names one request: the
	// same key with another type, subject or text is refused, never dropped
	// by the bridge's dedup on (source, id) while reporting success.
	reqSum := sha256.Sum256([]byte(typ + "\x00" + subject + "\x00" + text))
	reqSHA := hex.EncodeToString(reqSum[:])
	id := uuid.NewString()
	if key != "" {
		sum := sha256.Sum256([]byte(sessionID + "\x00" + key))
		id = hex.EncodeToString(sum[:16])
	}
	var storedSHA string
	err = pool.QueryRow(ctx, `SELECT request_sha FROM published_events WHERE session_id = $1 AND event_id = $2`, sessionID, id).Scan(&storedSHA)
	switch {
	case err == nil:
		if storedSHA != reqSHA {
			return PublishResult{}, fmt.Errorf("%w: this idempotency_key was used for a different event", ErrPublishRefused)
		}
		// A retry: published again, the bridge dedups it.
	case errors.Is(err, pgx.ErrNoRows):
		var recent int
		if err := pool.QueryRow(ctx, `SELECT count(*) FROM published_events WHERE session_id = $1 AND published_at > now() - $2::interval`,
			sessionID, PublishWindow.String()).Scan(&recent); err != nil {
			return PublishResult{}, err
		}
		if recent >= MaxPublishes {
			return PublishResult{}, fmt.Errorf("%w: this session published %d events in the last %s, its budget", ErrPublishRefused, recent, PublishWindow)
		}
		if _, err := pool.Exec(ctx, `INSERT INTO published_events (session_id, event_id, request_sha) VALUES ($1, $2, $3) ON CONFLICT DO NOTHING`,
			sessionID, id, reqSHA); err != nil {
			return PublishResult{}, err
		}
	default:
		return PublishResult{}, err
	}

	data, _ := json.Marshal(map[string]string{"text": text, "definition": definition})
	err = pub.Publish(ctx, &bridgev1.CloudEvent{
		Id: id, Type: typ, Subject: subject, Time: timestamppb.Now(), Data: data,
		Actor: "agent", ActorLogin: "agent:" + definition, Trusted: trusted,
		Hops: hops, CausationId: causation,
	})
	if err != nil {
		if refused(err) {
			// The bridge will never take it (its client table, its shape).
			return PublishResult{}, fmt.Errorf("%w: the bridge refused it: %v", ErrPublishRefused, err)
		}
		if key == "" {
			// Unkeyed, a retry is a new event: this one costs no budget.
			_, _ = pool.Exec(context.WithoutCancel(ctx), `DELETE FROM published_events WHERE session_id = $1 AND event_id = $2`, sessionID, id)
		}
		return PublishResult{}, err
	}
	return PublishResult{ID: id, Trusted: trusted, Hops: hops}, nil
}

func refused(err error) bool {
	switch connect.CodeOf(err) {
	case connect.CodeInvalidArgument, connect.CodePermissionDenied, connect.CodeFailedPrecondition:
		return true
	}
	return false
}
