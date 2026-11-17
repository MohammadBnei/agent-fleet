package localapi

import (
	"bufio"
	"context"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"google.golang.org/grpc"
	"google.golang.org/grpc/metadata"

	agentfleetv1 "github.com/MohammadBnei/agent-fleet/proto/gen/go/agentfleet/v1"
	"github.com/MohammadBnei/agent-fleet/sidecar/internal/coreclient"
)

type threadCore struct {
	agentfleetv1.UnimplementedCoreServiceServer
	mu       sync.Mutex
	threads  []string // x-fleet-thread of each SendMessage, in order
	saved    []*agentfleetv1.SaveAgentSessionIdRequest
	events   []string // idempotency keys of PublishEvent
	cards    []string // idempotency keys of DraftCard
	taskResp *agentfleetv1.GetSessionResponse
}

func threadOf(ctx context.Context) string {
	md, _ := metadata.FromIncomingContext(ctx)
	if v := md.Get("x-fleet-thread"); len(v) > 0 {
		return v[0]
	}
	return ""
}

func (f *threadCore) SendMessage(ctx context.Context, _ *agentfleetv1.SendMessageRequest) (*agentfleetv1.AppendResponse, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.threads = append(f.threads, threadOf(ctx))
	return &agentfleetv1.AppendResponse{Seq: 1}, nil
}

func (f *threadCore) SaveAgentSessionId(_ context.Context, r *agentfleetv1.SaveAgentSessionIdRequest) (*agentfleetv1.SaveAgentSessionIdResponse, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.saved = append(f.saved, r)
	return &agentfleetv1.SaveAgentSessionIdResponse{}, nil
}

func (f *threadCore) PublishEvent(_ context.Context, r *agentfleetv1.PublishEventRequest) (*agentfleetv1.PublishEventResponse, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.events = append(f.events, r.GetIdempotencyKey())
	return &agentfleetv1.PublishEventResponse{}, nil
}

func (f *threadCore) DraftCard(_ context.Context, r *agentfleetv1.DraftCardRequest) (*agentfleetv1.DraftCardResponse, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.cards = append(f.cards, r.GetIdempotencyKey())
	return &agentfleetv1.DraftCardResponse{}, nil
}

func (f *threadCore) GetSession(context.Context, *agentfleetv1.GetSessionRequest) (*agentfleetv1.GetSessionResponse, error) {
	return f.taskResp, nil
}

func (f *threadCore) StreamHumanMessages(_ *agentfleetv1.StreamHumanMessagesRequest, s agentfleetv1.CoreService_StreamHumanMessagesServer) error {
	return s.Send(&agentfleetv1.TranscriptEntry{Seq: 3, From: "human", Text: "hi", ThreadKey: "topic-a"})
}

func newThreadRig(t *testing.T) (*threadCore, *coreclient.Client, string) {
	t.Helper()
	lis, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	fake := &threadCore{taskResp: &agentfleetv1.GetSessionResponse{
		Session:          &agentfleetv1.Session{},
		ThreadSessionIds: map[string]string{"topic-a": "sdk-a"},
	}}
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
	return fake, core, srv.URL
}

func post(t *testing.T, url, body string) {
	t.Helper()
	resp, err := http.Post(url, "application/json", strings.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("POST %s %s: HTTP %d", url, body, resp.StatusCode)
	}
}

// docs/adr/0084 §4: once the worker names a turn's thread, every call the sidecar makes carries it; an entry the
// worker files under another thread (a result after its deadline) carries that one; outside a turn none does.
func TestTurn_ThreadRidesOnEveryCall(t *testing.T) {
	fake, _, url := newThreadRig(t)
	msg := `{"from":"agent","text":"x"}`
	post(t, url+"/message", msg) // no turn
	post(t, url+"/turn", `{"thread":"topic-a","seq":7}`)
	post(t, url+"/message", msg)
	post(t, url+"/message", `{"from":"agent","text":"late result","threadKey":"topic-b"}`)
	post(t, url+"/turn", `{"thread":"","seq":0}`)
	post(t, url+"/message", msg)
	fake.mu.Lock()
	defer fake.mu.Unlock()
	if got, want := strings.Join(fake.threads, ","), ",topic-a,topic-b,"; got != want {
		t.Fatalf("threads = %q, want %q", got, want)
	}
}

func TestSessionID_CarriesTheThreadKey(t *testing.T) {
	fake, _, url := newThreadRig(t)
	post(t, url+"/session-id", `{"sessionId":"sdk-a","model":"m","threadKey":"topic-a"}`)
	post(t, url+"/session-id", `{"sessionId":"sdk-main","model":"m"}`)
	fake.mu.Lock()
	defer fake.mu.Unlock()
	if len(fake.saved) != 2 || fake.saved[0].GetThreadKey() != "topic-a" || fake.saved[1].GetThreadKey() != "" {
		t.Fatalf("saved = %v", fake.saved)
	}
}

// Every effect method's key is (session, thread, entry seq, call ordinal) inside a turn, whatever key the agent
// chose, counting up per turn; outside a turn the agent's own key stands.
func TestEffectKeys_PerTurnOrdinals(t *testing.T) {
	fake, core, url := newThreadRig(t)
	ctx := context.Background()
	if _, err := core.PublishEvent(ctx, "t", "s", "x", "agent-key"); err != nil {
		t.Fatal(err)
	}
	post(t, url+"/turn", `{"thread":"topic-a","seq":7}`)
	if _, err := core.PublishEvent(ctx, "t", "s", "x", "agent-key"); err != nil {
		t.Fatal(err)
	}
	if _, err := core.DraftCard(ctx, &agentfleetv1.DraftCardRequest{IdempotencyKey: "agent-key"}); err != nil {
		t.Fatal(err)
	}
	post(t, url+"/turn", `{"thread":"topic-b","seq":9}`)
	if _, err := core.PublishEvent(ctx, "t", "s", "x", ""); err != nil {
		t.Fatal(err)
	}
	fake.mu.Lock()
	defer fake.mu.Unlock()
	if got, want := strings.Join(fake.events, " "), "agent-key eff:s1:topic-a:7:1 eff:s1:topic-b:9:1"; got != want {
		t.Fatalf("publish_event keys = %q, want %q", got, want)
	}
	if len(fake.cards) != 1 || fake.cards[0] != "eff:s1:topic-a:7:2" {
		t.Fatalf("draft_card keys = %v, want eff:s1:topic-a:7:2 (the second effect of the turn)", fake.cards)
	}
}

func TestTaskAndFeed_CarryThreads(t *testing.T) {
	_, _, url := newThreadRig(t)
	resp, err := http.Get(url + "/task")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = resp.Body.Close() }()
	var sb strings.Builder
	sc := bufio.NewScanner(resp.Body)
	for sc.Scan() {
		sb.WriteString(sc.Text())
	}
	if !strings.Contains(sb.String(), `"threadSessionIds":{"topic-a":"sdk-a"}`) {
		t.Fatalf("/task = %s", sb.String())
	}
	feed, err := http.Get(url + "/human-messages?sinceSeq=0")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = feed.Body.Close() }()
	sc = bufio.NewScanner(feed.Body)
	for sc.Scan() {
		if strings.HasPrefix(sc.Text(), "data: ") {
			if !strings.Contains(sc.Text(), `"threadKey":"topic-a"`) {
				t.Fatalf("feed entry = %s", sc.Text())
			}
			return
		}
	}
	t.Fatal("no entry on the feed")
}
