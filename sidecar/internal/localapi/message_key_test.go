package localapi

import (
	"context"
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

type messageRecorder struct {
	agentfleetv1.UnimplementedCoreServiceServer
	mu   sync.Mutex
	reqs []*agentfleetv1.SendMessageRequest
}

func (f *messageRecorder) SendMessage(_ context.Context, req *agentfleetv1.SendMessageRequest) (*agentfleetv1.AppendResponse, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.reqs = append(f.reqs, req)
	return &agentfleetv1.AppendResponse{Seq: int64(len(f.reqs))}, nil
}

// POST /message passes idempotencyKey to core, with and without a replyTo
// (docs/adr/0070 §3).
func TestMessage_IdempotencyKeyReachesCore(t *testing.T) {
	lis, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	fake := &messageRecorder{}
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

	for _, body := range []string{
		`{"from":"agent","text":"r","type":"result","idempotencyKey":"result:s1:4:1"}`,
		`{"from":"agent","text":"r","type":"result","replyTo":2,"idempotencyKey":"result:s1:4:2"}`,
		`{"from":"agent","text":"r","type":"result"}`,
	} {
		resp, err := http.Post(srv.URL+"/message", "application/json", strings.NewReader(body))
		if err != nil {
			t.Fatal(err)
		}
		_ = resp.Body.Close()
		if resp.StatusCode != http.StatusOK {
			t.Fatalf("HTTP %d for %s", resp.StatusCode, body)
		}
	}
	// Any key that is not this session's result key is refused.
	for _, k := range []string{"result:other:4:1", "stale-decision-s1-3", "bridge:x:y"} {
		resp, err := http.Post(srv.URL+"/message", "application/json",
			strings.NewReader(`{"from":"agent","text":"r","idempotencyKey":"`+k+`"}`))
		if err != nil {
			t.Fatal(err)
		}
		_ = resp.Body.Close()
		if resp.StatusCode != http.StatusBadRequest {
			t.Fatalf("key %q: HTTP %d, want 400", k, resp.StatusCode)
		}
	}
	fake.mu.Lock()
	defer fake.mu.Unlock()
	want := []string{"result:s1:4:1", "result:s1:4:2", ""}
	if len(fake.reqs) != len(want) {
		t.Fatalf("got %d requests", len(fake.reqs))
	}
	for i, w := range want {
		if got := fake.reqs[i].GetIdempotencyKey(); got != w {
			t.Errorf("request %d key = %q, want %q", i, got, w)
		}
	}
}
