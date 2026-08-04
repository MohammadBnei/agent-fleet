package dashboard

import (
	"encoding/json"
	"log/slog"
	"net/http"

	"github.com/google/uuid"

	"github.com/MohammadBnei/agent-fleet/fleet-core/internal/e2eclient"
	"github.com/MohammadBnei/agent-fleet/fleet-core/internal/transcript"
)

// approveHandler, stopHandler, and killE2eHandler below call the exact same
// store methods fleet-core/internal/discord/handlers.go's /approve, /stop,
// and /e2e-kill commands already call — no new business logic, just a
// second caller (see docs/adr/0013).

func approveHandler(transcr transcript.Store) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		taskID := r.PathValue("id")
		if _, err := transcr.Append(r.Context(), taskID, "human", "approved", "approve", uuid.NewString()); err != nil {
			slog.Error("dashboard approve failed", "taskId", taskID, "error", err)
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		writeJSON(w, map[string]string{"status": "approved"})
	}
}

type stopRequest struct {
	Reason string `json:"reason"`
}

func stopHandler(transcr transcript.Store) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		taskID := r.PathValue("id")
		reason := "stopped by human"
		var body stopRequest
		if err := json.NewDecoder(r.Body).Decode(&body); err == nil && body.Reason != "" {
			reason = body.Reason
		}
		if _, err := transcr.Append(r.Context(), taskID, "human", reason, "abort", uuid.NewString()); err != nil {
			slog.Error("dashboard stop failed", "taskId", taskID, "error", err)
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		writeJSON(w, map[string]string{"status": "stopping"})
	}
}

func killE2eHandler(e2e *e2eclient.Client) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		taskID := r.PathValue("id")
		killed, err := e2e.KillSession(r.Context(), taskID, uuid.NewString())
		if err != nil {
			slog.Error("dashboard e2e-kill failed", "taskId", taskID, "error", err)
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		writeJSON(w, map[string]bool{"killed": killed})
	}
}
