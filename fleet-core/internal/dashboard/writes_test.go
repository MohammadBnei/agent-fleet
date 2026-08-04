package dashboard

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/MohammadBnei/agent-fleet/fleet-core/internal/transcript"
)

// recordingStore captures the arguments its last Append call was made
// with — enough to verify writes.go's handlers call transcript.Store the
// same way fleet-core/internal/discord/handlers.go's Discord commands do
// (see docs/adr/0013), without needing a real Postgres for logic this
// thin. transcript.Store.Append's own idempotency guarantee is
// PostgresStore's responsibility, not this package's.
type recordingStore struct {
	lastTaskID, lastFrom, lastText, lastType string
}

func (r *recordingStore) Append(_ context.Context, taskID, from, text, msgType, _ string) (int64, error) {
	r.lastTaskID, r.lastFrom, r.lastText, r.lastType = taskID, from, text, msgType
	return 0, nil
}

func (r *recordingStore) ReadSince(context.Context, string, int64, int) ([]transcript.Entry, int64, error) {
	return nil, 0, nil
}

func TestApproveHandler(t *testing.T) {
	store := &recordingStore{}
	mux := http.NewServeMux()
	mux.HandleFunc("POST /t/{id}/approve", approveHandler(store))

	req := httptest.NewRequest(http.MethodPost, "/t/task-1/approve", nil)
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", w.Code, w.Body)
	}
	if store.lastTaskID != "task-1" || store.lastFrom != "human" || store.lastText != "approved" || store.lastType != "approve" {
		t.Errorf("Append(%q, %q, %q, %q), want (task-1, human, approved, approve)",
			store.lastTaskID, store.lastFrom, store.lastText, store.lastType)
	}
}

func TestStopHandler_DefaultReason(t *testing.T) {
	store := &recordingStore{}
	mux := http.NewServeMux()
	mux.HandleFunc("POST /t/{id}/stop", stopHandler(store))

	req := httptest.NewRequest(http.MethodPost, "/t/task-1/stop", nil)
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", w.Code, w.Body)
	}
	if store.lastText != "stopped by human" || store.lastType != "abort" {
		t.Errorf("got (%q, %q), want (stopped by human, abort)", store.lastText, store.lastType)
	}
}

func TestStopHandler_CustomReason(t *testing.T) {
	store := &recordingStore{}
	mux := http.NewServeMux()
	mux.HandleFunc("POST /t/{id}/stop", stopHandler(store))

	req := httptest.NewRequest(http.MethodPost, "/t/task-1/stop", strings.NewReader(`{"reason":"wrong direction"}`))
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", w.Code, w.Body)
	}
	if store.lastText != "wrong direction" {
		t.Errorf("text = %q, want %q", store.lastText, "wrong direction")
	}
}
