package bridgeclient

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"sync"
	"testing"

	"connectrpc.com/connect"

	bridgev1 "github.com/MohammadBnei/ukubi-bridge/proto/gen/go/bridge/v1"
	"github.com/MohammadBnei/ukubi-bridge/proto/gen/go/bridge/v1/bridgev1connect"
)

type fakeBridge struct {
	bridgev1connect.UnimplementedBridgeServiceHandler
	mu     sync.Mutex
	auth   []string
	events []*bridgev1.CloudEvent
}

func (f *fakeBridge) Publish(_ context.Context, req *connect.Request[bridgev1.PublishRequest]) (*connect.Response[bridgev1.PublishResponse], error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.auth = append(f.auth, req.Header().Get("Authorization"))
	f.events = append(f.events, req.Msg.GetEvent())
	return connect.NewResponse(&bridgev1.PublishResponse{}), nil
}

func TestPublishSignsInAsTheServiceAccount(t *testing.T) {
	var grants []string
	var mu sync.Mutex
	token := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = r.ParseForm()
		mu.Lock()
		grants = append(grants, r.Host+" "+r.Header.Get("X-Forwarded-Proto")+" "+r.Form.Get("grant_type")+" "+
			r.Form.Get("client_id")+" "+r.Form.Get("username")+" "+r.Form.Get("password"))
		mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{"access_token": "tok", "token_type": "Bearer", "expires_in": 600})
	}))
	defer token.Close()
	fb := &fakeBridge{}
	mux := http.NewServeMux()
	mux.Handle(bridgev1connect.NewBridgeServiceHandler(fb))
	bridge := httptest.NewServer(mux)
	defer bridge.Close()

	dir := t.TempDir()
	pwFile := filepath.Join(dir, "app_password")
	c := New(Config{URL: bridge.URL, TokenURL: token.URL, AuthHost: "authentik.bnei.dev", ClientID: "ukubi-bridge-api",
		Username: "bridge-core", PasswordFile: pwFile})

	if err := c.Publish(context.Background(), &bridgev1.CloudEvent{Id: "1"}); !errors.Is(err, ErrNotConfigured) {
		t.Fatalf("no password file: %v, want ErrNotConfigured", err)
	}
	if err := os.WriteFile(pwFile, []byte("<no value>\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := c.Publish(context.Background(), &bridgev1.CloudEvent{Id: "1"}); !errors.Is(err, ErrNotConfigured) {
		t.Fatalf("the operator's missing-key placeholder: %v, want ErrNotConfigured", err)
	}
	if err := os.WriteFile(pwFile, []byte("s3cret\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	for _, id := range []string{"1", "2"} {
		if err := c.Publish(context.Background(), &bridgev1.CloudEvent{Id: id}); err != nil {
			t.Fatal(err)
		}
	}
	if len(grants) != 1 || grants[0] != "authentik.bnei.dev https client_credentials ukubi-bridge-api bridge-core s3cret" {
		t.Fatalf("grants %q, want one grant as bridge-core through the public host", grants)
	}
	if len(fb.events) != 2 || fb.auth[0] != "Bearer tok" || fb.auth[1] != "Bearer tok" {
		t.Fatalf("published %d with %q", len(fb.events), fb.auth)
	}

	// A rotated password signs in again.
	if err := os.WriteFile(pwFile, []byte("rotated"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := c.Publish(context.Background(), &bridgev1.CloudEvent{Id: "3"}); err != nil {
		t.Fatal(err)
	}
	if len(grants) != 2 {
		t.Fatalf("a rotated password did not sign in again: %q", grants)
	}
}
