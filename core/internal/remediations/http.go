package remediations

import (
	"context"
	"crypto/subtle"
	"encoding/json"
	"errors"
	"net/http"
	"strings"

	"github.com/google/uuid"
)

// Path is the executor's endpoint: POST Path+"<id>/pull" and Path+"<id>/report".
const Path = "/remediations/"

// Puller is the store's executor side.
type Puller interface {
	Pull(ctx context.Context, id string) (Row, error)
	Report(ctx context.Context, id string, o Outcome) error
}

// Handler serves the remediation executor, authenticated by one bearer token (empty token serves nothing).
func Handler(st Puller, token string) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		got := strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer ")
		if token == "" || r.Method != http.MethodPost || subtle.ConstantTimeCompare([]byte(got), []byte(token)) != 1 {
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return
		}
		id, op, ok := strings.Cut(strings.TrimPrefix(r.URL.Path, Path), "/")
		if _, err := uuid.Parse(id); err != nil || !ok {
			http.NotFound(w, r)
			return
		}
		switch op {
		case "pull":
			row, err := st.Pull(r.Context(), id)
			if err != nil {
				fail(w, err)
				return
			}
			_ = json.NewEncoder(w).Encode(row)
		case "report":
			var o Outcome
			if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<16)).Decode(&o); err != nil {
				http.Error(w, "bad body", http.StatusBadRequest)
				return
			}
			if err := st.Report(r.Context(), id, o); err != nil {
				fail(w, err)
				return
			}
			w.WriteHeader(http.StatusNoContent)
		default:
			http.NotFound(w, r)
		}
	})
}

func fail(w http.ResponseWriter, err error) {
	switch {
	case errors.Is(err, ErrNotFound):
		http.Error(w, "not found or already used", http.StatusNotFound)
	case errors.Is(err, ErrInvalid):
		http.Error(w, "invalid", http.StatusBadRequest)
	default:
		http.Error(w, "internal", http.StatusInternalServerError)
	}
}
