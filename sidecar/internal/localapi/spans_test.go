package localapi

import (
	"context"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"google.golang.org/grpc"

	agentfleetv1 "github.com/MohammadBnei/agent-fleet/proto/gen/go/agentfleet/v1"
	"github.com/MohammadBnei/agent-fleet/sidecar/internal/coreclient"
)

type spanRecorder struct {
	agentfleetv1.UnimplementedCoreServiceServer
	mu  sync.Mutex
	req *agentfleetv1.ExportSpansRequest
}

func (f *spanRecorder) ExportSpans(_ context.Context, req *agentfleetv1.ExportSpansRequest) (*agentfleetv1.ExportSpansResponse, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.req = req
	return &agentfleetv1.ExportSpansResponse{Accepted: int32(len(req.GetSpans()))}, nil
}

func spansEnv(t *testing.T) (*httptest.Server, *spanRecorder, *grpc.Server) {
	t.Helper()
	lis, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	fake := &spanRecorder{}
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
	return srv, fake, gs
}

const spansBody = `{"spans":[{"name":"execute_tool Bash","id":"t1","parent":"a1","turn":"u1","startNs":1700000000000000000,"endNs":1700000001000000000,"status":"error",
"attrs":{"gen_ai.tool.name":"Bash","gen_ai.usage.input_tokens":42,"error":true}}]}`

// /spans forwards the worker's batch under the sidecar's own session, mapping
// JSON attributes onto the string and int maps.
func TestSpans_ForwardsUnderTheOwnSession(t *testing.T) {
	srv, fake, _ := spansEnv(t)
	resp, err := http.Post(srv.URL+"/spans", "application/json", strings.NewReader(spansBody))
	if err != nil {
		t.Fatal(err)
	}
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("HTTP %d", resp.StatusCode)
	}
	fake.mu.Lock()
	r := fake.req
	fake.mu.Unlock()
	if r.GetSessionId() != "s1" || len(r.GetSpans()) != 1 {
		t.Fatalf("got %v", r)
	}
	s := r.GetSpans()[0]
	if s.GetName() != "execute_tool Bash" || s.GetLocalId() != "t1" || s.GetParentLocalId() != "a1" || s.GetTurnKey() != "u1" ||
		s.GetStartUnixNano() != 1700000000000000000 || s.GetEndUnixNano() != 1700000001000000000 ||
		s.GetStatus() != agentfleetv1.WorkerSpanStatus_WORKER_SPAN_STATUS_ERROR {
		t.Errorf("span = %v", s)
	}
	if s.GetAttributes()["gen_ai.tool.name"] != "Bash" || s.GetAttributes()["error"] != "true" || s.GetIntAttributes()["gen_ai.usage.input_tokens"] != 42 {
		t.Errorf("attributes = %v %v", s.GetAttributes(), s.GetIntAttributes())
	}
}

// Core down: the batch is dropped with a 502 inside the timeout, never held.
func TestSpans_DropsWhenCoreIsDown(t *testing.T) {
	srv, _, gs := spansEnv(t)
	gs.Stop()
	start := time.Now()
	resp, err := http.Post(srv.URL+"/spans", "application/json", strings.NewReader(spansBody))
	if err != nil {
		t.Fatal(err)
	}
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusBadGateway {
		t.Fatalf("HTTP %d, want 502", resp.StatusCode)
	}
	if d := time.Since(start); d > spansTimeout+2*time.Second {
		t.Fatalf("took %v", d)
	}
}

func TestSpans_RejectsBadJSON(t *testing.T) {
	srv, _, _ := spansEnv(t)
	resp, err := http.Post(srv.URL+"/spans", "application/json", strings.NewReader("{"))
	if err != nil {
		t.Fatal(err)
	}
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("HTTP %d", resp.StatusCode)
	}
}
