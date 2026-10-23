// Package dispatch is core's consumer of the event bridge (docs/adr/0071 §3-6
// as amended by docs/adr/0078 §5): one subscription, read one event at a
// time, each handled synchronously and acked, or nacked so the bridge retries
// and in the end dead-letters it. Core holds no queue (docs/adr/0048 §2).
//
// An event goes to the definitions whose owns or subscribes match it and that
// the bridge client may open, through AgentService.Open as that client. A
// firing alert goes to the alert handler instead, which dedups it against the
// direct webhook (docs/adr/0078 §6).
package dispatch

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"slices"
	"strings"
	"time"

	"connectrpc.com/connect"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/MohammadBnei/agent-fleet/core/internal/agentsvc"
	"github.com/MohammadBnei/agent-fleet/core/internal/alertwebhook"
	"github.com/MohammadBnei/agent-fleet/core/internal/bridgeclient"
	"github.com/MohammadBnei/agent-fleet/core/internal/definitions"
	"github.com/MohammadBnei/agent-fleet/core/internal/events"
	"github.com/MohammadBnei/agent-fleet/core/internal/proposals"
	agentfleetv1 "github.com/MohammadBnei/agent-fleet/proto/gen/go/agentfleet/v1"
	bridgev1 "github.com/MohammadBnei/ukubi-bridge/proto/gen/go/bridge/v1"
)

const (
	// Client is the AgentClient the dispatcher opens threads as
	// (definitions/clients/bridge.yaml): in-process, no tokenRef.
	Client = "bridge"
	// Subscription is core's subscription name on the bridge.
	Subscription = "bridge-core/dispatch"

	typeAlertFiring   = "dev.bnei.alert.firing"
	typeAlertResolved = "dev.bnei.alert.resolved"
	typePRClosed      = "dev.bnei.github.pull_request.closed"
)

// Types core subscribes to. Never a reserved type (infra-bootstrap ADR-0066
// §3): the bridge refuses one, and TestTypesAvoidReserved checks it here.
var Types = []string{"dev.bnei.github.", "dev.bnei.alert.", "dev.bnei.agent."}

// Tunables; tests shrink them.
var (
	HandleTimeout = 2 * time.Minute
	BackoffMin    = 30 * time.Second
	BackoffMax    = 5 * time.Minute
	// HealthyAfter: a stream that lasted this long resets the backoff, and
	// its end (the token's expiry, most often) reconnects at once.
	HealthyAfter = 30 * time.Second
	// KeepDecisions outlives the bridge's 30-day retention, after which no
	// redelivery can need a decision.
	KeepDecisions = 31 * 24 * time.Hour
)

// Bridge is bridgeclient.Client; tests fake it.
type Bridge interface {
	Subscribe(ctx context.Context, r *bridgev1.SubscribeRequest) (bridgeclient.Stream, error)
	Ack(ctx context.Context, subscription string, seq int64) error
	Nack(ctx context.Context, subscription string, seq int64, reason string) error
	ConfirmGap(ctx context.Context, subscription string, version int64) error
}

// Agents is agentsvc.Server.
type Agents interface {
	AsClient(ctx context.Context, client string) (context.Context, error)
	Open(ctx context.Context, req *connect.Request[agentfleetv1.OpenRequest]) (*connect.Response[agentfleetv1.OpenResponse], error)
	CloseThread(ctx context.Context, req *connect.Request[agentfleetv1.CloseThreadRequest]) (*connect.Response[agentfleetv1.CloseThreadResponse], error)
	OpenAsAdmin(ctx context.Context, client, adminSub string, m *agentfleetv1.OpenRequest) (*agentfleetv1.OpenResponse, error)
}

// Definitions is definitions.Registry.
type Definitions interface {
	List() ([]definitions.Head, []string)
	MayOpen(client, definition string) (definitions.Head, bool)
	Clients() []definitions.Client
}

// Proposals is proposals.Store.
type Proposals interface {
	CreateEvent(ctx context.Context, repo, dedupKey, title, body, payloadJSON string, ev proposals.Event) (string, bool, error)
	FoldEvent(ctx context.Context, repo, dedupKey, marker, text string, tainted bool) (bool, error)
	DismissUnopened(ctx context.Context, repo, dedupKey string) (bool, error)
}

// Alerts is alertwebhook.Handler.
type Alerts interface {
	File(ctx context.Context, raw json.RawMessage) (bool, error)
	Resolve(ctx context.Context, raw json.RawMessage) error
}

type Dispatcher struct {
	pool   *pgxpool.Pool
	bridge Bridge
	agents Agents
	defs   Definitions
	props  Proposals
	alerts Alerts // nil: alerts are acked and dropped (no alert repo configured)

	lastNote string // the last "not configured" reason logged
}

func New(pool *pgxpool.Pool, bridge Bridge, agents Agents, defs Definitions, props Proposals, alerts Alerts) *Dispatcher {
	return &Dispatcher{pool: pool, bridge: bridge, agents: agents, defs: defs, props: props, alerts: alerts}
}

// Run holds the subscription until ctx ends. Not being configured (no
// password file) is one Info line per reason and a retry every BackoffMax; a
// failed stream is a WARN with backoff, never an error loop.
func (d *Dispatcher) Run(ctx context.Context) {
	go d.prune(ctx)
	backoff := time.Duration(0)
	for ctx.Err() == nil {
		start := time.Now()
		err := d.stream(ctx)
		switch {
		case ctx.Err() != nil:
			return
		case errors.Is(err, bridgeclient.ErrNotConfigured), errors.Is(err, errNotReady):
			if note := err.Error(); note != d.lastNote {
				slog.Info("dispatch: no bridge subscription", "reason", note)
				d.lastNote = note
			}
			backoff = BackoffMax
		case time.Since(start) >= HealthyAfter:
			d.lastNote = ""
			backoff = 0
			if connect.CodeOf(err) != connect.CodeUnauthenticated {
				slog.Warn("dispatch: bridge stream ended, reconnecting", "error", err)
			}
		default:
			d.lastNote = ""
			backoff = min(max(2*backoff, BackoffMin), BackoffMax)
			slog.Warn("dispatch: bridge stream failed, backing off", "retryIn", backoff, "error", err)
		}
		if backoff > 0 {
			select {
			case <-ctx.Done():
				return
			case <-time.After(backoff):
			}
		}
	}
}

// errNotReady: the registry has not synced (or is stale), or no client
// bridge is registered. Routing would find no owner and ack every event
// unhandled, so nothing is read until it is ready.
var errNotReady = errors.New("the definitions registry has no bridge client yet")

func (d *Dispatcher) ready() bool {
	return slices.ContainsFunc(d.defs.Clients(), func(c definitions.Client) bool { return c.Name == Client })
}

// stream is one Subscribe call, read to its end.
func (d *Dispatcher) stream(ctx context.Context) error {
	if !d.ready() {
		return errNotReady
	}
	st, err := d.bridge.Subscribe(ctx, &bridgev1.SubscribeRequest{Name: Subscription, Types: Types})
	if err != nil {
		return err
	}
	defer func() { _ = st.Close() }()
	for st.Receive() {
		m := st.Msg()
		if g := m.GetGap(); g != nil {
			d.gap(ctx, g)
			continue
		}
		e := m.GetEvent()
		if e == nil {
			continue
		}
		if !d.ready() {
			return errNotReady // unsettled: redelivered on the next stream
		}
		hctx, cancel := context.WithTimeout(ctx, HandleTimeout)
		herr := d.Handle(hctx, e)
		cancel()
		if herr != nil {
			slog.Warn("dispatch: event not handled, nacking", "seq", e.GetSeq(), "type", e.GetType(), "subject", e.GetSubject(), "error", herr)
			err = d.bridge.Nack(ctx, Subscription, e.GetSeq(), truncate(herr.Error(), 500))
		} else {
			err = d.bridge.Ack(ctx, Subscription, e.GetSeq())
		}
		if err != nil {
			// Unsettled, the event is redelivered on the next stream, and
			// the stored decisions make that a no-op where it was handled.
			return fmt.Errorf("settle seq %d: %w", e.GetSeq(), err)
		}
	}
	return st.Err()
}

// gap: retention deleted events this subscription never acked, so core missed
// them (it was down for 30 days). Core has no GitHub client to resync from,
// so it names what may be stale and confirms (docs/adr/0078 §5): a thread
// whose closing event was lost stays open until someone closes it.
func (d *Dispatcher) gap(ctx context.Context, g *bridgev1.Gap) {
	var open int
	if err := d.pool.QueryRow(ctx, `SELECT count(*) FROM sessions WHERE invoker = $1 AND archived_at IS NULL`, Client).Scan(&open); err != nil {
		slog.Warn("dispatch: count event threads after a gap", "error", err)
	}
	slog.Warn("dispatch: the bridge deleted events core never handled; open event threads may be stale",
		"fromSeq", g.GetFromSeq(), "toSeq", g.GetToSeq(), "openThreads", open)
	if err := d.bridge.ConfirmGap(ctx, Subscription, g.GetVersion()); err != nil {
		// Sent again on the next stream, which confirms it then.
		slog.Warn("dispatch: confirm gap", "version", g.GetVersion(), "error", err)
	}
}

func (d *Dispatcher) prune(ctx context.Context) {
	t := time.NewTicker(time.Hour)
	defer t.Stop()
	for {
		if _, err := d.pool.Exec(ctx, `DELETE FROM dispatch_decisions WHERE decided_at < now() - $1::interval`, KeepDecisions.String()); err != nil && ctx.Err() == nil {
			slog.Warn("dispatch: prune decisions", "error", err)
		}
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
	}
}

// Handle is one event. nil means ack: handled, or nothing to do. An error
// means nack: the bridge retries it and, after its attempts, dead-letters it
// where the operator sees it.
func (d *Dispatcher) Handle(ctx context.Context, e *bridgev1.CloudEvent) error {
	switch e.GetType() {
	case typeAlertFiring, typeAlertResolved:
		return d.alert(ctx, e)
	}
	if strings.HasPrefix(e.GetType(), "dev.bnei.alert.") {
		return nil
	}
	targets := d.route(e)
	if len(targets) == 0 {
		slog.Debug("dispatch: no definition owns or subscribes to this event", "type", e.GetType(), "subject", e.GetSubject())
		return nil
	}
	var errs []error
	for _, h := range targets {
		if err := d.deliver(ctx, e, h); err != nil {
			errs = append(errs, fmt.Errorf("%s: %w", h.Name, err))
		}
	}
	return errors.Join(errs...)
}

// alert: a firing alert files a proposal, deduped against the direct webhook
// on its fingerprint; a resolved one dismisses it if nobody opened it. An
// alert never opens a thread (it is machine), so no definition sees it.
func (d *Dispatcher) alert(ctx context.Context, e *bridgev1.CloudEvent) error {
	if d.alerts == nil || e.GetReplayed() {
		return nil
	}
	var err error
	if e.GetType() == typeAlertFiring {
		_, err = d.alerts.File(ctx, e.GetData())
	} else {
		err = d.alerts.Resolve(ctx, e.GetData())
	}
	if errors.Is(err, alertwebhook.ErrSkipped) {
		return nil
	}
	return err
}

// route is every definition that owns or subscribes to e and that the bridge
// client may open (two-sided: the client lists it, it lists the client), and
// whose interaction and lifecycle Open's only shape fits.
func (d *Dispatcher) route(e *bridgev1.CloudEvent) []definitions.Head {
	heads, _ := d.defs.List()
	var out []definitions.Head
	for _, h := range heads {
		if h.SHA == "" || !(owns(h.Effective.Owns, e) || subscribes(h.Effective.Subscribes, e)) {
			continue
		}
		cur, ok := d.defs.MayOpen(Client, h.Name)
		if !ok || !allows(cur.Effective.Interaction, "ask") || !allows(cur.Effective.Lifecycle, "resident") {
			continue
		}
		out = append(out, cur)
	}
	return out
}

func allows(l *definitions.AllowList, v string) bool { return l == nil || slices.Contains(l.Allow, v) }

// owns: every list the definition fills must match (repos, subject globs,
// type prefixes), and at least one is filled. No event names a component
// yet, so an owns listing components matches nothing.
func owns(o *definitions.Owns, e *bridgev1.CloudEvent) bool {
	if o == nil || len(o.Components) > 0 || (len(o.Repos) == 0 && len(o.Subjects) == 0 && len(o.Types) == 0) {
		return false
	}
	if len(o.Repos) > 0 {
		full, name := repoOf(e.GetSubject())
		if name == "" || !slices.ContainsFunc(o.Repos, func(r string) bool { return strings.EqualFold(r, name) || strings.EqualFold(r, full) }) {
			return false
		}
	}
	return events.SubjectMatches(o.Subjects, e.GetSubject()) && events.TypeMatches(o.Types, e.GetType())
}

// subscribes is the bridge's own filter; the loader requires its types.
func subscribes(s *definitions.Subscribes, e *bridgev1.CloudEvent) bool {
	return s != nil && len(s.Types) > 0 && events.TypeMatches(s.Types, e.GetType()) && events.SubjectMatches(s.Subjects, e.GetSubject())
}

// repoOf reads a GitHub subject, github/<owner>/<repo>/..., as owner/repo
// and repo.
func repoOf(subject string) (full, name string) {
	parts := strings.SplitN(subject, "/", 4)
	if len(parts) < 3 || parts[0] != "github" || parts[1] == "" || parts[2] == "" {
		return "", ""
	}
	return parts[1] + "/" + parts[2], parts[2]
}

type decision struct {
	wake, tainted bool
	outcome       *string
}

// decide returns the stored gate decision for (event, definition), computing
// and storing it the first time: a redelivery reuses it, since its inputs
// (the client's actors, whether the thread waits on a person) move on.
func (d *Dispatcher) decide(ctx context.Context, e *bridgev1.CloudEvent, def string) (decision, error) {
	var dec decision
	err := d.pool.QueryRow(ctx, `SELECT wake, tainted, outcome FROM dispatch_decisions WHERE source = $1 AND event_id = $2 AND definition = $3`,
		e.GetSource(), e.GetId(), def).Scan(&dec.wake, &dec.tainted, &dec.outcome)
	if err == nil {
		return dec, nil
	}
	if !errors.Is(err, pgx.ErrNoRows) {
		return dec, err
	}
	wake, err := d.gate(ctx, e, def)
	if err != nil {
		return dec, err
	}
	err = d.pool.QueryRow(ctx, `
		INSERT INTO dispatch_decisions (source, event_id, definition, wake, tainted) VALUES ($1, $2, $3, $4, $5)
		ON CONFLICT (source, event_id, definition) DO UPDATE SET source = dispatch_decisions.source
		RETURNING wake, tainted, outcome`, e.GetSource(), e.GetId(), def, wake, !e.GetTrusted()).Scan(&dec.wake, &dec.tainted, &dec.outcome)
	return dec, err
}

// gate is docs/adr/0071 §6: only a trusted person (or a trusted hand-off
// through PublishEvent) the bridge client may claim wakes, never a replay,
// and never a thread that waits on a person, whose follow-ups are context.
func (d *Dispatcher) gate(ctx context.Context, e *bridgev1.CloudEvent, def string) (bool, error) {
	actor := e.GetActor() == "human" || (e.GetActor() == "agent" && strings.HasPrefix(e.GetActorLogin(), "agent:"))
	if !e.GetTrusted() || e.GetReplayed() || !actor || !d.mayClaim(e.GetActorLogin()) {
		return false, nil
	}
	var blocked bool
	err := d.pool.QueryRow(ctx, `
		SELECT EXISTS (
		  SELECT 1 FROM sessions s JOIN transcript q ON q.session_id = s.id
		   WHERE s.invoker = $1 AND s.definition_name = $2 AND s.thread_key = $3 AND s.archived_at IS NULL
		     AND q.type IN ('question', 'permission_request')
		     AND NOT EXISTS (SELECT 1 FROM transcript r WHERE r.session_id = q.session_id AND r.reply_to_seq = q.seq))`,
		Client, def, e.GetSubject()).Scan(&blocked)
	return !blocked, err
}

func (d *Dispatcher) mayClaim(login string) bool {
	for _, c := range d.defs.Clients() {
		if c.Name == Client {
			return login != "" && slices.Contains(c.Spec.Actors.Allow, login)
		}
	}
	return false
}

func (d *Dispatcher) finish(ctx context.Context, e *bridgev1.CloudEvent, def, outcome string) error {
	_, err := d.pool.Exec(ctx, `UPDATE dispatch_decisions SET outcome = $4 WHERE source = $1 AND event_id = $2 AND definition = $3`,
		e.GetSource(), e.GetId(), def, outcome)
	return err
}

// deliver is one event to one definition.
func (d *Dispatcher) deliver(ctx context.Context, e *bridgev1.CloudEvent, h definitions.Head) error {
	dec, err := d.decide(ctx, e, h.Name)
	if err != nil {
		return err
	}
	if dec.outcome != nil {
		return nil // handled by an earlier delivery
	}
	actx, err := d.agents.AsClient(ctx, Client)
	if err != nil {
		return err
	}
	if e.GetType() == typePRClosed && !e.GetReplayed() {
		return d.close(ctx, actx, e, h)
	}
	_, err = d.agents.Open(actx, connect.NewRequest(&agentfleetv1.OpenRequest{
		Definition: h.Name, ThreadKey: e.GetSubject(), Message: Render(e),
		Wake: dec.wake, Tainted: dec.tainted,
		Interaction: "ask", Lifecycle: "resident", Visibility: "shared",
		IdempotencyKey: e.GetSource() + ":" + e.GetId() + ":" + h.Name,
		Actor:          e.GetActorLogin(), CausationId: Causation(e),
	}))
	switch {
	case err == nil:
		return d.finish(ctx, e, h.Name, "delivered")
	case errors.Is(err, agentsvc.ErrNeedsWake), connect.CodeOf(err) == connect.CodeResourceExhausted:
		// No thread and not waking, or no slot: a person decides. Acked
		// either way, since the bridge delivers head-of-line.
		if err := d.propose(ctx, e, h, dec.tainted); err != nil {
			return err
		}
		return d.finish(ctx, e, h.Name, "proposal")
	default:
		return err
	}
}

// close ends the PR's thread, and retires a proposal for it nobody opened.
func (d *Dispatcher) close(ctx, actx context.Context, e *bridgev1.CloudEvent, h definitions.Head) error {
	_, err := d.agents.CloseThread(actx, connect.NewRequest(&agentfleetv1.CloseThreadRequest{Definition: h.Name, ThreadKey: e.GetSubject()}))
	if err != nil && connect.CodeOf(err) != connect.CodeNotFound {
		return err
	}
	if _, err := d.props.DismissUnopened(ctx, h.Effective.Workspace.Repo, proposalKey(h.Name, e.GetSubject())); err != nil {
		return err
	}
	return d.finish(ctx, e, h.Name, "closed")
}

func proposalKey(def, thread string) string { return "event:" + def + ":" + thread }

// propose files the event as a proposal for the definition's thread, one per
// thread while it stands: a later event folds into it, so nothing is lost
// while it waits, and opening it delivers them all.
func (d *Dispatcher) propose(ctx context.Context, e *bridgev1.CloudEvent, h definitions.Head, tainted bool) error {
	repo, key, text := h.Effective.Workspace.Repo, proposalKey(h.Name, e.GetSubject()), Render(e)
	trust := "trusted"
	if tainted {
		trust = "untrusted"
	}
	title := truncate(fmt.Sprintf("%s: %s on %s (%s)", h.Name, strings.TrimPrefix(e.GetType(), "dev.bnei."), e.GetSubject(), trust), 120)
	payload, _ := json.Marshal(map[string]any{
		"seq": e.GetSeq(), "source": e.GetSource(), "id": e.GetId(), "type": e.GetType(), "subject": e.GetSubject(),
		"actor": e.GetActor(), "actor_login": e.GetActorLogin(), "trusted": e.GetTrusted(),
	})
	_, created, err := d.props.CreateEvent(ctx, repo, key, title, text, string(payload), proposals.Event{
		Definition: h.Name, ThreadKey: e.GetSubject(), CausationID: Causation(e), Tainted: tainted,
	})
	if err != nil || created {
		return err
	}
	folded, err := d.props.FoldEvent(ctx, repo, key, marker(e), text, tainted)
	if err != nil {
		return err
	}
	if !folded {
		// A proposal stands for this thread but was opened, into a session
		// that is still live and yet refused this event: retry.
		return fmt.Errorf("a proposal for %s stands and cannot take this event", e.GetSubject())
	}
	return nil
}

// Causation is the event's identity as a session's causation_id: its id is
// unique only per source.
func Causation(e *bridgev1.CloudEvent) string { return e.GetSource() + ":" + e.GetId() }

func marker(e *bridgev1.CloudEvent) string { return "[bridge event " + Causation(e) + "]" }

// maxQuoted bounds what Render quotes from an event's own text.
const maxQuoted = 16 << 10

// Render is the message an event delivers: its envelope, then what a person
// wrote in it (a PR's title and body, a comment, a review), never the whole
// webhook payload. Deterministic, since it is part of Open's request
// identity.
func Render(e *bridgev1.CloudEvent) string {
	var b strings.Builder
	trust := "trusted"
	if !e.GetTrusted() {
		trust = "untrusted"
	}
	fmt.Fprintf(&b, "%s\ntype: %s\nsubject: %s\nactor: %s (%s, %s)\n", marker(e), e.GetType(), e.GetSubject(), e.GetActorLogin(), e.GetActor(), trust)
	line := func(label, v string) { fmt.Fprintf(&b, "%s: %s\n", label, truncate(v, 500)) }
	if !strings.HasPrefix(e.GetType(), "dev.bnei.github.") {
		if len(e.GetData()) > 0 {
			b.WriteString("\n" + truncate(string(e.GetData()), maxQuoted) + "\n")
		}
		return b.String()
	}
	var gh struct {
		PullRequest *struct {
			Title   string `json:"title"`
			Body    string `json:"body"`
			HTMLURL string `json:"html_url"`
		} `json:"pull_request"`
		Issue *struct {
			Title   string `json:"title"`
			HTMLURL string `json:"html_url"`
		} `json:"issue"`
		Comment *struct {
			Body    string `json:"body"`
			HTMLURL string `json:"html_url"`
		} `json:"comment"`
		Review *struct {
			State   string `json:"state"`
			Body    string `json:"body"`
			HTMLURL string `json:"html_url"`
		} `json:"review"`
	}
	_ = json.Unmarshal(e.GetData(), &gh)
	quote := func(label, s string) {
		if s != "" {
			b.WriteString("\n" + label + ":\n" + truncate(s, maxQuoted) + "\n")
		}
	}
	// The kind, not what the payload happens to carry: a comment's payload
	// holds its issue, a review's its PR.
	kind, action, _ := strings.Cut(strings.TrimPrefix(e.GetType(), "dev.bnei.github."), ".")
	switch {
	case kind == "issue_comment" && gh.Comment != nil:
		if gh.Issue != nil {
			line("on", gh.Issue.Title)
		}
		line("url", gh.Comment.HTMLURL)
		quote("comment", gh.Comment.Body)
	case kind == "pull_request_review" && gh.Review != nil:
		if gh.PullRequest != nil {
			line("on", gh.PullRequest.Title)
		}
		line("url", gh.Review.HTMLURL)
		line("state", gh.Review.State)
		quote("review", gh.Review.Body)
	case kind == "pull_request" && gh.PullRequest != nil:
		line("title", gh.PullRequest.Title)
		line("url", gh.PullRequest.HTMLURL)
		if action == "opened" {
			quote("description", gh.PullRequest.Body)
		}
	}
	return b.String()
}

// truncate also makes s storable: Postgres text takes no NUL and only valid
// UTF-8, and a comment can decode to either, or be cut inside a rune.
func truncate(s string, n int) string {
	s = strings.ToValidUTF8(strings.ReplaceAll(s, "\x00", ""), "\uFFFD")
	if len(s) <= n {
		return s
	}
	return strings.ToValidUTF8(s[:n], "") + "…[truncated]"
}

// OpenProposal opens an event proposal as the admin who clicked it
// (docs/adr/0078 §5): the definition's thread, through the bridge client's
// Open, waking, with the proposal's taint and causation. The key carries the
// body's hash, so a retry after a failed attempt that folded more events in
// is a new request rather than a refused one.
func OpenProposal(ctx context.Context, agents Agents, p *proposals.Proposal, adminSub string) (string, error) {
	if p.Event == nil {
		return "", errors.New("not an event proposal")
	}
	sum := sha256.Sum256([]byte(p.Body))
	res, err := agents.OpenAsAdmin(ctx, Client, adminSub, &agentfleetv1.OpenRequest{
		Definition: p.Event.Definition, ThreadKey: p.Event.ThreadKey, Message: p.Body, Tainted: p.Event.Tainted,
		Interaction: "ask", Lifecycle: "resident", Visibility: "shared",
		IdempotencyKey: "proposal:" + p.ID + ":" + hex.EncodeToString(sum[:8]), CausationId: p.Event.CausationID,
	})
	if err != nil {
		return "", err
	}
	return res.GetThreadId(), nil
}
