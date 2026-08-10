// Package localapi is the sidecar's second local surface — a plain
// localhost HTTP/JSON API (deliberately not MCP-shaped, not gRPC) for the
// TS worker wrapper's own control-flow and housekeeping calls: everything
// worker/src/session.ts/db.ts/index.ts used to do via direct SQL, plus —
// the load-bearing one — delivering live human messages for streamInput()
// (docs/adr/0020 point 5's third responsibility, docs/adr/0021 point 2).
// None of this is something the agent decides to do; it's the wrapper's
// own event-driven orchestration, kept on a separate surface from the
// agent-facing MCP server above even though both funnel through the same
// coreclient.Client underneath. Plain HTTP/JSON, not gRPC, so the worker
// doesn't need a gRPC client library dependency for a single local,
// low-volume, request-response API — Bun's native fetch() is enough.
package localapi

import (
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
	"strconv"
	"sync"
	"time"

	agentfleetv1 "github.com/MohammadBnei/agent-fleet/proto/gen/go/agentfleet/v1"

	"github.com/MohammadBnei/agent-fleet/sidecar/internal/coreclient"
)

// sseKeepAliveInterval is how often humanMessagesHandler writes a comment
// line while idle. Confirmed live in prod: with no keep-alive, this feed
// can go silent at the wire level for as long as there's simply no new
// human message (core's own poll loop never touches the wire when it
// finds nothing new) — long enough for something upstream to eventually
// kill the connection outright (a redeploy, an idle-connection reaper,
// ...), which then permanently ends this task's ability to ever receive
// another human message (see streamHumanMessages' own reconnect loop on
// the worker side for the other half of this fix). 15s is comfortably
// under any idle-connection policy this is realistically up against.
const sseKeepAliveInterval = 15 * time.Second

func New(core *coreclient.Client) http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /readyz", readyzHandler(core))
	mux.HandleFunc("POST /heartbeat", heartbeatHandler(core))
	mux.HandleFunc("POST /status", statusHandler(core))
	mux.HandleFunc("POST /journal", journalHandler(core))
	mux.HandleFunc("POST /session-id", sessionIDHandler(core))
	mux.HandleFunc("GET /still-holds-lease", stillHoldsLeaseHandler(core))
	mux.HandleFunc("POST /telemetry", telemetryHandler(core))
	mux.HandleFunc("POST /message", messageHandler(core))
	mux.HandleFunc("GET /human-messages", humanMessagesHandler(core))
	mux.HandleFunc("GET /task", taskHandler(core))
	mux.HandleFunc("POST /permission-mode", permissionModeHandler(core))
	return mux
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

func writeError(w http.ResponseWriter, status int, err error) {
	writeJSON(w, status, map[string]string{"error": err.Error()})
}

func decodeJSON(r *http.Request, v any) error {
	defer func() { _ = r.Body.Close() }()
	return json.NewDecoder(r.Body).Decode(v)
}

// readyzHandler backs the provisioner's StartupProbe on the sidecar init
// container — 200 only once core.WaitReady (started in main) has observed a
// live connection to core, 503 until then. Deliberately not the same as
// "process is up": a bare TCP/liveness check passes immediately regardless
// of whether core is reachable, which is what let the worker container
// start against an unreachable core in the first place.
func readyzHandler(core *coreclient.Client) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if !core.Ready() {
			w.WriteHeader(http.StatusServiceUnavailable)
			return
		}
		w.WriteHeader(http.StatusOK)
	}
}

func heartbeatHandler(core *coreclient.Client) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		var body struct {
			LeaseID string `json:"leaseId"`
		}
		if err := decodeJSON(r, &body); err != nil {
			writeError(w, http.StatusBadRequest, err)
			return
		}
		if err := core.Heartbeat(r.Context(), body.LeaseID); err != nil {
			writeError(w, http.StatusBadGateway, err)
			return
		}
		writeJSON(w, http.StatusOK, map[string]bool{"ok": true})
	}
}

func statusHandler(core *coreclient.Client) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		var body struct {
			Status    string  `json:"status"`
			PrURL     *string `json:"prUrl"`
			Notes     *string `json:"notes"`
			LastError *string `json:"lastError"`
		}
		if err := decodeJSON(r, &body); err != nil {
			writeError(w, http.StatusBadRequest, err)
			return
		}
		if err := core.SetTaskStatus(r.Context(), body.Status, body.PrURL, body.Notes, body.LastError); err != nil {
			writeError(w, http.StatusBadGateway, err)
			return
		}
		writeJSON(w, http.StatusOK, map[string]bool{"ok": true})
	}
}

func journalHandler(core *coreclient.Client) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		var body struct {
			Repo      string `json:"repo"`
			Actor     string `json:"actor"`
			EventType string `json:"eventType"`
			Payload   any    `json:"payload"`
		}
		if err := decodeJSON(r, &body); err != nil {
			writeError(w, http.StatusBadRequest, err)
			return
		}
		payloadJSON, err := json.Marshal(body.Payload)
		if err != nil {
			writeError(w, http.StatusBadRequest, err)
			return
		}
		if err := core.AppendJournal(r.Context(), body.Repo, body.Actor, body.EventType, string(payloadJSON)); err != nil {
			writeError(w, http.StatusBadGateway, err)
			return
		}
		writeJSON(w, http.StatusOK, map[string]bool{"ok": true})
	}
}

func sessionIDHandler(core *coreclient.Client) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		var body struct {
			SessionID string `json:"sessionId"`
			Model     string `json:"model"`
		}
		if err := decodeJSON(r, &body); err != nil {
			writeError(w, http.StatusBadRequest, err)
			return
		}
		if err := core.SaveSessionID(r.Context(), body.SessionID, body.Model); err != nil {
			writeError(w, http.StatusBadGateway, err)
			return
		}
		writeJSON(w, http.StatusOK, map[string]bool{"ok": true})
	}
}

func stillHoldsLeaseHandler(core *coreclient.Client) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		leaseID := r.URL.Query().Get("leaseId")
		holds, err := core.StillHoldsLease(r.Context(), leaseID)
		if err != nil {
			writeError(w, http.StatusBadGateway, err)
			return
		}
		writeJSON(w, http.StatusOK, map[string]bool{"holds": holds})
	}
}

func telemetryHandler(core *coreclient.Client) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		var summary any
		if err := decodeJSON(r, &summary); err != nil {
			writeError(w, http.StatusBadRequest, err)
			return
		}
		summaryJSON, err := json.Marshal(summary)
		if err != nil {
			writeError(w, http.StatusBadRequest, err)
			return
		}
		if err := core.PushToolTelemetry(r.Context(), string(summaryJSON)); err != nil {
			writeError(w, http.StatusBadGateway, err)
			return
		}
		writeJSON(w, http.StatusOK, map[string]bool{"ok": true})
	}
}

// messageHandler lets the wrapper itself post into the transcript — the
// round-cap checkpoint text and the agent's raw assistant narration (today
// relayed directly to Discord by worker/src/discord.ts, now gone; both
// route through core's existing relay loop instead, so Discord delivery
// stays uniform whether a message came from the agent's own send_message
// tool call or the wrapper's own housekeeping). Not agent-initiated, so it
// lives here rather than the MCP server, even though it ends up as the
// exact same kind of transcript entry.
func messageHandler(core *coreclient.Client) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		var body struct {
			From string `json:"from"`
			Text string `json:"text"`
			Type string `json:"type"`
		}
		if err := decodeJSON(r, &body); err != nil {
			writeError(w, http.StatusBadRequest, err)
			return
		}
		if body.From == "" || body.Text == "" {
			writeError(w, http.StatusBadRequest, errors.New("from and text are required"))
			return
		}
		seq, err := core.SendMessage(r.Context(), body.From, body.Text, stringToProtoType(body.Type), "")
		if err != nil {
			writeError(w, http.StatusBadGateway, err)
			return
		}
		writeJSON(w, http.StatusOK, map[string]int64{"seq": seq})
	}
}

// humanMessagesHandler is Server-Sent Events, not a gRPC stream — the
// worker consumes it with a plain fetch()+ReadableStream, no new
// client-library dependency needed for the one local streaming call it
// makes (docs/adr/0021 point 2). sinceSeq comes from the query string —
// the worker's own reconnect loop passes the last seq it actually
// processed so a reconnect resumes exactly where it left off instead of
// replaying (or worse, silently missing) history. It runs
// core.StreamHumanMessages in its own goroutine so a keep-alive ticker
// can still write to the response while that call is blocked waiting on
// the next real message.
func humanMessagesHandler(core *coreclient.Client) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		sinceSeq := int64(0)
		if v := r.URL.Query().Get("sinceSeq"); v != "" {
			if parsed, err := strconv.ParseInt(v, 10, 64); err == nil {
				sinceSeq = parsed
			}
		}
		flusher, ok := w.(http.Flusher)
		if !ok {
			writeError(w, http.StatusInternalServerError, errors.New("response writer does not support flushing"))
			return
		}
		w.Header().Set("Content-Type", "text/event-stream")
		w.Header().Set("Cache-Control", "no-cache")
		w.WriteHeader(http.StatusOK)

		// core.StreamHumanMessages only ever writes when onEntry fires, so
		// without this ticker the connection can go byte-silent for as long
		// as there's simply nothing new to deliver.
		var writeMu sync.Mutex
		write := func(b []byte) {
			writeMu.Lock()
			defer writeMu.Unlock()
			_, _ = w.Write(b)
			flusher.Flush()
		}

		done := make(chan error, 1)
		go func() {
			done <- core.StreamHumanMessages(r.Context(), sinceSeq, func(entry *agentfleetv1.TranscriptEntry) {
				payload, _ := json.Marshal(map[string]any{
					"seq": entry.GetSeq(), "from": entry.GetFrom(), "text": entry.GetText(), "type": protoTypeToString(entry.GetType()),
					"replyTo": entry.ReplyTo,
				})
				write([]byte("data: " + string(payload) + "\n\n"))
			})
		}()

		ticker := time.NewTicker(sseKeepAliveInterval)
		defer ticker.Stop()
		for {
			select {
			case err := <-done:
				if err != nil {
					slog.Info("human-messages stream ended", "error", err)
				}
				return
			case <-ticker.C:
				write([]byte(": keep-alive\n\n"))
			case <-r.Context().Done():
				return
			}
		}
	}
}

func stringToProtoType(s string) agentfleetv1.TranscriptEntryType {
	switch s {
	case "discussion":
		return agentfleetv1.TranscriptEntryType_TRANSCRIPT_ENTRY_TYPE_DISCUSSION
	case "approve":
		return agentfleetv1.TranscriptEntryType_TRANSCRIPT_ENTRY_TYPE_APPROVE
	case "abort":
		return agentfleetv1.TranscriptEntryType_TRANSCRIPT_ENTRY_TYPE_ABORT
	case "system":
		return agentfleetv1.TranscriptEntryType_TRANSCRIPT_ENTRY_TYPE_SYSTEM
	case "assistant":
		return agentfleetv1.TranscriptEntryType_TRANSCRIPT_ENTRY_TYPE_ASSISTANT
	case "user":
		return agentfleetv1.TranscriptEntryType_TRANSCRIPT_ENTRY_TYPE_USER
	case "result":
		return agentfleetv1.TranscriptEntryType_TRANSCRIPT_ENTRY_TYPE_RESULT
	case "permission_mode":
		return agentfleetv1.TranscriptEntryType_TRANSCRIPT_ENTRY_TYPE_PERMISSION_MODE
	case "permission_request":
		return agentfleetv1.TranscriptEntryType_TRANSCRIPT_ENTRY_TYPE_PERMISSION_REQUEST
	case "permission_response":
		return agentfleetv1.TranscriptEntryType_TRANSCRIPT_ENTRY_TYPE_PERMISSION_RESPONSE
	case "interrupt":
		return agentfleetv1.TranscriptEntryType_TRANSCRIPT_ENTRY_TYPE_INTERRUPT
	default:
		return agentfleetv1.TranscriptEntryType_TRANSCRIPT_ENTRY_TYPE_UNSPECIFIED
	}
}

func protoTypeToString(t agentfleetv1.TranscriptEntryType) string {
	switch t {
	case agentfleetv1.TranscriptEntryType_TRANSCRIPT_ENTRY_TYPE_DISCUSSION:
		return "discussion"
	case agentfleetv1.TranscriptEntryType_TRANSCRIPT_ENTRY_TYPE_APPROVE:
		return "approve"
	case agentfleetv1.TranscriptEntryType_TRANSCRIPT_ENTRY_TYPE_ABORT:
		return "abort"
	case agentfleetv1.TranscriptEntryType_TRANSCRIPT_ENTRY_TYPE_QUESTION:
		return "question"
	case agentfleetv1.TranscriptEntryType_TRANSCRIPT_ENTRY_TYPE_ANSWER:
		return "answer"
	case agentfleetv1.TranscriptEntryType_TRANSCRIPT_ENTRY_TYPE_SYSTEM:
		return "system"
	case agentfleetv1.TranscriptEntryType_TRANSCRIPT_ENTRY_TYPE_ASSISTANT:
		return "assistant"
	case agentfleetv1.TranscriptEntryType_TRANSCRIPT_ENTRY_TYPE_USER:
		return "user"
	case agentfleetv1.TranscriptEntryType_TRANSCRIPT_ENTRY_TYPE_RESULT:
		return "result"
	case agentfleetv1.TranscriptEntryType_TRANSCRIPT_ENTRY_TYPE_PERMISSION_MODE:
		return "permission_mode"
	case agentfleetv1.TranscriptEntryType_TRANSCRIPT_ENTRY_TYPE_PERMISSION_REQUEST:
		return "permission_request"
	case agentfleetv1.TranscriptEntryType_TRANSCRIPT_ENTRY_TYPE_PERMISSION_RESPONSE:
		return "permission_response"
	case agentfleetv1.TranscriptEntryType_TRANSCRIPT_ENTRY_TYPE_INTERRUPT:
		return "interrupt"
	default:
		return ""
	}
}

// taskHandler fetches task details from the database via the dashboard API,
// including model and permission_mode. Used by the worker on startup to get
// fresh task data instead of relying on stale environment variables.
func taskHandler(core *coreclient.Client) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		task, err := core.GetTask(r.Context())
		if err != nil {
			writeError(w, http.StatusBadGateway, err)
			return
		}
		// Return only the fields the worker needs
		response := map[string]interface{}{
			"description": task.GetDescription(),
			"guidance":    "",
			"baseBranch":  "main",
		}
		// Add optional fields if they exist
		// Note: We need to find the repo's base branch from somewhere
		// For now, we'll use "main" as default since it's not in the Task proto
		if task.PermissionMode != nil {
			response["permissionMode"] = *task.PermissionMode
		}
		// Model field will be added when proto regenerates
		writeJSON(w, http.StatusOK, response)
	}
}

// permissionModeHandler persists the current permission mode to the database.
// Used by the worker to save the initial "default" mode or when the mode
// changes.
func permissionModeHandler(core *coreclient.Client) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		var body struct {
			Mode string `json:"mode"`
		}
		if err := decodeJSON(r, &body); err != nil {
			writeError(w, http.StatusBadRequest, err)
			return
		}
		if body.Mode == "" {
			writeError(w, http.StatusBadRequest, errors.New("mode is required"))
			return
		}
		if err := core.SetPermissionMode(r.Context(), body.Mode); err != nil {
			writeError(w, http.StatusBadGateway, err)
			return
		}
		writeJSON(w, http.StatusOK, map[string]bool{"ok": true})
	}
}
