package localapi

import (
	"bufio"
	"context"
	"encoding/json"
	"net"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"

	"google.golang.org/grpc"
	"google.golang.org/protobuf/proto"

	agentfleetv1 "github.com/MohammadBnei/agent-fleet/proto/gen/go/agentfleet/v1"
	"github.com/MohammadBnei/agent-fleet/sidecar/internal/coreclient"
)

type specRecorder struct {
	agentfleetv1.UnimplementedCoreServiceServer
	mu                 sync.Mutex
	streamSHA, waitSHA string
}

func (f *specRecorder) GetSession(context.Context, *agentfleetv1.GetSessionRequest) (*agentfleetv1.GetSessionResponse, error) {
	return &agentfleetv1.GetSessionResponse{
		Session:        &agentfleetv1.Session{Id: "s1", Effort: proto.String("high"), DefinitionSha: proto.String("d3f")},
		AgentSpec:      &agentfleetv1.AgentSpec{ToolsAllow: []string{"Read"}, SidecarTools: []string{"send_message"}, MaxTurns: 60},
		AgentSpecSha:   "abc",
		DecisionWaitMs: 45000,
	}, nil
}

func (f *specRecorder) StreamHumanMessages(req *agentfleetv1.StreamHumanMessagesRequest, st agentfleetv1.CoreService_StreamHumanMessagesServer) error {
	f.mu.Lock()
	f.streamSHA = req.GetAgentSpecSha()
	f.mu.Unlock()
	return st.Send(&agentfleetv1.TranscriptEntry{Seq: 1, From: "human", Text: "hi"})
}

func (f *specRecorder) WaitForMessages(_ context.Context, req *agentfleetv1.ReadTranscriptSinceRequest) (*agentfleetv1.ReadTranscriptSinceResponse, error) {
	f.mu.Lock()
	f.waitSHA = req.GetAgentSpecSha()
	f.mu.Unlock()
	return &agentfleetv1.ReadTranscriptSinceResponse{}, nil
}

// docs/adr/0066 §8: /task hands the worker its spec and sha, the sha the
// worker passes on /human-messages reaches core on the stream, and the agent's
// wait_for_messages (no worker in that path) carries the same registered sha.
func TestAgentSpec_ShaFlowsFromTheWorkerToCore(t *testing.T) {
	lis, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	fake := &specRecorder{}
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

	resp, err := http.Get(srv.URL + "/task")
	if err != nil {
		t.Fatal(err)
	}
	var task struct {
		AgentSpec    map[string]any `json:"agentSpec"`
		AgentSpecSha string         `json:"agentSpecSha"`
		Effort       string         `json:"effort"`
		DefSha       string         `json:"definitionSha"`
		DecisionWait int            `json:"decisionWaitMs"`
	}
	err = json.NewDecoder(resp.Body).Decode(&task)
	_ = resp.Body.Close()
	if err != nil {
		t.Fatal(err)
	}
	if task.AgentSpecSha != "abc" || task.AgentSpec["maxTurns"] != float64(60) || task.AgentSpec["toolsDeny"] == nil {
		t.Fatalf("/task: %+v, want the spec in camelCase with every field and its sha", task)
	}
	// docs/adr/0077: the row's effort reaches the worker.
	if task.Effort != "high" {
		t.Fatalf("/task effort %q, want high", task.Effort)
	}
	// docs/adr/0082 §5: core's DECISION_WAIT_MS reaches the worker's gate.
	if task.DecisionWait != 45000 {
		t.Fatalf("/task decisionWaitMs %d, want 45000", task.DecisionWait)
	}
	if task.DefSha != "d3f" {
		t.Fatalf("/task definitionSha %q, want the pinned commit", task.DefSha)
	}

	resp, err = http.Get(srv.URL + "/human-messages?sinceSeq=0&agentSpecSha=abc")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := bufio.NewReader(resp.Body).ReadString('\n'); err != nil {
		t.Fatal(err)
	}
	_ = resp.Body.Close()
	if _, _, err := core.WaitForMessages(context.Background(), 0, 1); err != nil {
		t.Fatal(err)
	}
	fake.mu.Lock()
	defer fake.mu.Unlock()
	if fake.streamSHA != "abc" || fake.waitSHA != "abc" {
		t.Fatalf("sha sent to core: stream %q, wait %q; want abc for both", fake.streamSHA, fake.waitSHA)
	}
}
