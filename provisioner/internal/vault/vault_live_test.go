//go:build integration

package vault_test

import (
	"context"
	"net/http"
	"os"
	"sync"
	"testing"

	"github.com/MohammadBnei/agent-fleet/provisioner/internal/vault"
)

// The fake echoes services; this checks the real v0.40.0 round trip: a
// second reconcile of an unchanged spec makes no write. Needs a throwaway
// vault and a member agent token, e.g.
//
//	docker run -d -p 127.0.0.1:14321:14321 -e AGENT_VAULT_MASTER_PASSWORD=… infisical/agent-vault:0.40.0@sha256:4d490e25… server
//	AGENT_VAULT_LIVE_API=http://127.0.0.1:14321 AGENT_VAULT_LIVE_TOKEN_FILE=… go test -tags=integration -run Live ./internal/vault/
func TestLive_ReconcileIdempotent(t *testing.T) {
	api, tokFile := os.Getenv("AGENT_VAULT_LIVE_API"), os.Getenv("AGENT_VAULT_LIVE_TOKEN_FILE")
	if api == "" || tokFile == "" {
		t.Skip("AGENT_VAULT_LIVE_API / AGENT_VAULT_LIVE_TOKEN_FILE unset")
	}
	tok, err := os.ReadFile(tokFile)
	if err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	writeSecrets(t, dir, map[string]string{"AGENT_VAULT_TOKEN": string(tok)})
	writeSecrets(t, dir, notesSecrets)
	c := vault.New(api, "unused:14322", dir)
	rec := &recorder{next: http.DefaultTransport}
	c.HTTP.Transport = rec
	ctx := context.Background()
	if _, err := c.Reconcile(ctx, notesSpec()); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _, _ = c.Reconcile(ctx, vault.Spec{Name: "dev-notes"}) })
	rec.reset()
	if _, err := c.Reconcile(ctx, notesSpec()); err != nil {
		t.Fatal(err)
	}
	if w := rec.writes(); len(w) != 0 {
		t.Errorf("an unchanged reconcile against v0.40.0 wrote: %v", w)
	}
}

type recorder struct {
	next http.RoundTripper
	mu   sync.Mutex
	log  []string
}

func (r *recorder) RoundTrip(req *http.Request) (*http.Response, error) {
	r.mu.Lock()
	r.log = append(r.log, req.Method+" "+req.URL.Path)
	r.mu.Unlock()
	return r.next.RoundTrip(req)
}

func (r *recorder) reset() { r.mu.Lock(); r.log = nil; r.mu.Unlock() }

func (r *recorder) writes() []string {
	r.mu.Lock()
	defer r.mu.Unlock()
	var out []string
	for _, l := range r.log {
		if l[:4] != "GET " {
			out = append(out, l)
		}
	}
	return out
}
