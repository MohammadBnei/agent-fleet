// Package alertwebhook turns a firing Alertmanager alert into a thot task
// (docs/adr/0037).
//
// Before that ADR this needed a bespoke HTTP listener on a standing thot
// service. Now an alert is just another way a task gets created — the same
// row, the same dispatch loop, the same transcript and dashboard. The only
// thing this package adds is translation and admission control.
package alertwebhook

import (
	"context"
	"crypto/subtle"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"strings"
)

// Notifier is the Discord surface, narrowed so this package can be tested
// without a bot session. nil disables the Discord side entirely.
//
// It posts a notification, and that is all it can do. This used to open a
// thread the human could reply into; Discord is outbound-only now
// (docs/adr/0048), so the reply happens in the dashboard.
type Notifier interface {
	Notify(ctx context.Context, text string) error
}

// ProposalCreator is proposals.Store, narrowed to the one method used.
//
// A firing alert files a PROPOSAL, never a session — and that is the whole
// safety property, expressed in the schema rather than in a status value.
// A proposal row has no pod path at all, so an alert storm cannot spawn
// agents no matter how many times it fires; the worst it can do is fill a
// list a human scrolls past. The old design relied on dispatch never
// SELECTing status='proposed', which held only as long as every future query
// remembered to exclude it.
//
// That matters most here specifically: this handler's repo is
// infra-bootstrap, whose sessions carry cluster access (docs/adr/0037).
type ProposalCreator interface {
	Create(ctx context.Context, repo, source, dedupKey, title, body, payloadJSON string) (string, bool, error)
	DismissUnopened(ctx context.Context, repo, dedupKey string) (bool, error)
}

type Config struct {
	// Token is a shared secret Alertmanager sends as a bearer. Still required
	// even though this endpoint no longer creates anything runnable: an
	// unauthenticated caller inside the cluster could otherwise flood the
	// proposal list and bury a real alert. Empty disables the endpoint
	// entirely rather than serving it open.
	Token string
	// Repo a thot session runs on (its worktree, and where a durable fix
	// becomes a PR).
	Repo string
	// ChannelID is retained for config compatibility; notifications go to the
	// bot's configured channel.
	ChannelID string
}

type Handler struct {
	proposals ProposalCreator
	discord   Notifier
	cfg       Config
	inactive  func(ctx context.Context, repo string) bool
}

// SetInactive installs the repo gate: while cfg.Repo is inactive, firing
// alerts file no proposal. Nil means no gate.
func (h *Handler) SetInactive(fn func(ctx context.Context, repo string) bool) { h.inactive = fn }

func New(p ProposalCreator, discord Notifier, cfg Config) *Handler {
	return &Handler{proposals: p, discord: discord, cfg: cfg}
}

// payload is the subset of Alertmanager's webhook body this needs. Its
// schema is far larger; decoding only these fields means an Alertmanager
// upgrade adding fields can't break parsing.
//
// The alerts stay as raw JSON alongside the decoded form, because decoding a
// subset is exactly what used to lose the alert: describe() below flattens
// three fields and four labels into text, and everything else — the other
// labels, startsAt, generatorURL — was gone the moment this function
// returned, unrecoverable afterwards since core holds no Alertmanager client.
// The raw element is stored verbatim; the decode is only for the routing
// decisions (firing? fingerprint? title?) this handler has to make.
type payload struct {
	Alerts []json.RawMessage `json:"alerts"`
}

type alert struct {
	Status      string            `json:"status"`
	Labels      map[string]string `json:"labels"`
	Annotations map[string]string `json:"annotations"`
	Fingerprint string            `json:"fingerprint"`
}

func (h *Handler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if h.cfg.Token == "" {
		// Refusing to serve is the safe default. A misconfiguration that
		// silently accepted anonymous task creation would be far worse
		// than alerts not reaching thot.
		http.Error(w, "alert webhook disabled: no token configured", http.StatusServiceUnavailable)
		return
	}
	if r.Method != http.MethodPost {
		http.Error(w, "POST only", http.StatusMethodNotAllowed)
		return
	}

	// Constant-time compare — this is a secret check on a public-ish
	// in-cluster endpoint.
	supplied := strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer ")
	if subtle.ConstantTimeCompare([]byte(supplied), []byte(h.cfg.Token)) != 1 {
		http.Error(w, "unauthorized", http.StatusUnauthorized)
		return
	}

	var p payload
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<20)).Decode(&p); err != nil {
		http.Error(w, "bad payload", http.StatusBadRequest)
		return
	}

	// 200, not an error: Alertmanager would only retry the same batch.
	if h.inactive != nil && h.inactive(r.Context(), h.cfg.Repo) {
		slog.Info("alertwebhook: repo inactive, not filing", "repo", h.cfg.Repo, "alerts", len(p.Alerts))
		_, _ = fmt.Fprintf(w, "skipped: repo %s inactive\n", h.cfg.Repo)
		return
	}

	created, skipped := 0, 0
	for _, raw := range p.Alerts {
		// One malformed or failed alert must not discard the rest of the
		// batch: Alertmanager groups alerts, so failing the whole request
		// would drop good alerts along with the bad one, and it retries the
		// batch as a unit. File logs its own failures.
		switch ok, err := h.file(r.Context(), raw); {
		case err != nil:
		case ok:
			created++
		default:
			skipped++
		}
	}

	w.WriteHeader(http.StatusOK)
	_, _ = fmt.Fprintf(w, "created=%d deduped=%d\n", created, skipped)
}

// ErrSkipped is File's answer for an alert it will never file: undecodable,
// not firing, or without a fingerprint. Retrying it changes nothing.
var ErrSkipped = errors.New("alert not filed")

// File files one firing alert as a proposal, deduped on its fingerprint. It
// is shared by this webhook and the bridge's dispatcher (docs/adr/0078 §6):
// whichever path delivers an alert first files it, the other is a no-op, and
// only the call that filed it pings Discord. created is false when a
// proposal already stands or the repo is inactive.
func (h *Handler) File(ctx context.Context, raw json.RawMessage) (created bool, err error) {
	if h.inactive != nil && h.inactive(ctx, h.cfg.Repo) {
		return false, nil
	}
	return h.file(ctx, raw)
}

func (h *Handler) file(ctx context.Context, raw json.RawMessage) (bool, error) {
	var a alert
	if err := json.Unmarshal(raw, &a); err != nil {
		slog.Warn("alertwebhook: undecodable alert, skipping", "error", err)
		return false, ErrSkipped
	}
	// Only firing. A resolved notification needs no investigation, and
	// acting on one would double every alert's task count.
	if a.Status != "firing" {
		return false, ErrSkipped
	}
	key := a.Fingerprint
	if key == "" {
		// Without a fingerprint there is no dedup key, and an alert
		// storm could spawn unbounded tasks. Skip loudly.
		slog.Warn("alertwebhook: alert with no fingerprint, skipping", "labels", a.Labels)
		return false, ErrSkipped
	}

	name := a.Labels["alertname"]
	if name == "" {
		name = "alert"
	}
	id, ok, err := h.proposals.Create(ctx, h.cfg.Repo, "alert", key, truncate(name, 120), describe(a), string(raw))
	if err != nil {
		slog.Error("alertwebhook: create proposal", "fingerprint", key, "error", err)
		return false, err
	}
	if !ok {
		// A proposal for this alert is already standing — the partial
		// unique index doing its job, not a failure.
		return false, nil
	}
	slog.Info("alertwebhook: filed proposal", "proposalId", id, "alert", name)
	h.notify(ctx, id, name, a)
	return true, nil
}

// Resolve dismisses the proposal a resolved alert filed, if nobody opened
// it (docs/adr/0078 §5). Only the bridge path delivers resolutions; the
// direct webhook keeps dropping them. An opened one stays with its session.
func (h *Handler) Resolve(ctx context.Context, raw json.RawMessage) error {
	var a alert
	if err := json.Unmarshal(raw, &a); err != nil || a.Status != "resolved" || a.Fingerprint == "" {
		return ErrSkipped
	}
	_, err := h.proposals.DismissUnopened(ctx, h.cfg.Repo, a.Fingerprint)
	return err
}

// notify is best-effort: losing the Discord ping must not lose the proposal,
// which is already filed and visible in the dashboard by this point.
//
// This is the only notification a firing alert produces, which is why the
// Discord surface could not be deleted outright when its inbound half went
// (docs/adr/0048) — an alert nobody is told about is an alert that did not
// fire.
func (h *Handler) notify(ctx context.Context, proposalID, name string, a alert) {
	if h.discord == nil {
		return
	}
	msg := fmt.Sprintf("**🔥 %s** — filed as proposal `%s`, waiting for you in the dashboard\n%s",
		name, proposalID[:8], a.Annotations["summary"])
	if err := h.discord.Notify(ctx, msg); err != nil {
		slog.Error("alertwebhook: notify", "proposalId", proposalID, "error", err)
	}
}

func describe(a alert) string {
	var b strings.Builder
	b.WriteString("Investigate this firing cluster alert. Use kubectl_read freely to diagnose; anything that changes the cluster will ask a human first.\n\n")
	if name := a.Labels["alertname"]; name != "" {
		b.WriteString("Alert: " + name + "\n")
	}
	if s := a.Annotations["summary"]; s != "" {
		b.WriteString("Summary: " + s + "\n")
	}
	if d := a.Annotations["description"]; d != "" {
		b.WriteString("Description: " + d + "\n")
	}
	for _, k := range []string{"namespace", "pod", "severity", "instance"} {
		if v := a.Labels[k]; v != "" {
			b.WriteString(k + ": " + v + "\n")
		}
	}
	b.WriteString("\nReport what is wrong and why. If a durable fix belongs in infra-bootstrap, open a PR for it.")
	return b.String()
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n]
}
