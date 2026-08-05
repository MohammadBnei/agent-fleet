// Package localapi is the sidecar's second local surface — a plain
// localhost HTTP/JSON API (deliberately not MCP-shaped, not gRPC) for the
// TS worker wrapper's own control-flow and housekeeping calls: everything
// worker/src/planning.ts/db.ts/index.ts used to do via direct SQL, plus —
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

	agentfleetv1 "github.com/MohammadBnei/agent-fleet/proto/gen/go/agentfleet/v1"

	"github.com/MohammadBnei/agent-fleet/sidecar/internal/coreclient"
)

func New(core *coreclient.Client) http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("POST /heartbeat", heartbeatHandler(core))
	mux.HandleFunc("POST /status", statusHandler(core))
	mux.HandleFunc("POST /journal", journalHandler(core))
	mux.HandleFunc("POST /session-id", sessionIDHandler(core))
	mux.HandleFunc("GET /still-holds-lease", stillHoldsLeaseHandler(core))
	mux.HandleFunc("POST /telemetry", telemetryHandler(core))
	mux.HandleFunc("POST /message", messageHandler(core))
	mux.HandleFunc("GET /human-messages", humanMessagesHandler(core))
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
			PlanningSessionID string `json:"planningSessionId"`
			Model             string `json:"model"`
		}
		if err := decodeJSON(r, &body); err != nil {
			writeError(w, http.StatusBadRequest, err)
			return
		}
		if err := core.SaveSessionID(r.Context(), body.PlanningSessionID, body.Model); err != nil {
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
// makes (docs/adr/0021 point 2).
func humanMessagesHandler(core *coreclient.Client) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		sinceSeq := int64(0)
		flusher, ok := w.(http.Flusher)
		if !ok {
			writeError(w, http.StatusInternalServerError, errors.New("response writer does not support flushing"))
			return
		}
		w.Header().Set("Content-Type", "text/event-stream")
		w.Header().Set("Cache-Control", "no-cache")
		w.WriteHeader(http.StatusOK)

		err := core.StreamHumanMessages(r.Context(), sinceSeq, func(entry *agentfleetv1.TranscriptEntry) {
			payload, _ := json.Marshal(map[string]any{
				"seq": entry.GetSeq(), "from": entry.GetFrom(), "text": entry.GetText(), "type": protoTypeToString(entry.GetType()),
			})
			_, _ = w.Write([]byte("data: " + string(payload) + "\n\n"))
			flusher.Flush()
		})
		if err != nil {
			slog.Info("human-messages stream ended", "error", err)
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
	default:
		return ""
	}
}
