package coreserver

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"

	"google.golang.org/grpc/metadata"

	"github.com/MohammadBnei/agent-fleet/core/internal/sessions"
	"github.com/MohammadBnei/agent-fleet/core/internal/transcript"
)

// mdThread is the thread a standing character's pod is running a turn for (docs/adr/0084 §4), set by the
// sidecar on every call while the worker has named one. Only one thread runs per pod, so it is ambient, not a
// field of each request. It is not a trust boundary: every thread holds the same set, and the append refuses
// a thread the row does not have.
const mdThread = "x-fleet-thread"

// withThread puts the caller's thread on ctx, so every append its handler makes carries it.
func withThread(ctx context.Context) context.Context {
	md, _ := metadata.FromIncomingContext(ctx)
	return transcript.WithThread(ctx, first(md, mdThread))
}

// noteEffect records, before an effect method runs, that it runs for the entry its key names
// (docs/adr/0084 §4): after a crash that entry is done, never replayed. A key that is not an effect key
// (a non-standing session, or a caller outside a turn) notes nothing.
func (s *Server) noteEffect(ctx context.Context, sessionID, key string) {
	thread, seq, ok := sessions.ParseEffectKey(sessionID, key)
	if !ok {
		return
	}
	if err := s.sessions.NoteEffect(ctx, sessionID, thread, seq); err != nil {
		slog.ErrorContext(ctx, "coreserver: noting an effect", "sessionId", sessionID, "thread", thread, "error", err)
	}
}

// settleCrash closes, with a crash result, each thread head entry a CRASHED pod settled (docs/adr/0084 §4): one
// an effect ran for, or the third crash in a row. The result advances the thread's cursor past the entry, so
// the re-warm delivers what is behind it and never replays it; a poison entry also alerts the owner.
func (s *Server) settleCrash(ctx context.Context, sessionID string) {
	t, err := s.sessions.Get(ctx, sessionID)
	if err != nil || t == nil || t.Lifecycle == nil || *t.Lifecycle != sessions.Standing {
		return
	}
	settled, err := s.sessions.SettleCrash(ctx, sessionID)
	if err != nil {
		slog.ErrorContext(ctx, "coreserver: settling a crash", "sessionId", sessionID, "error", err)
		return
	}
	for _, st := range settled {
		tctx := transcript.WithThread(ctx, st.Key)
		body, _ := json.Marshal(map[string]any{"subtype": "crashed", "consumedSeq": st.Seq, "crash": true, "reason": st.Reason})
		if _, err := s.transcr.Append(tctx, sessionID, "fleet", string(body), "result", fmt.Sprintf("crash:%s:%s:%d", sessionID, st.Key, st.Seq)); err != nil {
			slog.ErrorContext(ctx, "coreserver: crash result", "sessionId", sessionID, "thread", st.Key, "error", err)
			continue
		}
		if st.Reason == "poison" {
			s.announceBlocked(ctx, sessionID, fmt.Sprintf("entry %d of thread %q crashed its turn %d times and was skipped", st.Seq, st.Key, sessions.PoisonCrashes))
		}
	}
}
