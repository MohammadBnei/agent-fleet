// Package vaulttest is an in-memory Agent Vault API (v0.40.0 shapes, the
// subset internal/vault calls) for tests in internal/vault and its callers.
package vaulttest

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"
)

// Token is the agent token the fake accepts.
const Token = "av_agt_test"

// CA is what GET /v1/mitm/ca.pem returns.
const CA = "-----BEGIN CERTIFICATE-----\nfake\n-----END CERTIFICATE-----\n"

type Session struct {
	ID, Label, Token string
	TTL              int
}

type Vault struct {
	Services json.RawMessage
	Creds    map[string]string
	Policy   string
	Sessions []Session
}

type Server struct {
	*httptest.Server
	mu     sync.Mutex
	Vaults map[string]*Vault
	// Log is every request, "METHOD /path?query", in order.
	Log []string
	// Delay is added before every response (a slow vault).
	Delay time.Duration
	// ConflictOnCreate answers POST /v1/vaults with 409 after creating the
	// vault (it was created concurrently elsewhere).
	ConflictOnCreate bool
	// CredentialsError answers POST /v1/credentials with 400 and this text.
	CredentialsError string
	n                int
}

func New(t *testing.T) *Server {
	t.Helper()
	f := &Server{Vaults: map[string]*Vault{}}
	f.Server = httptest.NewServer(http.HandlerFunc(f.serve))
	t.Cleanup(f.Close)
	return f
}

// Writes is every non-GET request logged so far.
func (f *Server) Writes() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	var out []string
	for _, l := range f.Log {
		if !strings.HasPrefix(l, "GET ") {
			out = append(out, l)
		}
	}
	return out
}

// Calls is every request logged so far.
func (f *Server) Calls() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.Log...)
}

// Names lists the vaults.
func (f *Server) Names() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	var out []string
	for n := range f.Vaults {
		out = append(out, n)
	}
	return out
}

// Configure changes the fake's knobs under its lock.
func (f *Server) Configure(fn func(*Server)) {
	f.mu.Lock()
	defer f.mu.Unlock()
	fn(f)
}

func (f *Server) ResetLog() {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.Log = nil
}

// Get returns a copy-safe view of a vault under the lock.
func (f *Server) Get(name string) (Vault, bool) {
	f.mu.Lock()
	defer f.mu.Unlock()
	v, ok := f.Vaults[name]
	if !ok {
		return Vault{}, false
	}
	out := *v
	out.Creds = map[string]string{}
	for k, x := range v.Creds {
		out.Creds[k] = x
	}
	out.Sessions = append([]Session(nil), v.Sessions...)
	return out, true
}

// AddSession seeds a session (for revoke tests).
func (f *Server) AddSession(vault, label string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	v := f.vault(vault)
	f.n++
	v.Sessions = append(v.Sessions, Session{ID: fmt.Sprintf("s%d", f.n), Label: label, Token: fmt.Sprintf("av_sess_%d", f.n)})
}

func (f *Server) vault(name string) *Vault {
	v, ok := f.Vaults[name]
	if !ok {
		v = &Vault{Services: json.RawMessage("[]"), Creds: map[string]string{}, Policy: "passthrough"}
		f.Vaults[name] = v
	}
	return v
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

func fail(w http.ResponseWriter, status int, msg string) {
	writeJSON(w, status, map[string]string{"error": msg})
}

func (f *Server) serve(w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	delay, conflict, credErr := f.Delay, f.ConflictOnCreate, f.CredentialsError
	line := r.Method + " " + r.URL.Path
	if r.URL.RawQuery != "" {
		line += "?" + r.URL.RawQuery
	}
	f.Log = append(f.Log, line)
	f.mu.Unlock()
	if delay > 0 {
		select {
		case <-time.After(delay):
		case <-r.Context().Done():
			return
		}
	}
	if r.URL.Path == "/v1/mitm/ca.pem" {
		_, _ = w.Write([]byte(CA))
		return
	}
	if r.Header.Get("Authorization") != "Bearer "+Token {
		fail(w, http.StatusUnauthorized, "Unauthorized")
		return
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	q := r.URL.Query().Get("vault")
	parts := strings.Split(strings.Trim(r.URL.Path, "/"), "/")
	switch {
	case r.Method == http.MethodGet && r.URL.Path == "/v1/vaults":
		items := []map[string]string{}
		for n := range f.Vaults {
			items = append(items, map[string]string{"name": n})
		}
		writeJSON(w, http.StatusOK, map[string]any{"vaults": items})
	case r.Method == http.MethodPost && r.URL.Path == "/v1/vaults":
		var req struct{ Name string }
		_ = json.NewDecoder(r.Body).Decode(&req)
		if _, ok := f.Vaults[req.Name]; ok {
			fail(w, http.StatusConflict, "exists")
			return
		}
		f.vault(req.Name)
		if conflict {
			fail(w, http.StatusConflict, "exists")
			return
		}
		writeJSON(w, http.StatusCreated, map[string]string{"name": req.Name})
	case len(parts) == 4 && parts[1] == "vaults" && parts[3] == "services":
		v, ok := f.Vaults[parts[2]]
		if !ok {
			fail(w, http.StatusNotFound, "Vault not found")
			return
		}
		// Echoing the PUT body is faithful for the fields the client sends:
		// v0.40.0 splits "host/path" on ingest and broker.Service.MarshalJSON
		// rejoins it on GET. vault_live_test.go checks the real round trip.
		if r.Method == http.MethodPut {
			var req struct{ Services json.RawMessage }
			_ = json.NewDecoder(r.Body).Decode(&req)
			v.Services = req.Services
			writeJSON(w, http.StatusOK, map[string]any{"vault": parts[2]})
			return
		}
		writeJSON(w, http.StatusOK, map[string]any{"vault": parts[2], "services": v.Services})
	case len(parts) == 4 && parts[1] == "vaults" && parts[3] == "settings":
		v, ok := f.Vaults[parts[2]]
		if !ok {
			fail(w, http.StatusNotFound, "Vault not found")
			return
		}
		if r.Method == http.MethodPatch {
			var req struct {
				Policy string `json:"unmatched_host_policy"`
			}
			_ = json.NewDecoder(r.Body).Decode(&req)
			v.Policy = req.Policy
		}
		writeJSON(w, http.StatusOK, map[string]string{"unmatched_host_policy": v.Policy})
	case r.URL.Path == "/v1/credentials":
		var req struct {
			Vault       string
			Credentials map[string]string
			Keys        []string
		}
		if r.Method != http.MethodGet {
			_ = json.NewDecoder(r.Body).Decode(&req)
			q = req.Vault
		}
		v, ok := f.Vaults[q]
		if !ok {
			fail(w, http.StatusNotFound, "Vault not found")
			return
		}
		switch r.Method {
		case http.MethodGet:
			keys := []string{}
			for k := range v.Creds {
				keys = append(keys, k)
			}
			writeJSON(w, http.StatusOK, map[string]any{"keys": keys})
		case http.MethodPost:
			if credErr != "" {
				fail(w, http.StatusBadRequest, credErr)
				return
			}
			for k, x := range req.Credentials {
				v.Creds[k] = x
			}
			writeJSON(w, http.StatusOK, map[string]any{"set": len(req.Credentials)})
		case http.MethodDelete:
			for _, k := range req.Keys {
				delete(v.Creds, k)
			}
			writeJSON(w, http.StatusOK, map[string]any{"deleted": req.Keys})
		}
	case r.URL.Path == "/v1/sessions" && r.Method == http.MethodPost:
		var req struct {
			Vault string `json:"vault"`
			TTL   int    `json:"ttl_seconds"`
			Label string `json:"label"`
		}
		_ = json.NewDecoder(r.Body).Decode(&req)
		v, ok := f.Vaults[req.Vault]
		if !ok {
			fail(w, http.StatusNotFound, "Vault not found")
			return
		}
		if req.TTL < 300 || req.TTL > 604800 {
			fail(w, http.StatusBadRequest, "ttl_seconds must be between 300 and 604800")
			return
		}
		f.n++
		s := Session{ID: fmt.Sprintf("s%d", f.n), Label: req.Label, Token: fmt.Sprintf("av_sess_%d", f.n), TTL: req.TTL}
		v.Sessions = append(v.Sessions, s)
		writeJSON(w, http.StatusOK, map[string]string{"token": s.Token, "expires_at": "x"})
	case r.URL.Path == "/v1/sessions" && r.Method == http.MethodGet:
		v, ok := f.Vaults[q]
		if !ok {
			fail(w, http.StatusNotFound, "Vault not found")
			return
		}
		out := []map[string]string{}
		for _, s := range v.Sessions {
			out = append(out, map[string]string{"id": s.ID, "label": s.Label})
		}
		writeJSON(w, http.StatusOK, map[string]any{"sessions": out})
	case len(parts) == 3 && parts[1] == "sessions" && r.Method == http.MethodDelete:
		v, ok := f.Vaults[q]
		if !ok {
			fail(w, http.StatusBadRequest, "vault is required")
			return
		}
		for i, s := range v.Sessions {
			if s.ID == parts[2] {
				v.Sessions = append(v.Sessions[:i], v.Sessions[i+1:]...)
				writeJSON(w, http.StatusOK, map[string]string{})
				return
			}
		}
		fail(w, http.StatusNotFound, "not found")
	default:
		fail(w, http.StatusNotFound, "no route "+line)
	}
}
