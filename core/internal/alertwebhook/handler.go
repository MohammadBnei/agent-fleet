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

// ThreadOpener is the Discord surface, narrowed so this package can be
// tested without a bot session. nil disables the Discord side entirely.
type ThreadOpener interface {
	OpenThread(channelID, title, body string) (string, error)
}

// TaskCreator is tasks.Store, narrowed to the one method used.
type TaskCreator interface {
	CreateDeduped(ctx context.Context, kind, externalKey, repo, description string, channelID, threadID *string) (string, bool, error)
}

type Config struct {
	// Token is a shared secret Alertmanager sends as a bearer. Required in
	// practice: this endpoint creates tasks, and a thot task runs an agent
	// with cluster access, so an unauthenticated caller inside the cluster
	// could spawn privileged work. Empty disables the endpoint entirely
	// rather than serving it open.
	Token string
	// Repo a thot session runs on (its worktree, and where a durable fix
	// becomes a PR).
	Repo string
	// ChannelID for the Discord thread. Empty falls back to the trigger
	// channel inside OpenThread.
	ChannelID string
}

type Handler struct {
	tasks   TaskCreator
	discord ThreadOpener
	cfg     Config
}

func New(tasks TaskCreator, discord ThreadOpener, cfg Config) *Handler {
	return &Handler{tasks: tasks, discord: discord, cfg: cfg}
}

// payload is the subset of Alertmanager's webhook body this needs. Its
// schema is far larger; decoding only these fields means an Alertmanager
// upgrade adding fields can't break parsing.
type payload struct {
	Alerts []alert `json:"alerts"`
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
	for _, a := range p.Alerts {
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

		id, ok, err := h.tasks.CreateDeduped(r.Context(), "thot", key, h.cfg.Repo, describe(a), nil, nil)
		if err != nil {
			slog.Error("alertwebhook: create task", "fingerprint", key, "error", err)
			continue
		}
		if !ok {
			// An active task for this alert already exists — the dedup
			// index doing its job, not a failure.
			skipped++
			continue
		}
		created++
		slog.Info("alertwebhook: created thot task", "sessionId", id, "alert", a.Labels["alertname"])
		h.attachThread(r.Context(), id, a)
	}

	w.WriteHeader(http.StatusOK)
	_, _ = fmt.Fprintf(w, "created=%d deduped=%d\n", created, skipped)
}

// attachThread is best-effort: losing the Discord thread must not lose the
// task, which is already created and dispatching by this point.
func (h *Handler) attachThread(_ context.Context, taskID string, a alert) {
	if h.discord == nil {
		return
	}
	name := a.Labels["alertname"]
	if name == "" {
		name = "alert"
	}
	_, err := h.discord.OpenThread(
		h.cfg.ChannelID,
		truncate(name, 90),
		fmt.Sprintf("**🔥 %s** — proposed as task `%s`, awaiting approval in the dashboard\n%s", name, taskID[:8], a.Annotations["summary"]),
	)
	if err != nil {
		slog.Error("alertwebhook: open thread", "sessionId", taskID, "error", err)
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
