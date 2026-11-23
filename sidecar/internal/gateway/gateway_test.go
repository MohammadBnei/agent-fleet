package gateway

import (
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func setup(t *testing.T, key string, up http.Handler) (http.Handler, string) {
	t.Helper()
	srv := httptest.NewServer(up)
	t.Cleanup(srv.Close)
	u, _ := url.Parse(srv.URL)
	f := filepath.Join(t.TempDir(), "GATEWAY_KEY")
	if key != "" {
		if err := os.WriteFile(f, []byte(key+"\n"), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	return New(u, f, "sess-1"), f
}

func TestInjectsKeyStripsClientAuthOverwritesIdentity(t *testing.T) {
	var got http.Header
	h, _ := setup(t, "gw-key", http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { got = r.Header.Clone() }))
	r := httptest.NewRequest("POST", "/v1/messages", strings.NewReader("{}"))
	r.Header.Set("Authorization", "Bearer client")
	r.Header.Set("x-api-key", "client-key")
	r.Header.Set("X-Ukubi-Session", "spoofed")
	r.Header.Set("X-Ukubi-Tag", "spoofed")
	h.ServeHTTP(httptest.NewRecorder(), r)
	if got.Get("Authorization") != "" || got.Get("x-api-key") != "gw-key" || got.Get("X-Ukubi-Session") != "sess-1" || got.Get("X-Ukubi-Tag") != "coding" {
		t.Errorf("upstream headers %v", got)
	}
}

func TestRefusesOrigin(t *testing.T) {
	h, _ := setup(t, "k", http.HandlerFunc(func(http.ResponseWriter, *http.Request) { t.Error("reached upstream") }))
	r := httptest.NewRequest("POST", "/v1/messages", nil)
	r.Header.Set("Origin", "https://evil.test")
	w := httptest.NewRecorder()
	h.ServeHTTP(w, r)
	if w.Code != 403 {
		t.Errorf("code %d", w.Code)
	}
}

func TestNoKeyIs503AnthropicError(t *testing.T) {
	h, f := setup(t, "", http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}))
	w := httptest.NewRecorder()
	h.ServeHTTP(w, httptest.NewRequest("POST", "/v1/messages", nil))
	if w.Code != 503 || w.Body.String() != missingKey {
		t.Errorf("%d %s", w.Code, w.Body)
	}
	// Rotation: a key appearing later is picked up without a restart.
	_ = os.WriteFile(f, []byte("late"), 0o600)
	w = httptest.NewRecorder()
	h.ServeHTTP(w, httptest.NewRequest("POST", "/v1/messages", nil))
	if w.Code == 503 {
		t.Error("key written after start still refused")
	}
}

func TestPassesUpstream4xxBody(t *testing.T) {
	h, _ := setup(t, "k", http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(429)
		_, _ = w.Write([]byte(`{"type":"error","error":{"type":"rate_limit_error","message":"budget"}}`))
	}))
	w := httptest.NewRecorder()
	h.ServeHTTP(w, httptest.NewRequest("POST", "/v1/messages", nil))
	if w.Code != 429 || !strings.Contains(w.Body.String(), "budget") {
		t.Errorf("%d %s", w.Code, w.Body)
	}
}

// Each SSE chunk reaches the client before the upstream finishes: no buffering.
func TestStreamsWithoutBuffering(t *testing.T) {
	release := make(chan struct{})
	h, _ := setup(t, "k", http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = w.Write([]byte("data: one\n\n"))
		w.(http.Flusher).Flush()
		<-release
		_, _ = w.Write([]byte("data: two\n\n"))
	}))
	front := httptest.NewServer(h)
	defer front.Close()
	resp, err := http.Post(front.URL+"/v1/messages", "application/json", strings.NewReader("{}"))
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	buf := make([]byte, 64)
	done := make(chan string, 1)
	go func() { n, _ := resp.Body.Read(buf); done <- string(buf[:n]) }()
	select {
	case s := <-done:
		if s != "data: one\n\n" {
			t.Errorf("first chunk %q", s)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("first chunk held back until upstream finished")
	}
	close(release)
	rest, _ := io.ReadAll(resp.Body)
	if string(rest) != "data: two\n\n" {
		t.Errorf("rest %q", rest)
	}
}
