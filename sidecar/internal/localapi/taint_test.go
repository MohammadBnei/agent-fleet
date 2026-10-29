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

	agentfleetv1 "github.com/MohammadBnei/agent-fleet/proto/gen/go/agentfleet/v1"
	"github.com/MohammadBnei/agent-fleet/sidecar/internal/coreclient"
)

type taintRecorder struct {
	agentfleetv1.UnimplementedCoreServiceServer
	mu   sync.Mutex
	reqs []*agentfleetv1.TaintRequest
}

func (f *taintRecorder) Taint(_ context.Context, req *agentfleetv1.TaintRequest) (*agentfleetv1.TaintResponse, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.reqs = append(f.reqs, req)
	return &agentfleetv1.TaintResponse{Tainted: req.GetOutsideRead() != ""}, nil
}

// docs/adr/0082 §4: POST /taint names this pod's own session, passes the
// outside read on, and hands core's answer back. A body asking to clear taint
// has no field to land in.
func TestTaint_OwnSessionAndCoresAnswer(t *testing.T) {
	lis, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	fake := &taintRecorder{}
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

	post := func(body string) bool {
		t.Helper()
		resp, err := http.Post(srv.URL+"/taint", "application/json", strings.NewReader(body))
		if err != nil {
			t.Fatal(err)
		}
		defer func() { _ = resp.Body.Close() }()
		if resp.StatusCode != http.StatusOK {
			t.Fatalf("HTTP %d", resp.StatusCode)
		}
		var out struct{ Tainted bool }
		if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
			t.Fatal(err)
		}
		return out.Tainted
	}
	if !post(`{"outsideRead":"curl x"}`) || post(`{"tainted":false,"sessionId":"other"}`) {
		t.Fatal("core's answer was not passed back")
	}
	fake.mu.Lock()
	defer fake.mu.Unlock()
	if len(fake.reqs) != 2 || fake.reqs[0].GetSessionId() != "s1" || fake.reqs[0].GetOutsideRead() != "curl x" ||
		fake.reqs[1].GetSessionId() != "s1" || fake.reqs[1].GetOutsideRead() != "" {
		t.Fatalf("requests: %v", fake.reqs)
	}
}
