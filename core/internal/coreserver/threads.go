package coreserver

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"

	"github.com/google/uuid"

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

// settleCrash handles a CRASHED pod of a standing character (docs/adr/0084 §3, §4). Each thread head entry it
// settled (an effect of it ran, or the third crash in a row) is closed with a crash result, which moves the
// thread's cursor so the re-warm never replays it; a poison entry also alerts the owner. Every thread that had a
// turn running gets a crash entry, or main when none did, and the third crash entry in an hour alerts the owner:
// the re-warm stops there. The entry's key is the pod attempt's (its Job UID, which a re-warm changes while the Job
// name stays), so the same crash seen twice is one entry and two crashes are two. loss is why the node took the
// pod away ("Evicted", "Preempted"), else "": a planned loss adds no crash entry and no count against an entry,
// and still settles a head whose effect ran.
func (s *Server) settleCrash(ctx context.Context, sessionID, jobUID, loss, reason string) {
	t, err := s.sessions.Get(ctx, sessionID)
	if err != nil || t == nil || t.Lifecycle == nil || *t.Lifecycle != sessions.Standing {
		return
	}
	settled, running, err := s.sessions.SettleCrash(ctx, sessionID, loss != "")
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
	if loss != "" {
		return
	}
	key := "podcrash:" + sessionID + ":" + jobUID
	if jobUID == "" { // a warm that failed before any Job existed
		key = "podcrash:" + sessionID + ":" + uuid.NewString()
	}
	if len(running) == 0 {
		running = []string{sessions.MainThread}
	}
	// One pod, one entry: the key is the pod's, so a thread named later by a re-sighting adds none.
	if _, err := s.transcr.Append(transcript.WithThread(ctx, running[0]), sessionID, "fleet", sessions.CrashEntry(reason), "system", key); err != nil {
		slog.ErrorContext(ctx, "coreserver: crash entry", "sessionId", sessionID, "error", err)
		return
	}
	// ponytail: a pod runs one thread at a time (§4), so one entry covers "each thread with a turn running".
	if n, err := s.sessions.CrashesInWindow(ctx, sessionID); err == nil && n >= sessions.MaxCrashes {
		s.announceBlocked(ctx, sessionID, fmt.Sprintf("crashed %d times in an hour: core stopped re-warming it", n))
	}
}
