// Package gateway is the sidecar's loopback route to llm-gateway (docs/adr/0088):
// the worker's ANTHROPIC_BASE_URL points here, and the sidecar, not the worker,
// holds the key. Requests are streamed through unbuffered.
package gateway

import (
	"net/http"
	"net/http/httputil"
	"net/url"
	"os"
	"strings"
	"time"
)

const missingKey = `{"type":"error","error":{"type":"api_error","message":"fleet gateway key not provisioned for this session"}}`

// New proxies to upstream. keyFile is read on every request; sessionID is
// stamped over whatever identity the client sent.
func New(upstream *url.URL, keyFile, sessionID string) http.Handler {
	rp := &httputil.ReverseProxy{Rewrite: func(pr *httputil.ProxyRequest) { pr.SetURL(upstream) }}
	rp.FlushInterval = -1 // SSE: every chunk goes out as it arrives
	rp.Transport = &http.Transport{
		ResponseHeaderTimeout: 10 * time.Minute, // a long first token; the body streams for as long as it needs
		IdleConnTimeout:       90 * time.Second,
		DisableCompression:    true,
	}
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// A browser page cannot reach this loopback listener's key.
		if r.Header.Get("Origin") != "" {
			http.Error(w, "forbidden", http.StatusForbidden)
			return
		}
		b, err := os.ReadFile(keyFile)
		key := strings.TrimSpace(string(b))
		if keyFile == "" || err != nil || key == "" {
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusServiceUnavailable)
			_, _ = w.Write([]byte(missingKey))
			return
		}
		r.Header.Del("Authorization")
		r.Header.Set("x-api-key", key)
		r.Header.Set("X-Ukubi-Session", sessionID)
		r.Header.Set("X-Ukubi-Tag", "coding")
		rp.ServeHTTP(w, r)
	})
}
