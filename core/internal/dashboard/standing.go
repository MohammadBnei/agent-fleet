package dashboard

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"slices"

	"connectrpc.com/connect"
	"github.com/google/uuid"

	"github.com/MohammadBnei/agent-fleet/core/internal/auth"
	"github.com/MohammadBnei/agent-fleet/core/internal/definitions"
	"github.com/MohammadBnei/agent-fleet/core/internal/errkeys"
	"github.com/MohammadBnei/agent-fleet/core/internal/sessions"
	"github.com/MohammadBnei/agent-fleet/core/internal/transcript"
	agentfleetv1 "github.com/MohammadBnei/agent-fleet/proto/gen/go/agentfleet/v1"
)

// StandingOnly is the refusal of every path that would open a standing definition other than Start
// (docs/adr/0084 §2): nil when spec is not standing.
func StandingOnly(spec *definitions.Spec) error {
	if spec == nil || spec.Lifecycle == nil || !slices.Contains(spec.Lifecycle.Allow, sessions.Standing) {
		return nil
	}
	return errkeys.Connect(connect.CodeFailedPrecondition, errkeys.KeyStandingOnly, errors.New("this definition is a standing character: an admin's Start opens it"))
}

// The standing lifecycle's outcomes a caller may treat as a state rather than a failure: AgentService's character API
// (docs/adr/0084 amendment A4) answers each with the current state, the console with the error.
var (
	// ErrStandingUnknown: no valid standing definition by that name.
	ErrStandingUnknown = errors.New("no such character")
	// ErrStandingNever: the character was never started, so it has no row.
	ErrStandingNever = errors.New("never started")
	// ErrStandingStarted: Start on a character that is started.
	ErrStandingStarted = errors.New("already started")
	// ErrStandingStopping: Start on a stopped character whose pod is still going down.
	ErrStandingStopping = errors.New("pod still stopping")
	// ErrStandingStopped: Stop on a character that is stopped.
	ErrStandingStopped = errors.New("already stopped")
	// ErrNoThread: the character has no open thread by that key, or the key is main, which never closes.
	ErrNoThread = errors.New("no such thread")
	// ErrThreadParked: Close on a thread that is already parked.
	ErrThreadParked = errors.New("already parked")
)

// StartStanding makes a character's one session, or resumes a stopped one, every thread with its kept SDK session id and
// cursor, on the current definition version (docs/adr/0084 §2, amended A4). It provisions nothing, as CreateSession
// does not.
func (s *Server) StartStanding(ctx context.Context, req *connect.Request[agentfleetv1.StartStandingRequest]) (*connect.Response[agentfleetv1.StartStandingResponse], error) {
	name := req.Msg.GetDefinition()
	if s.definitions == nil {
		return nil, errkeys.Connect(connect.CodeUnavailable, errkeys.KeyInternal, errors.New("defined agents are not configured"))
	}
	by := adminSub(ctx)
	if by == "" {
		return nil, errkeys.Connect(connect.CodeUnauthenticated, errkeys.KeyInternal, errors.New("no identity on the request: a character is started by its admin"))
	}
	head, err := s.definitions.Resolve(name)
	switch {
	case errors.Is(err, definitions.ErrUnknown):
		return nil, errkeys.Connect(connect.CodeNotFound, errkeys.KeyInternal, fmt.Errorf("%w: %w", ErrStandingUnknown, err))
	case errors.Is(err, definitions.ErrInvalid):
		return nil, errkeys.Connect(connect.CodeFailedPrecondition, errkeys.KeyInternal, err)
	case err != nil:
		return nil, errkeys.Connect(connect.CodeUnavailable, errkeys.KeyInternal, err)
	}
	if StandingOnly(head.Effective) == nil {
		return nil, errkeys.Connect(connect.CodeFailedPrecondition, errkeys.KeyInternal, fmt.Errorf("definition %q is not standing: %w", name, ErrStandingUnknown))
	}

	row, err := s.sessions.StandingByDefinition(ctx, name)
	switch {
	case errors.Is(err, sessions.ErrNotFound):
		principal := sessions.StandingPrefix + name
		row, err = s.createSession(ctx, &agentfleetv1.CreateSessionRequest{Title: &name}, &head, sessions.CreateParams{
			Owner: "client:" + principal, Visibility: "private", Lifecycle: sessions.Standing,
			Invoker: principal, ThreadKey: "main", StartedBy: by, TrustSet: sessions.TrustFor(sessions.TierFleet),
		})
		if err != nil {
			// A concurrent Start won the insert (sessions_open_thread_idx): report it as the clean error.
			if _, rerr := s.sessions.StandingByDefinition(ctx, name); rerr == nil {
				return nil, errkeys.Connect(connect.CodeFailedPrecondition, errkeys.KeyInternal, fmt.Errorf("%q is already started: %w", name, ErrStandingStarted))
			}
			return nil, err
		}
	case err != nil:
		return nil, errkeys.Connect(connect.CodeInternal, errkeys.KeyInternal, err)
	default:
		if row.StoppedAt == nil {
			return nil, errkeys.Connect(connect.CodeFailedPrecondition, errkeys.KeyInternal, fmt.Errorf("%q is already started: %w", name, ErrStandingStarted))
		}
		// The restart is when a stopped character takes the current effective model (docs/adr/0088 amendment).
		model, err := s.standingRestartModel(ctx, head, row.ID)
		if err != nil {
			return nil, err
		}
		if ok, err := s.sessions.RestartStanding(ctx, row.ID, head.SHA, by, model); err != nil {
			return nil, errkeys.Connect(connect.CodeInternal, errkeys.KeyInternal, err)
		} else if !ok {
			return nil, errkeys.Connect(connect.CodeAborted, errkeys.KeyInternal, fmt.Errorf("the character changed or its pod is still stopping; retry: %w", ErrStandingStopping))
		}
		if row, err = s.sessions.Get(ctx, row.ID); err != nil {
			return nil, errkeys.Connect(connect.CodeInternal, errkeys.KeyInternal, err)
		}
	}
	slog.InfoContext(ctx, "dashboard StartStanding", "definition", name, "sessionId", row.ID, "by", by)
	return connect.NewResponse(&agentfleetv1.StartStandingResponse{Session: SessionToProto(*row)}), nil
}

// StopStanding marks a character stopped (docs/adr/0084 §2, amended A4): the row takes no pod until Start, and a live
// pod gets StopSession's cooperative abort and its grace-period teardown. Only a turn in flight is aborted, like
// Ctrl-C: each running thread's head entry is settled with an aborted result, so Start does not replay it. Every
// thread keeps its SDK session id and cursor, and every waiting entry stays.
func (s *Server) StopStanding(ctx context.Context, req *connect.Request[agentfleetv1.StopStandingRequest]) (*connect.Response[agentfleetv1.StopStandingResponse], error) {
	name := req.Msg.GetDefinition()
	row, err := s.sessions.StandingByDefinition(ctx, name)
	if errors.Is(err, sessions.ErrNotFound) {
		return nil, errkeys.Connect(connect.CodeNotFound, errkeys.KeyInternal, fmt.Errorf("%q was never started: %w", name, ErrStandingNever))
	}
	if err != nil {
		return nil, errkeys.Connect(connect.CodeInternal, errkeys.KeyInternal, err)
	}
	// Read before the mark: once stopped, nothing says which turn the abort cut.
	running, err := s.sessions.RunningHeads(ctx, row.ID)
	if err != nil {
		return nil, errkeys.Connect(connect.CodeInternal, errkeys.KeyInternal, err)
	}
	// Marked before the abort entry: the mark is what keeps a racing message from warming it again.
	ok, err := s.sessions.MarkStandingStopped(ctx, row.ID)
	if err != nil {
		return nil, errkeys.Connect(connect.CodeInternal, errkeys.KeyInternal, err)
	}
	if !ok {
		return nil, errkeys.Connect(connect.CodeFailedPrecondition, errkeys.KeyInternal, fmt.Errorf("%q is already stopped: %w", name, ErrStandingStopped))
	}
	if _, err := s.transcr.Append(ctx, row.ID, "human", "stopped by "+adminSub(ctx), "abort", uuid.NewString()); err != nil {
		return nil, errkeys.Connect(connect.CodeInternal, errkeys.KeyInternal, err)
	}
	for key, seq := range running {
		body, _ := json.Marshal(map[string]any{"subtype": "aborted", "consumedSeq": seq, "aborted": true})
		if _, err := s.transcr.Append(transcript.WithThread(ctx, key), row.ID, "fleet", string(body), "result", fmt.Sprintf("stop:%s:%s:%d", row.ID, key, seq)); err != nil {
			return nil, errkeys.Connect(connect.CodeInternal, errkeys.KeyInternal, err)
		}
	}
	slog.InfoContext(ctx, "dashboard StopStanding", "definition", name, "sessionId", row.ID)
	return connect.NewResponse(&agentfleetv1.StopStandingResponse{}), nil
}

// CloseStandingThread is the owner's close of one thread (docs/adr/0084 §4, amended A4): the thread is parked. Its SDK
// session id and transcript stay, and the owner's next message to the key resumes it with its context. An abort entry
// in the thread tells a live pod to end that thread's SDK session (the pod ends only that thread: Stop is the abort in
// main), which aborts a turn in flight like Ctrl-C. Main never closes.
func (s *Server) CloseStandingThread(ctx context.Context, req *connect.Request[agentfleetv1.CloseStandingThreadRequest]) (*connect.Response[agentfleetv1.CloseStandingThreadResponse], error) {
	name, key := req.Msg.GetDefinition(), req.Msg.GetThreadKey()
	row, err := s.sessions.StandingByDefinition(ctx, name)
	if errors.Is(err, sessions.ErrNotFound) {
		return nil, errkeys.Connect(connect.CodeNotFound, errkeys.KeyInternal, fmt.Errorf("%q was never started: %w", name, ErrStandingNever))
	}
	if err != nil {
		return nil, errkeys.Connect(connect.CodeInternal, errkeys.KeyInternal, err)
	}
	// The abort goes first and the close after, so a failed append leaves the thread open for a retry instead of
	// closed under a live SDK session.
	threads, err := s.sessions.Threads(ctx, row.ID)
	if err != nil {
		return nil, errkeys.Connect(connect.CodeInternal, errkeys.KeyInternal, err)
	}
	i := slices.IndexFunc(threads, func(t sessions.Thread) bool { return t.Key == key && key != sessions.MainThread })
	switch {
	case i < 0:
		return nil, errkeys.Connect(connect.CodeFailedPrecondition, errkeys.KeyInternal, fmt.Errorf("thread %q of %q is main or unknown: %w", key, name, ErrNoThread))
	case threads[i].Closed:
		return nil, errkeys.Connect(connect.CodeFailedPrecondition, errkeys.KeyInternal, fmt.Errorf("thread %q of %q is already closed: %w", key, name, ErrThreadParked))
	}
	if _, err := s.transcr.Append(transcript.WithThread(ctx, key), row.ID, "human", "closed by "+adminSub(ctx), "abort", uuid.NewString()); err != nil {
		return nil, errkeys.Connect(connect.CodeInternal, errkeys.KeyInternal, err)
	}
	if ok, err := s.sessions.CloseThread(ctx, row.ID, key); err != nil {
		return nil, errkeys.Connect(connect.CodeInternal, errkeys.KeyInternal, err)
	} else if !ok {
		return nil, errkeys.Connect(connect.CodeFailedPrecondition, errkeys.KeyInternal, fmt.Errorf("thread %q of %q was closed meanwhile: %w", key, name, ErrThreadParked))
	}
	slog.InfoContext(ctx, "dashboard CloseStandingThread", "definition", name, "sessionId", row.ID, "thread", key)
	return connect.NewResponse(&agentfleetv1.CloseStandingThreadResponse{}), nil
}

func adminSub(ctx context.Context) string {
	id, _ := auth.FromContext(ctx)
	if id.Sub == "" {
		return id.Email // a cookie minted before Sub existed
	}
	return id.Sub
}
