package agentsvc

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"connectrpc.com/connect"

	"github.com/MohammadBnei/agent-fleet/core/internal/auth"
	agentfleetv1 "github.com/MohammadBnei/agent-fleet/proto/gen/go/agentfleet/v1"
	"github.com/MohammadBnei/agent-fleet/proto/gen/go/agentfleet/v1/agentfleetv1connect"
)

// docs/adr/0084 §7: a surface (the game's admin origin) may talk to and hear characters, and still nothing else.
func TestSurfaceReachesTheGameChannel(t *testing.T) {
	surface := Principal{Member: &auth.Identity{Sub: "a", Groups: []string{auth.RequiredGroup}}, Origin: "https://game.bnei.dev", Surface: true}
	for _, proc := range []string{agentfleetv1connect.AgentServicePostToCharacterProcedure, agentfleetv1connect.AgentServiceWatchSpeechProcedure} {
		if !allowed(proc, surface) {
			t.Errorf("a surface is refused %s", proc)
		}
		if allowed(proc, Principal{Client: "bot"}) {
			t.Errorf("a client may call %s", proc)
		}
	}
	if allowed(agentfleetv1connect.AgentServicePostMessageProcedure, surface) {
		t.Error("a surface posts to a thread")
	}
}

// Only the owner (an admin, on the console or an admin origin) talks to or hears a character: a member, and an admin
// on an island, are refused before anything is read.
func TestGameChannelIsTheOwnersAlone(t *testing.T) {
	s := &Server{speech: newSpeechHub()}
	admin := auth.Identity{Sub: "a", Groups: []string{auth.RequiredGroup}}
	for name, p := range map[string]Principal{
		"member":          {Member: &auth.Identity{Sub: "m", Groups: []string{"notes"}}},
		"admin on island": {Member: &admin, Origin: "https://notes.example"},
		"client":          {Client: "bot"},
	} {
		ctx := withPrincipal(context.Background(), p)
		_, err := s.PostToCharacter(ctx, connect.NewRequest(&agentfleetv1.PostToCharacterRequest{Character: "ops", Text: "hi"}))
		if connect.CodeOf(err) != connect.CodePermissionDenied {
			t.Errorf("%s PostToCharacter: %v", name, err)
		}
		if err := s.WatchSpeech(ctx, connect.NewRequest(&agentfleetv1.WatchSpeechRequest{}), nil); connect.CodeOf(err) != connect.CodePermissionDenied {
			t.Errorf("%s WatchSpeech: %v", name, err)
		}
	}
	ctx := withPrincipal(context.Background(), Principal{Member: &admin})
	for _, r := range []*agentfleetv1.PostToCharacterRequest{
		{Character: "ops"},
		{Character: "ops", Text: strings.Repeat("x", maxText+1)},
		{Character: "ops", Text: "hi", ThreadKey: "bad key"},
	} {
		if _, err := s.PostToCharacter(ctx, connect.NewRequest(r)); connect.CodeOf(err) != connect.CodeInvalidArgument {
			t.Errorf("PostToCharacter %+v: %v", r, err)
		}
	}
}

// docs/adr/0084 §7: a line is bounded in size, in its face and in count per turn; a replayed key is one line; the
// next turn counts afresh; with nobody watching it is dropped and says so.
func TestSpeak_BoundsAndDelivery(t *testing.T) {
	ctx := context.Background()
	s := &Server{speech: newSpeechHub()}
	key := func(seq, ord int) string { return fmt.Sprintf("eff:c1:main:%d:%d", seq, ord) }
	for name, c := range map[string]struct{ text, face, key string }{
		"empty":         {"", "", key(1, 1)},
		"too long":      {strings.Repeat("x", maxSpeakText+1), "", key(1, 1)},
		"bad face":      {"hi", "Happy!", key(1, 1)},
		"long face":     {"hi", strings.Repeat("a", maxFace+1), key(1, 1)},
		"outside turn":  {"hi", "", "agent-key"},
		"other session": {"hi", "", "eff:c2:main:1:1"},
	} {
		if _, err := s.Speak(ctx, "c1", c.text, c.face, c.key); connect.CodeOf(err) != connect.CodeInvalidArgument {
			t.Errorf("%s: %v", name, err)
		}
	}

	lines, cancel := s.speech.subscribe()
	defer cancel()
	line := &agentfleetv1.WatchSpeechResponse{Character: "ops", Text: "hi", Face: "happy"}
	for i := 1; i <= maxSpeakPerTurn; i++ {
		if d, err := s.speech.say("c1", "main:1", key(1, i), line); err != nil || !d {
			t.Fatalf("line %d: %v %v", i, d, err)
		}
	}
	if len(lines) != maxSpeakPerTurn {
		t.Fatalf("%d lines reached the browser, want %d", len(lines), maxSpeakPerTurn)
	}
	if _, err := s.speech.say("c1", "main:1", key(1, maxSpeakPerTurn+1), line); connect.CodeOf(err) != connect.CodeResourceExhausted {
		t.Errorf("the 21st line of a turn: %v", err)
	}
	if d, err := s.speech.say("c1", "main:1", key(1, 3), line); err != nil || !d || len(lines) != maxSpeakPerTurn {
		t.Errorf("a replayed key: %v %v, %d queued", d, err, len(lines))
	}
	if _, err := s.speech.say("c1", "main:2", key(2, 1), line); err != nil {
		t.Errorf("the next turn counts afresh: %v", err)
	}
	if _, err := s.speech.say("c2", "main:1", "eff:c2:main:1:1", line); err != nil {
		t.Errorf("another character has its own count: %v", err)
	}
	cancel()
	if d, err := s.speech.say("c1", "main:2", key(2, 2), line); err != nil || d {
		t.Errorf("nobody watching: delivered %v, %v", d, err)
	}
}

// A character's own sidecar names the turn, so changing the turn over and over must not lift the bound.
func TestSpeak_BoundHoldsAcrossTurns(t *testing.T) {
	h := newSpeechHub()
	line := &agentfleetv1.WatchSpeechResponse{Text: "hi"}
	var err error
	n := 0
	for ; n < 2*maxSpeakPerWin && err == nil; n++ {
		_, err = h.say("c1", fmt.Sprintf("main:%d", n), fmt.Sprintf("eff:c1:main:%d:1", n), line)
	}
	if connect.CodeOf(err) != connect.CodeResourceExhausted || n != maxSpeakPerWin+1 {
		t.Fatalf("stopped at %d with %v, want the %dth line refused", n, err, maxSpeakPerWin+1)
	}
	if _, err := h.say("c2", "main:1", "eff:c2:main:1:1", line); err != nil {
		t.Errorf("another character has its own cap: %v", err)
	}
}

// A replay reports what the first try did: a line nobody saw is not "delivered" because a browser came after.
func TestSpeak_ReplayKeepsFirstOutcome(t *testing.T) {
	h := newSpeechHub()
	line := &agentfleetv1.WatchSpeechResponse{Text: "hi"}
	if d, err := h.say("c1", "main:1", "eff:c1:main:1:1", line); err != nil || d {
		t.Fatalf("first try, nobody watching: %v %v", d, err)
	}
	lines, cancel := h.subscribe()
	defer cancel()
	if d, err := h.say("c1", "main:1", "eff:c1:main:1:1", line); err != nil || d || len(lines) != 0 {
		t.Errorf("replay: delivered %v, %v, %d queued", d, err, len(lines))
	}
}

// WatchSpeech over a real Connect stream: an owner receives a Speak line, and once it ends later lines say nobody heard.
func TestWatchSpeech_Streams(t *testing.T) {
	s := &Server{speech: newSpeechHub()}
	admin := auth.Identity{Sub: "a", Groups: []string{auth.RequiredGroup}}
	mux := http.NewServeMux()
	mux.Handle(agentfleetv1connect.AgentServiceWatchSpeechProcedure, connect.NewServerStreamHandler(
		agentfleetv1connect.AgentServiceWatchSpeechProcedure, s.WatchSpeech))
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mux.ServeHTTP(w, r.WithContext(withPrincipal(r.Context(), Principal{Member: &admin})))
	}))
	defer srv.Close()
	ctx, stop := context.WithCancel(context.Background())
	defer stop()
	got := make(chan *agentfleetv1.WatchSpeechResponse, 1)
	go func() { // the call returns once the first line makes the headers flow
		st, err := connect.NewClient[agentfleetv1.WatchSpeechRequest, agentfleetv1.WatchSpeechResponse](
			srv.Client(), srv.URL+agentfleetv1connect.AgentServiceWatchSpeechProcedure).CallServerStream(ctx, connect.NewRequest(&agentfleetv1.WatchSpeechRequest{}))
		if err == nil && st.Receive() {
			got <- st.Msg()
		}
	}()
	line := &agentfleetv1.WatchSpeechResponse{Character: "ops", Text: "hi", Face: "happy"}
	var d bool
	for i := 0; i < 50 && !d; i++ { // retry until the handler is subscribed
		d, _ = s.speech.say("c1", "main:1", fmt.Sprintf("eff:c1:main:1:%d", i+1), line)
		if !d {
			time.Sleep(20 * time.Millisecond)
		}
	}
	select {
	case m := <-got:
		if m.GetCharacter() != "ops" || m.GetText() != "hi" || m.GetFace() != "happy" {
			t.Errorf("received %v", m)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("no line reached the stream")
	}
	stop()
	for i := 0; i < 100; i++ { // the handler unsubscribes when the context ends
		s.speech.mu.Lock()
		n := len(s.speech.subs)
		s.speech.mu.Unlock()
		if n == 0 {
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
	if d, _ := s.speech.say("c1", "main:2", "eff:c1:main:2:1", line); d {
		t.Error("a line after the stream ended was delivered")
	}
}
