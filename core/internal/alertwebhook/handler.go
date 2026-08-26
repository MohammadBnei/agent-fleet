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
}

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

	created, skipped := 0, 0
	for _, raw := range p.Alerts {
		var a alert
		if err := json.Unmarshal(raw, &a); err != nil {
			// One malformed alert must not discard the rest of the batch:
			// Alertmanager groups alerts, so failing the whole request here
			// would drop good alerts along with the bad one, and it retries
			// the batch as a unit.
			slog.Warn("alertwebhook: undecodable alert, skipping", "error", err)
			continue
		}
		// Only firing. A resolved notification needs no investigation, and
		// acting on one would double every alert's task count.
		if a.Status != "firing" {
			continue
		}
		key := a.Fingerprint
		if key == "" {
			// Without a fingerprint there is no dedup key, and an alert
			// storm could spawn unbounded tasks. Skip loudly.
			slog.Warn("alertwebhook: alert with no fingerprint, skipping", "labels", a.Labels)
			continue
		}

		name := a.Labels["alertname"]
		if name == "" {
			name = "alert"
		}
		id, ok, err := h.proposals.Create(r.Context(), h.cfg.Repo, "alert", key, truncate(name, 120), describe(a), string(raw))
		if err != nil {
			slog.Error("alertwebhook: create proposal", "fingerprint", key, "error", err)
			continue
		}
		if !ok {
			// A proposal for this alert is already standing — the partial
			// unique index doing its job, not a failure.
			skipped++
			continue
		}
		created++
		slog.Info("alertwebhook: filed proposal", "proposalId", id, "alert", name)
		h.notify(r.Context(), id, name, a)
	}

	w.WriteHeader(http.StatusOK)
	_, _ = fmt.Fprintf(w, "created=%d deduped=%d\n", created, skipped)
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
