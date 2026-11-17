package agentsvc

import (
	"context"
	"errors"
	"fmt"
	"regexp"
	"sync"
	"time"
	"unicode/utf8"

	"connectrpc.com/connect"
	"github.com/google/uuid"

	"github.com/MohammadBnei/agent-fleet/core/internal/errkeys"
	"github.com/MohammadBnei/agent-fleet/core/internal/sessions"
	"github.com/MohammadBnei/agent-fleet/core/internal/transcript"
	agentfleetv1 "github.com/MohammadBnei/agent-fleet/proto/gen/go/agentfleet/v1"
)

// The game channel (docs/adr/0084 §7): the owner talks to a character with PostToCharacter, and a character answers
// the owner's browser with Speak. A line is live only: it never goes on the bridge or into a transcript, wakes
// nothing, and is dropped when no browser watches.
const (
	maxSpeakPerTurn = 20
	maxSpeakPerWin  = 3 * maxSpeakPerTurn // per speakWindow, whatever turn ids the character's keys name
	speakWindow     = time.Minute
	maxSpeakText    = 1000
	maxFace         = 32
	speechBuffer    = 32
)

var faceRe = regexp.MustCompile(`^[a-z0-9_-]*$`)

// speechHub fans a character's lines out to the owner's open WatchSpeech streams and counts them per turn.
type speechHub struct {
	mu     sync.Mutex
	subs   map[chan *agentfleetv1.WatchSpeechResponse]struct{}
	turns  map[string]*turnLines  // character session -> its current turn's lines
	recent map[string][]time.Time // character session -> when its recent lines went out
}

type turnLines struct {
	id   string
	keys map[string]bool // key -> whether a browser got the line
}

func newSpeechHub() *speechHub {
	return &speechHub{subs: map[chan *agentfleetv1.WatchSpeechResponse]struct{}{}, turns: map[string]*turnLines{}, recent: map[string][]time.Time{}}
}

func (h *speechHub) subscribe() (<-chan *agentfleetv1.WatchSpeechResponse, func()) {
	ch := make(chan *agentfleetv1.WatchSpeechResponse, speechBuffer)
	h.mu.Lock()
	h.subs[ch] = struct{}{}
	h.mu.Unlock()
	return ch, func() {
		h.mu.Lock()
		delete(h.subs, ch)
		h.mu.Unlock()
	}
}

// say counts the line against its turn (a replayed key counts once and is not sent again) and hands it to every
// subscriber that has room. delivered is whether a browser was watching.
func (h *speechHub) say(session, turn, key string, line *agentfleetv1.WatchSpeechResponse) (delivered bool, err error) {
	h.mu.Lock()
	defer h.mu.Unlock()
	tl := h.turns[session]
	if tl == nil || tl.id != turn {
		tl = &turnLines{id: turn, keys: map[string]bool{}}
		h.turns[session] = tl
	}
	if got, dup := tl.keys[key]; dup {
		return got, nil // what the first try reported, never a fresh guess
	}
	if len(tl.keys) >= maxSpeakPerTurn {
		return false, errkeys.Connect(connect.CodeResourceExhausted, errkeys.KeyInternal, fmt.Errorf("at most %d spoken lines per turn", maxSpeakPerTurn))
	}
	// The turn id comes from the character's own sidecar, so the per-turn count alone is not a bound: a rolling cap
	// per character holds however often the id changes.
	now := time.Now()
	recent := h.recent[session][:0:0]
	for _, at := range h.recent[session] {
		if now.Sub(at) < speakWindow {
			recent = append(recent, at)
		}
	}
	if len(recent) >= maxSpeakPerWin {
		h.recent[session] = recent
		return false, errkeys.Connect(connect.CodeResourceExhausted, errkeys.KeyInternal, fmt.Errorf("at most %d spoken lines per %s", maxSpeakPerWin, speakWindow))
	}
	h.recent[session] = append(recent, now)
	for ch := range h.subs {
		select {
		case ch <- line:
			delivered = true
		default: // a stalled browser loses the line, never blocks the character
		}
	}
	tl.keys[key] = delivered
	return delivered, nil
}

// Speak is a started character's line for the owner's browser. The key is the sidecar's effect key, so the turn it
// belongs to bounds the lines (docs/adr/0084 §7); a key outside a turn is refused.
func (s *Server) Speak(ctx context.Context, characterID, text, face, key string) (bool, error) {
	if text == "" || utf8.RuneCountInString(text) > maxSpeakText || !utf8.ValidString(text) {
		return false, errkeys.Connect(connect.CodeInvalidArgument, errkeys.KeyInternal, fmt.Errorf("text is required and at most %d characters", maxSpeakText))
	}
	if len(face) > maxFace || !faceRe.MatchString(face) {
		return false, errkeys.Connect(connect.CodeInvalidArgument, errkeys.KeyInternal, fmt.Errorf("face is at most %d of a-z, 0-9, - and _", maxFace))
	}
	thread, seq, ok := sessions.ParseEffectKey(characterID, key)
	if !ok {
		return false, errkeys.Connect(connect.CodeInvalidArgument, errkeys.KeyInternal, errors.New("a spoken line belongs to a turn"))
	}
	t, err := s.sessions.Get(ctx, characterID)
	if err != nil || t.DefinitionName == nil {
		return false, errkeys.Connect(connect.CodeNotFound, errkeys.KeyInternal, errors.New("no such character"))
	}
	return s.speech.say(characterID, fmt.Sprintf("%s:%d", thread, seq), key,
		&agentfleetv1.WatchSpeechResponse{Character: *t.DefinitionName, Text: text, Face: face})
}

// WatchSpeech streams the lines characters speak, to an admin on the console or an admin origin, until the stream's
// age is up (the client reconnects, as the other streams).
func (s *Server) WatchSpeech(ctx context.Context, _ *connect.Request[agentfleetv1.WatchSpeechRequest], stream *connect.ServerStream[agentfleetv1.WatchSpeechResponse]) error {
	p, err := principal(ctx)
	if err != nil {
		return err
	}
	if !p.AnswersCards() {
		return errkeys.Connect(connect.CodePermissionDenied, errkeys.KeyInternal, errors.New("only the owner hears characters"))
	}
	lines, cancel := s.speech.subscribe()
	defer cancel()
	deadline := time.NewTimer(streamMaxAge)
	defer deadline.Stop()
	for {
		select {
		case <-ctx.Done():
			return nil
		case <-deadline.C:
			return nil
		case l := <-lines:
			if err := stream.Send(l); err != nil {
				return err
			}
		}
	}
}

// PostToCharacter is the owner's message to a started character, from the console or an admin origin (the game): the
// same append as the console's PostMessage, so it is T0 and warms a character whose pod is down (docs/adr/0084 §7).
func (s *Server) PostToCharacter(ctx context.Context, req *connect.Request[agentfleetv1.PostToCharacterRequest]) (*connect.Response[agentfleetv1.PostToCharacterResponse], error) {
	p, err := principal(ctx)
	if err != nil {
		return nil, err
	}
	if !p.AnswersCards() {
		return nil, errkeys.Connect(connect.CodePermissionDenied, errkeys.KeyInternal, errors.New("only the owner talks to a character"))
	}
	text, key := req.Msg.GetText(), req.Msg.GetThreadKey()
	if text == "" || len(text) > maxText {
		return nil, errkeys.Connect(connect.CodeInvalidArgument, errkeys.KeyInternal, fmt.Errorf("text is required and at most %d bytes", maxText))
	}
	if key != "" && key != transcript.MainThread && !transcript.ValidThread(key) {
		return nil, errkeys.Connect(connect.CodeInvalidArgument, errkeys.KeyInternal, transcript.ErrBadThread)
	}
	c, err := s.sessions.StandingByDefinition(ctx, req.Msg.GetCharacter())
	switch {
	case errors.Is(err, sessions.ErrNotFound):
		return nil, errkeys.Connect(connect.CodeNotFound, errkeys.KeyInternal, errors.New("no such character"))
	case err != nil:
		return nil, errkeys.Connect(connect.CodeInternal, errkeys.KeyInternal, err)
	case c.StoppedAt != nil:
		return nil, errkeys.Connect(connect.CodeFailedPrecondition, errkeys.KeyInternal, errors.New("this character is stopped: start it first"))
	}
	// Warm BEFORE appending, and hold the release until the append lands (dashboard.WarmIfIdle).
	_, release, err := s.dash.WarmForAppend(ctx, c.ID)
	defer release()
	if err != nil {
		return nil, err
	}
	seq, err := s.transcr.AppendAuthored(transcript.WithThread(ctx, key), c.ID, p.author(), text, "discussion", uuid.NewString(), nil)
	if err != nil {
		return nil, errkeys.Connect(connect.CodeInternal, errkeys.KeyInternal, err)
	}
	return connect.NewResponse(&agentfleetv1.PostToCharacterResponse{Seq: seq}), nil
}
