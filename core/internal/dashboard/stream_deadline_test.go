package dashboard

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"connectrpc.com/connect"

	agentfleetv1 "github.com/MohammadBnei/agent-fleet/proto/gen/go/agentfleet/v1"
	"github.com/MohammadBnei/agent-fleet/proto/gen/go/agentfleet/v1/agentfleetv1connect"
)

// streamTestClient stands up a real Connect handler over httptest against the
// given hub. StreamTranscript touches nothing on Server but hub, so the rest
// stays nil deliberately — and a real round trip is the only way to observe
// what this change is actually about: whether the stream ends *cleanly* or as
// an error. A hand-rolled fake stream would return whatever we told it to.
func streamTestClient(t *testing.T, hub *Hub) agentfleetv1connect.DashboardServiceClient {
	t.Helper()
	path, handler := agentfleetv1connect.NewDashboardServiceHandler(&Server{hub: hub})
	mux := http.NewServeMux()
	mux.Handle(path, handler)
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	return agentfleetv1connect.NewDashboardServiceClient(srv.Client(), srv.URL)
}

// An idle session sends nothing for as long as nobody types — hours, if the
// agent is thinking or blocked on a permission decision. Cloudflare's free plan
// answers 100s of origin silence with a 524, and that single fact is why
// fleet.bnei.dev is the one *.bnei.dev host still DNS-only: no WAF, no geo
// rules, no origin lock, public origin IP.
//
// The fix is for the server to end the response first. This asserts it does,
// and that it does so cleanly.
func TestStreamTranscript_EndsBeforeCloudflareWouldTimeOut(t *testing.T) {
	restore := streamMaxAge
	streamMaxAge = 150 * time.Millisecond
	t.Cleanup(func() { streamMaxAge = restore })

	client := streamTestClient(t, NewHub())
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	start := time.Now()
	stream, err := client.StreamTranscript(ctx, connect.NewRequest(&agentfleetv1.StreamTranscriptRequest{SessionId: "silent-session"}))
	if err != nil {
		t.Fatalf("StreamTranscript: %v", err)
	}
	defer func() { _ = stream.Close() }()

	for stream.Receive() {
		t.Fatalf("received an entry on a session with no transcript activity: %+v", stream.Msg())
	}

	// THE assertion. subscribeTranscript resubscribes from its cursor on a
	// clean end as well as on an error, so delivery is safe either way — but a
	// clean end is not a failure and must not be reported as one. Every idle
	// viewer would otherwise generate an RPC error every 90 seconds, in the
	// access log and in anything alerting on it.
	if err := stream.Err(); err != nil {
		t.Errorf("stream ended with error %v, want a clean end", err)
	}
	if elapsed := time.Since(start); elapsed > 3*time.Second {
		t.Errorf("stream stayed open %v; streamMaxAge was %v", elapsed, streamMaxAge)
	}
}

// The deadline must not cost entries. Ending a stream early is only safe
// because the client resumes from its own cursor, so entries still have to be
// delivered normally while it is open.
func TestStreamTranscript_StillDeliversEntries(t *testing.T) {
	restore := streamMaxAge
	streamMaxAge = 3 * time.Second
	t.Cleanup(func() { streamMaxAge = restore })

	hub := NewHub()
	client := streamTestClient(t, hub)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	// This goroutine must start BEFORE the call below. client.StreamTranscript
	// does not return until the server sends its first message — the handler
	// writes no response headers until then — so anything sequenced after it
	// runs only once an entry has already been delivered. Broadcasting from
	// there would deadlock against the deadline every time, which is exactly
	// how this test first failed.
	//
	// It repeats because broadcast drops rather than queues when no subscriber
	// is attached yet, and the handler subscribes some microseconds after the
	// request lands.
	//
	// seq 0 is a REAL entry: Append assigns COALESCE(MAX(seq), -1) + 1, so the
	// first row of every session is 0, not 1. Pinned here because that is
	// precisely what rules 0 out as a sentinel for anyone later tempted to
	// solve the timeout with a keepalive frame instead of this deadline.
	stop := make(chan struct{})
	defer close(stop)
	go func() {
		ticker := time.NewTicker(10 * time.Millisecond)
		defer ticker.Stop()
		for {
			select {
			case <-stop:
				return
			case <-ticker.C:
				hub.broadcast("chatty", &agentfleetv1.TranscriptEntry{SessionId: "chatty", Seq: 0, Text: "first"})
			}
		}
	}()

	stream, err := client.StreamTranscript(ctx, connect.NewRequest(&agentfleetv1.StreamTranscriptRequest{SessionId: "chatty"}))
	if err != nil {
		t.Fatalf("StreamTranscript: %v", err)
	}
	defer func() { _ = stream.Close() }()

	if !stream.Receive() {
		t.Fatalf("no entry delivered while the stream was open: %v", stream.Err())
	}
	if got := stream.Msg(); got.GetSeq() != 0 || got.GetText() != "first" {
		t.Errorf("got %+v, want seq 0 text %q", got, "first")
	}
}
