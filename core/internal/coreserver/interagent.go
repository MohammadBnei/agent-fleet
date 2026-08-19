package coreserver

import (
	"context"
	"fmt"
	"log/slog"
	"time"

	agentfleetv1 "github.com/MohammadBnei/agent-fleet/proto/gen/go/agentfleet/v1"

	"github.com/MohammadBnei/agent-fleet/core/internal/dashboard"
	"github.com/MohammadBnei/agent-fleet/core/internal/sessions"
)

// Inter-agent coordination (docs/adr/0041). One agent can see the fleet's
// other sessions, send one a message, and block until it reaches a given
// liveness state — herdr's "prompt each other, and wait until another agent
// is genuinely blocked", routed the only way docs/adr/0020 point 4 allows:
// agent -> its own sidecar -> core (here) -> the target.

// maxPromptDepth bounds a relay chain. A prompts B, B prompts C, C prompts
// A is a livelock with no human in it; the depth counter is what makes that
// terminate. Deliberately small — a chain this long is already a design
// smell, not a workflow to support.
const maxPromptDepth = 3

// settledStates is what an empty `until` means: the target stopped being
// something you wait on. Mirrors herdr's `agent prompt --wait` default
// (first settled idle/done/blocked) plus `stalled`, which is this fleet's
// own addition and just as final from a waiter's point of view.
var settledStates = []string{
	string(sessions.LiveStateIdle),
	string(sessions.LiveStateDone),
	string(sessions.LiveStateBlocked),
	string(sessions.LiveStateStalled),
}

// waitPollInterval matches the transcript relay's own 2s cadence — this
// polls Postgres rather than subscribing, for the same reason the
// transcript API is a pull/cursor read (docs/adr/0013): a missed event on a
// bare watch is unrecoverable, and a poll cannot miss one.
const waitPollInterval = 2 * time.Second

func (s *Server) liveStateOf(ctx context.Context, taskID string) (*sessions.Session, sessions.LiveState, error) {
	t, err := s.sessions.Get(ctx, taskID)
	if err != nil {
		return nil, "", fmt.Errorf("get task: %w", err)
	}
	if t == nil {
		return nil, "", fmt.Errorf("session %s not found", taskID)
	}
	return t, sessions.DeriveLiveState(t, time.Now(), dashboard.DefaultTurnStall), nil
}

func (s *Server) ListPeerSessions(ctx context.Context, req *agentfleetv1.ListPeerSessionsRequest) (*agentfleetv1.ListPeerSessionsResponse, error) {
	// Same bounded listing the dashboard uses — an agent picking a target
	// wants the fleet's current sessions, not its entire history.
	all, err := s.sessions.List(ctx, 100, "")
	if err != nil {
		return nil, fmt.Errorf("list sessions: %w", err)
	}
	out := make([]*agentfleetv1.SessionSummary, 0, len(all))
	for _, t := range all {
		if t.ID == req.GetCallerSessionId() {
			continue
		}
		out = append(out, &agentfleetv1.SessionSummary{
			SessionId:   t.ID,
			Repo:        t.Repo,
			Description: t.Description,
			LiveState:   string(sessions.DeriveLiveState(&t, time.Now(), dashboard.DefaultTurnStall)),
		})
	}
	return &agentfleetv1.ListPeerSessionsResponse{Sessions: out}, nil
}

// PromptSession delivers one agent's message to another session.
//
// The guards are the whole design; the delivery is three lines.
func (s *Server) PromptSession(ctx context.Context, req *agentfleetv1.PromptSessionRequest) (*agentfleetv1.PromptSessionResponse, error) {
	caller, target, text := req.GetCallerSessionId(), req.GetTargetSessionId(), req.GetText()
	if caller == "" || target == "" || text == "" {
		return nil, fmt.Errorf("caller_task_id, target_task_id and text are required")
	}
	if caller == target {
		// Not merely useless: a session's own messages already reach it as
		// its next input, so this would be a self-feeding loop.
		return nil, fmt.Errorf("a session cannot prompt itself")
	}
	if req.GetDepth() >= maxPromptDepth {
		return nil, fmt.Errorf("prompt relay depth %d reached the limit of %d — refusing to relay further", req.GetDepth(), maxPromptDepth)
	}

	t, state, err := s.liveStateOf(ctx, target)
	if err != nil {
		return nil, err
	}
	// A blocked session is waiting on a *human* — a permission prompt or a
	// question. Delivering an agent's message here would push a turn into a
	// session whose human decision is still outstanding, and (worse) invite
	// the caller to believe it had answered it. Permission decisions are
	// human-only by docs/adr/0029 and this must not become a side door.
	if state == sessions.LiveStateBlocked {
		return nil, fmt.Errorf("session %s is blocked waiting on a human decision — it cannot be prompted by an agent", target)
	}
	// The "unapproved proposal" guard that used to sit here is gone with the
	// status: a proposal is now a row in a different table with no pod path,
	// so a session reachable by this call has already been opened by a human
	// (docs/adr/0048).

	// Delivered as a plain discussion entry authored by "agent". It is
	// deliberately impossible for this path to produce an `answer` or a
	// `permission_response`: those resolve human decisions, and an agent
	// answering another agent's prompt would silently void docs/adr/0029's
	// guarantee that a permission decision is always a real human one.
	// Authored as "session", not "agent". The distinction is load-bearing:
	// a worker relays its own output as from="agent", and the human-message
	// stream filters those out precisely so a session cannot feed itself in
	// a loop. Delivering prompts as "agent" therefore never reached a live
	// target at all (found on a real cluster — the entry landed in the
	// transcript and the running agent never saw it). "session" is
	// deliverable and still unmistakably not the session's own voice.
	// Warm BEFORE appending, never after — see dashboard.WarmIfIdle for why.
	//
	// NOT best-effort, and the comment that said it was had the arithmetic
	// backwards. It read: "a capacity rejection is not worth failing over,
	// since the entry is durable either way and a later warm will pick it up
	// from a cursor computed after it exists."
	//
	// A cursor computed AFTER the entry is ABOVE it. resumeFromSeq is
	// LatestSeq — MAX(seq)+1 — so an entry that already exists is exactly the
	// thing a later warm skips. Durability was never the question: the row
	// persisted fine and no pod ever read it. So a capacity rejection did not
	// delay an inter-agent prompt, it dropped it permanently, while logging a
	// warning that read like a retry was coming.
	//
	// Fail instead. The caller is an agent with a tool result to read; telling
	// it the fleet is full is a fact it can act on, where an accepted prompt
	// that reaches nobody is not.
	var podName string
	if s.warm != nil && !sessions.IsPodPhaseLive(t.PodPhase) {
		var warmErr error
		if podName, warmErr = s.warm(ctx, target); warmErr != nil {
			slog.Warn("coreserver: PromptSession could not warm target", "callerTaskId", caller, "targetTaskId", target, "error", warmErr)
			return nil, fmt.Errorf("target session %s has no pod and could not be warmed: %w", target, warmErr)
		}
	}

	body := fmt.Sprintf("[from session %s]\n\n%s", caller, text)
	seq, err := s.transcr.Append(ctx, target, "session", body, "discussion", fmt.Sprintf("prompt-%s-%s-%d", caller, target, time.Now().UnixNano()))
	if err != nil {
		return nil, fmt.Errorf("append prompt: %w", err)
	}

	slog.Info("coreserver: session prompted another session", "callerTaskId", caller, "targetTaskId", target, "seq", seq, "depth", req.GetDepth())
	_, after, err := s.liveStateOf(ctx, target)
	if err != nil {
		return nil, err
	}
	return &agentfleetv1.PromptSessionResponse{Seq: seq, LiveState: string(after), PodName: podName}, nil
}

// WaitForSessionState blocks until the target reaches one of `until`, or
// the timeout expires. Returning timed_out rather than an error is the
// point: "still working after 2 minutes" is an answer, and the caller
// decides what to do with it.
func (s *Server) WaitForSessionState(ctx context.Context, req *agentfleetv1.WaitForSessionStateRequest) (*agentfleetv1.WaitForSessionStateResponse, error) {
	target := req.GetTargetSessionId()
	if target == "" {
		return nil, fmt.Errorf("target_task_id is required")
	}
	until := req.GetUntil()
	if len(until) == 0 {
		until = settledStates
	}
	wanted := make(map[string]bool, len(until))
	for _, u := range until {
		wanted[u] = true
	}

	timeout := time.Duration(req.GetTimeoutMs()) * time.Millisecond
	if timeout <= 0 {
		timeout = 2 * time.Minute
	}
	deadline := time.Now().Add(timeout)

	for {
		_, state, err := s.liveStateOf(ctx, target)
		if err != nil {
			return nil, err
		}
		// A settled state only counts once the target has actually moved
		// past the baseline. Otherwise "prompt, then wait for it to settle"
		// answers with the state it held before the prompt landed, which is
		// the exact race herdr's own two-phase wait exists to avoid.
		progressed := true
		if req.GetAfterSeq() > 0 {
			latest, err := s.transcr.LatestSeq(ctx, target)
			if err != nil {
				return nil, fmt.Errorf("latest seq: %w", err)
			}
			progressed = latest > req.GetAfterSeq()
		}
		if progressed && wanted[string(state)] {
			return &agentfleetv1.WaitForSessionStateResponse{LiveState: string(state)}, nil
		}
		// A target with no live pod will never move on its own — nothing is
		// running to change its state — so waiting the full timeout would
		// just be a slow way to time out.
		if state == sessions.LiveStateNone {
			return &agentfleetv1.WaitForSessionStateResponse{LiveState: string(state), TimedOut: true}, nil
		}
		if time.Now().After(deadline) {
			return &agentfleetv1.WaitForSessionStateResponse{LiveState: string(state), TimedOut: true}, nil
		}
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-time.After(waitPollInterval):
		}
	}
}
