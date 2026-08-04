// Package dashboard exposes the web dashboard's REST + SSE API on top of
// the exact same tasks.Store / transcript.Store / e2eclient.Client that
// fleet-core's Discord commands already use (see docs/adr/0013) — no new
// business logic, just a second caller of the same store methods.
package dashboard

import (
	"encoding/json"
	"net/http"
	"strconv"

	"github.com/MohammadBnei/agent-fleet/fleet-core/internal/e2eclient"
	"github.com/MohammadBnei/agent-fleet/fleet-core/internal/tasks"
	"github.com/MohammadBnei/agent-fleet/fleet-core/internal/transcript"
)

// Register wires the dashboard's REST + SSE routes onto mux, alongside
// fleet-core's existing /mcp and /healthz handlers.
func Register(mux *http.ServeMux, taskStore *tasks.Store, transcr transcript.Store, e2e *e2eclient.Client, hub *Hub) {
	mux.HandleFunc("GET /api/tasks", listTasksHandler(taskStore))
	mux.HandleFunc("GET /api/tasks/{id}", getTaskHandler(taskStore))
	mux.HandleFunc("GET /api/tasks/{id}/transcript", transcriptHandler(transcr))
	mux.HandleFunc("GET /api/tasks/{id}/e2e-status", e2eStatusHandler(e2e))
	mux.HandleFunc("GET /api/tasks/{id}/events", sseHandler(hub))

	mux.Handle("POST /api/tasks/{id}/approve", requireDashboardHeader(approveHandler(transcr)))
	mux.Handle("POST /api/tasks/{id}/stop", requireDashboardHeader(stopHandler(transcr)))
	mux.Handle("POST /api/tasks/{id}/e2e/kill", requireDashboardHeader(killE2eHandler(e2e)))
}

func listTasksHandler(store *tasks.Store) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		limit := 50
		if v := r.URL.Query().Get("limit"); v != "" {
			if n, err := strconv.Atoi(v); err == nil && n > 0 {
				limit = n
			}
		}
		list, err := store.ListRecentTasks(r.Context(), limit)
		if err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		writeJSON(w, list)
	}
}

func getTaskHandler(store *tasks.Store) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		t, err := store.GetTask(r.Context(), r.PathValue("id"))
		if err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		if t == nil {
			http.NotFound(w, r)
			return
		}
		writeJSON(w, t)
	}
}

func transcriptHandler(store transcript.Store) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		since := int64(0)
		if v := r.URL.Query().Get("since"); v != "" {
			if n, err := strconv.ParseInt(v, 10, 64); err == nil {
				since = n
			}
		}
		entries, next, err := store.ReadSince(r.Context(), r.PathValue("id"), since, 1000)
		if err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		writeJSON(w, map[string]any{"entries": entries, "nextSeq": next})
	}
}

func e2eStatusHandler(e2e *e2eclient.Client) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		status, previewURL, err := e2e.GetSessionStatus(r.Context(), r.PathValue("id"))
		if err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		writeJSON(w, map[string]string{"status": status, "previewUrl": previewURL})
	}
}

func writeJSON(w http.ResponseWriter, v any) {
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(v)
}
