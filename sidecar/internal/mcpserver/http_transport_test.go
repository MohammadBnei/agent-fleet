package mcpserver

import (
	"bufio"
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/MohammadBnei/agent-fleet/sidecar/internal/coreclient"
)

// The other tests in this package call handlers directly, so none of them
// would notice the transport changing under them. mcp-go v1 changed it twice
// over: a newer spec version to negotiate, and a DNS-rebinding guard that
// answers 403 to a loopback connection whose Host is not a loopback name. The
// worker dials http://127.0.0.1:<port>/mcp (provisioner's SIDECAR_MCP_ADDR),
// so these drive the real handler over real HTTP the way that client does.

// postRPC sends one JSON-RPC message and returns the decoded response body
// (from a JSON or an SSE reply) along with the HTTP response.
func postRPC(t *testing.T, url, host, sessionID string, msg map[string]any) (*http.Response, map[string]any) {
	t.Helper()
	body, _ := json.Marshal(msg)
	req, _ := http.NewRequest(http.MethodPost, url+"/mcp", bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json, text/event-stream")
	if host != "" {
		req.Host = host
	}
	if sessionID != "" {
		req.Header.Set("Mcp-Session-Id", sessionID)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("POST %v: %v", msg["method"], err)
	}
	defer func() { _ = resp.Body.Close() }()
	raw, _ := io.ReadAll(resp.Body)
	if strings.HasPrefix(resp.Header.Get("Content-Type"), "text/event-stream") {
		sc := bufio.NewScanner(bytes.NewReader(raw))
		raw = nil
		for sc.Scan() {
			if data, ok := strings.CutPrefix(sc.Text(), "data:"); ok {
				raw = []byte(strings.TrimSpace(data))
			}
		}
	}
	var out map[string]any
	if len(raw) > 0 && resp.StatusCode < 400 {
		if err := json.Unmarshal(raw, &out); err != nil {
			t.Fatalf("%v: undecodable reply %q: %v", msg["method"], raw, err)
		}
	}
	return resp, out
}

func newHTTPSidecar(t *testing.T) *httptest.Server {
	t.Helper()
	// grpc.NewClient does not dial, so a dead address is fine as long as no
	// call reaches core: a gRPC call to it waits for a connection with no
	// deadline, which is a hung test, not a failed one.
	core, err := coreclient.New("127.0.0.1:1", "session-test", "lease-test")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = core.Close() })
	srv := httptest.NewServer(New(core))
	t.Cleanup(srv.Close)
	return srv
}

func TestHTTP_LoopbackClientListsAndCallsTools(t *testing.T) {
	srv := newHTTPSidecar(t)

	// An older protocol version than mcp-go's newest: the CLI the worker ships
	// is not guaranteed to speak the latest spec, and the server must
	// negotiate down rather than refuse.
	resp, init := postRPC(t, srv.URL, "", "", map[string]any{
		"jsonrpc": "2.0", "id": 1, "method": "initialize",
		"params": map[string]any{
			"protocolVersion": "2025-06-18",
			"capabilities":    map[string]any{},
			"clientInfo":      map[string]any{"name": "test", "version": "0"},
		},
	})
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("initialize: HTTP %d %v", resp.StatusCode, init)
	}
	result, _ := init["result"].(map[string]any)
	if got := result["protocolVersion"]; got != "2025-06-18" {
		t.Fatalf("negotiated protocolVersion = %v, want the client's 2025-06-18", got)
	}
	sid := resp.Header.Get("Mcp-Session-Id")
	postRPC(t, srv.URL, "", sid, map[string]any{"jsonrpc": "2.0", "method": "notifications/initialized"})

	resp, list := postRPC(t, srv.URL, "", sid, map[string]any{"jsonrpc": "2.0", "id": 2, "method": "tools/list"})
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("tools/list: HTTP %d", resp.StatusCode)
	}
	tools, _ := list["result"].(map[string]any)["tools"].([]any)
	names := map[string]bool{}
	for _, tl := range tools {
		names[tl.(map[string]any)["name"].(string)] = true
	}
	for _, want := range []string{"AskUserQuestion", "send_message", "expose"} {
		if !names[want] {
			t.Errorf("tools/list over HTTP is missing %q (got %v)", want, names)
		}
	}

	// A call must reach the handler. Missing `text` fails the handler's own
	// check before it touches core, so the right answer is a tool-level error
	// result carrying the handler's message, not a transport or JSON-RPC error.
	resp, call := postRPC(t, srv.URL, "", sid, map[string]any{
		"jsonrpc": "2.0", "id": 3, "method": "tools/call",
		"params": map[string]any{"name": "send_message", "arguments": map[string]any{"from": "agent"}},
	})
	if resp.StatusCode != http.StatusOK || call["error"] != nil {
		t.Fatalf("tools/call: HTTP %d, error %v", resp.StatusCode, call["error"])
	}
	res, _ := call["result"].(map[string]any)
	if isErr, _ := res["isError"].(bool); !isErr || !strings.Contains(fmt.Sprint(res["content"]), "from and text are required") {
		t.Fatalf("tools/call did not come back from send_message's handler: %v", res)
	}
}

func TestHTTP_LoopbackHostHeaderIsAccepted(t *testing.T) {
	srv := newHTTPSidecar(t)
	initMsg := map[string]any{
		"jsonrpc": "2.0", "id": 1, "method": "initialize",
		"params": map[string]any{
			"protocolVersion": "2025-06-18", "capabilities": map[string]any{},
			"clientInfo": map[string]any{"name": "test", "version": "0"},
		},
	}
	// Exactly what the worker sends: Host is the dialled 127.0.0.1:<port>.
	if resp, _ := postRPC(t, srv.URL, "127.0.0.1:9090", "", initMsg); resp.StatusCode != http.StatusOK {
		t.Fatalf("Host 127.0.0.1:9090 got HTTP %d; the worker's own requests would be refused", resp.StatusCode)
	}
	// The guard itself: a rebinding-shaped Host on a loopback connection.
	if resp, _ := postRPC(t, srv.URL, "attacker.example", "", initMsg); resp.StatusCode != http.StatusForbidden {
		t.Fatalf("Host attacker.example got HTTP %d, want 403 from mcp-go's DNS-rebinding guard", resp.StatusCode)
	}
}
