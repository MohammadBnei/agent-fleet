package localapi

import (
	"context"
	"encoding/json"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	agentfleetv1 "github.com/MohammadBnei/agent-fleet/proto/gen/go/agentfleet/v1"
	"github.com/MohammadBnei/agent-fleet/sidecar/internal/coreclient"
)

type grantRecorder struct {
	agentfleetv1.UnimplementedCoreServiceServer
	mu  sync.Mutex
	req *agentfleetv1.ConsumeGrantRequest
}

func (f *grantRecorder) ConsumeGrant(_ context.Context, req *agentfleetv1.ConsumeGrantRequest) (*agentfleetv1.ConsumeGrantResponse, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if req.GetTool() == "NoCanon" {
		return nil, status.Error(codes.InvalidArgument, "input_json: not valid UTF-8")
	}
	f.req = req
	return &agentfleetv1.ConsumeGrantResponse{Granted: true, DecisionId: "d1"}, nil
}

// /grant/consume spends a grant of the sidecar's own session, whatever the
// body says, and passes the input on as the worker sent it.
func TestGrantConsume(t *testing.T) {
	lis, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	fake := &grantRecorder{}
	gs := grpc.NewServer()
	agentfleetv1.RegisterCoreServiceServer(gs, fake)
	go func() { _ = gs.Serve(lis) }()
	t.Cleanup(gs.Stop)
	core, err := coreclient.New(lis.Addr().String(), "s1", "lease")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = core.Close() })
	srv := httptest.NewServer(New(core))
	t.Cleanup(srv.Close)

	post := func(body string) *http.Response {
		resp, err := http.Post(srv.URL+"/grant/consume", "application/json", strings.NewReader(body))
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = resp.Body.Close() })
		return resp
	}
	resp := post(`{"tool":"Bash","input":{"command":"ls"},"sessionId":"s2"}`)
	var got struct {
		Granted    bool   `json:"granted"`
		DecisionID string `json:"decisionId"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&got); err != nil || resp.StatusCode != http.StatusOK || !got.Granted || got.DecisionID != "d1" {
		t.Fatalf("HTTP %d, %+v, %v", resp.StatusCode, got, err)
	}
	fake.mu.Lock()
	r := fake.req
	fake.mu.Unlock()
	if r.GetSessionId() != "s1" || r.GetTool() != "Bash" || r.GetInputJson() != `{"command":"ls"}` {
		t.Errorf("ConsumeGrant got %v", r)
	}
	// An input with no canon is never granted: not granted, not an error.
	resp = post(`{"tool":"NoCanon","input":{}}`)
	got.Granted, got.DecisionID = true, "x"
	if err := json.NewDecoder(resp.Body).Decode(&got); err != nil || resp.StatusCode != http.StatusOK || got.Granted || got.DecisionID != "" {
		t.Errorf("no canon: HTTP %d, %+v, %v; want 200 and not granted", resp.StatusCode, got, err)
	}
	if resp := post(`{"tool":"Bash"}`); resp.StatusCode != http.StatusBadRequest {
		t.Errorf("no input: HTTP %d", resp.StatusCode)
	}
}
