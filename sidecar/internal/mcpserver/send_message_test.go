package mcpserver

import (
	"context"
	"net"
	"sync"
	"testing"

	"github.com/mark3labs/mcp-go/mcp"
	"google.golang.org/grpc"

	agentfleetv1 "github.com/MohammadBnei/agent-fleet/proto/gen/go/agentfleet/v1"
	"github.com/MohammadBnei/agent-fleet/sidecar/internal/coreclient"
)

type sendRecorder struct {
	agentfleetv1.UnimplementedCoreServiceServer
	mu   sync.Mutex
	from string
}

func (f *sendRecorder) SendMessage(_ context.Context, req *agentfleetv1.SendMessageRequest) (*agentfleetv1.AppendResponse, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.from = req.GetFrom()
	return &agentfleetv1.AppendResponse{Seq: 1}, nil
}

// docs/adr/0064: core refuses a pod authoring as anyone but agent/worker (the
// worker obeys human and session entries), so the tool no longer takes a
// `from` the agent could set to "human": it stamps "agent".
func TestSendMessage_StampsAgent(t *testing.T) {
	for _, tl := range newMCPServer(nil).ListTools() {
		if tl.Tool.Name == "send_message" {
			if _, ok := tl.Tool.InputSchema.Properties["from"]; ok {
				t.Fatal("send_message still advertises a from parameter")
			}
		}
	}

	lis, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	fake := &sendRecorder{}
	gs := grpc.NewServer()
	agentfleetv1.RegisterCoreServiceServer(gs, fake)
	go func() { _ = gs.Serve(lis) }()
	t.Cleanup(gs.Stop)
	core, err := coreclient.New(lis.Addr().String(), "s1", "l1")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = core.Close() })

	req := mcp.CallToolRequest{}
	req.Params.Name = "send_message"
	req.Params.Arguments = map[string]any{"from": "human", "text": "hi"}
	res, err := sendMessageHandler(core)(context.Background(), req)
	if err != nil || res.IsError {
		t.Fatalf("send_message: %v %+v", err, res)
	}
	fake.mu.Lock()
	defer fake.mu.Unlock()
	if fake.from != "agent" {
		t.Fatalf("from = %q, want agent", fake.from)
	}
}
