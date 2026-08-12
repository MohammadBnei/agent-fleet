package coreserver

import (
	"context"
	"fmt"
	"log/slog"
	"time"

	agentfleetv1 "github.com/MohammadBnei/agent-fleet/proto/gen/go/agentfleet/v1"

	"github.com/MohammadBnei/agent-fleet/core/internal/dashboard"
	"github.com/MohammadBnei/agent-fleet/core/internal/tasks"
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
	string(tasks.LiveStateIdle),
	string(tasks.LiveStateDone),
	string(tasks.LiveStateBlocked),
	string(tasks.LiveStateStalled),
}

// waitPollInterval matches the transcript relay's own 2s cadence — this
// polls Postgres rather than subscribing, for the same reason the
// transcript API is a pull/cursor read (docs/adr/0013): a missed event on a
// bare watch is unrecoverable, and a poll cannot miss one.
const waitPollInterval = 2 * time.Second

func (s *Server) liveStateOf(ctx context.Context, taskID string) (*tasks.Task, tasks.LiveState, error) {
	t, err := s.tasks.GetTask(ctx, taskID)
	if err != nil {
		return nil, "", fmt.Errorf("get task: %w", err)
	}
	if t == nil {
		return nil, "", fmt.Errorf("session %s not found", taskID)
	}
	return t, tasks.DeriveLiveState(t, time.Now(), dashboard.DefaultTurnStall), nil
}

func (s *Server) ListSessions(ctx context.Context, req *agentfleetv1.ListSessionsRequest) (*agentfleetv1.ListSessionsResponse, error) {
	// Same bounded listing the dashboard uses — an agent picking a target
	// wants the fleet's current sessions, not its entire history.
	all, err := s.tasks.ListRecentTasks(ctx, 100)
	if err != nil {
		return nil, fmt.Errorf("list sessions: %w", err)
	}
	out := make([]*agentfleetv1.SessionSummary, 0, len(all))
	for _, t := range all {
		if t.ID == req.GetCallerTaskId() {
			continue
		}
		out = append(out, &agentfleetv1.SessionSummary{
			TaskId:      t.ID,
			Repo:        t.Repo,
			Description: t.Description,
			Status:      t.Status,
			LiveState:   string(tasks.DeriveLiveState(&t, time.Now(), dashboard.DefaultTurnStall)),
		})
	}
	return &agentfleetv1.ListSessionsResponse{Sessions: out}, nil
}

// PromptSession delivers one agent's message to another session.
//
// The guards are the whole design; the delivery is three lines.
func (s *Server) PromptSession(ctx context.Context, req *agentfleetv1.PromptSessionRequest) (*agentfleetv1.PromptSessionResponse, error) {
	caller, target, text := req.GetCallerTaskId(), req.GetTargetTaskId(), req.GetText()
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
	if state == tasks.LiveStateBlocked {
		return nil, fmt.Errorf("session %s is blocked waiting on a human decision — it cannot be prompted by an agent", target)
	}
	if t.Status == "proposed" {
		return nil, fmt.Errorf("session %s is an unapproved proposal", target)
	}

	// Delivered as a plain discussion entry authored by "agent". It is
	// deliberately impossible for this path to produce an `answer` or a
	// `permission_response`: those resolve human decisions, and an agent
	// answering another agent's prompt would silently void docs/adr/0029's
	// guarantee that a permission decision is always a real human one.
	body := fmt.Sprintf("[from session %s]\n\n%s", caller, text)
	seq, err := s.transcr.Append(ctx, target, "agent", body, "discussion", fmt.Sprintf("prompt-%s-%s-%d", caller, target, time.Now().UnixNano()))
	if err != nil {
		return nil, fmt.Errorf("append prompt: %w", err)
	}

	// Warm an idle target so the message is actually read rather than
	// sitting in a transcript nobody is attached to. Best-effort: the entry
	// is already durable, and the session will see it whenever it next
	// warms, so a capacity rejection is not worth failing the delivery over.
	var podName string
	if s.warm != nil && !tasks.IsPodPhaseLive(t.PodPhase) {
		if podName, err = s.warm(ctx, target); err != nil {
			slog.Warn("coreserver: PromptSession could not warm target", "callerTaskId", caller, "targetTaskId", target, "error", err)
		}
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
	target := req.GetTargetTaskId()
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
		if wanted[string(state)] {
			return &agentfleetv1.WaitForSessionStateResponse{LiveState: string(state)}, nil
		}
		// A target with no live pod will never move on its own — nothing is
		// running to change its state — so waiting the full timeout would
		// just be a slow way to time out.
		if state == tasks.LiveStateNone {
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
