package dashboard

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"time"

	"connectrpc.com/connect"

	"github.com/MohammadBnei/agent-fleet/core/internal/errkeys"
	"github.com/MohammadBnei/agent-fleet/core/internal/ratings"
	"github.com/MohammadBnei/agent-fleet/core/internal/sessions"
	agentfleetv1 "github.com/MohammadBnei/agent-fleet/proto/gen/go/agentfleet/v1"
)

// switchSessions, podRunner and resultReader are what a model switch touches,
// so its refusal paths and teardown order test against fakes.
type switchSessions interface {
	Get(ctx context.Context, id string) (*sessions.Session, error)
	ClearLease(ctx context.Context, id string) error
	SetPodPhase(ctx context.Context, id, phase, message string) error
	SetModel(ctx context.Context, id, model string) (bool, error)
}

type podRunner interface {
	TearDownWorker(ctx context.Context, sessionID string) error
	ListWorkerPods(ctx context.Context) (map[string]string, error)
}

type resultReader interface {
	LastResult(ctx context.Context, sessionID string) (ratings.Result, bool, error)
}

// SetSessionModel switches a repo session's model: the pod is stopped, the
// model saved, and the next message warms on it with RESUME_SESSION_ID (the
// existing path), so every per-process setting (base URL, window) is right.
// Never StopSession: that is Kill, an abort entry and a grace sweep.
func (s *Server) SetSessionModel(ctx context.Context, req *connect.Request[agentfleetv1.SetSessionModelRequest]) (*connect.Response[agentfleetv1.SetSessionModelResponse], error) {
	w := &modelSwitcher{
		lock: s.sends.lock, sessions: s.sessions, pods: s.e2e, transcr: s.transcr, results: s.ratings,
		gateway: s.gatewayModels, poll: time.Second, waitMax: time.Minute,
	}
	if err := w.switchModel(ctx, req.Msg.GetSessionId(), req.Msg.GetModel()); err != nil {
		return nil, err
	}
	return connect.NewResponse(&agentfleetv1.SetSessionModelResponse{}), nil
}

type modelSwitcher struct {
	lock     func(ctx context.Context, sessionID string) (func(), error)
	sessions switchSessions
	pods     podRunner
	transcr  interface {
		Append(ctx context.Context, sessionID, from, text, msgType, idempotencyKey string) (int64, error)
	}
	results resultReader
	gateway bool          // FLEET_GATEWAY_MODELS
	poll    time.Duration // between checks that the Job is gone
	waitMax time.Duration // give up waiting for it after this
}

func refuse(code connect.Code, format string, a ...any) error {
	return errkeys.Connect(code, errkeys.KeyInternal, fmt.Errorf(format, a...))
}

func (w *modelSwitcher) switchModel(ctx context.Context, sessionID, model string) error {
	// The gateway check comes first and alone: a gateway id may not be in
	// validModels yet, and a disabled one must say why.
	gw, known := gatewayModels[model]
	switch {
	case isGatewayRoute(model) && known && !w.gateway:
		return refuse(connect.CodeFailedPrecondition, "%s", errGatewayDisabled)
	case !known && !isValidModel(model):
		return refuse(connect.CodeInvalidArgument, "invalid model %q", model)
	}

	// The send lock makes this one critical section with every warm and
	// append: a message appended while the pod goes down would land below the
	// next warm's cursor and never be delivered (warm-then-append).
	release, err := w.lock(ctx, sessionID)
	if err != nil {
		return errkeys.Connect(connect.CodeCanceled, errkeys.KeyInternal, err)
	}
	defer release()

	t, err := w.sessions.Get(ctx, sessionID)
	if err != nil {
		if errors.Is(err, sessions.ErrNotFound) {
			return errkeys.Connect(connect.CodeNotFound, errkeys.KeyInternal, err)
		}
		return errkeys.Connect(connect.CodeInternal, errkeys.KeyInternal, err)
	}
	if t.DefinitionName != nil {
		return refuse(connect.CodeUnimplemented, "model switch is for repo sessions")
	}
	if t.SweptAt != nil {
		return refuse(connect.CodeFailedPrecondition, "session was swept: there is nothing to resume")
	}
	from := ""
	if t.Model != nil {
		from = *t.Model
	}
	if from == model {
		return nil
	}
	live := sessions.IsPodPhaseLive(t.PodPhase)
	if live {
		switch st := sessions.DeriveLiveState(t, time.Now(), DefaultTurnStall); st {
		case sessions.LiveStateIdle, sessions.LiveStateDone:
		default:
			return refuse(connect.CodeFailedPrecondition, "a turn is running (%s): interrupt it or wait for it to finish", st)
		}
	}
	// A smaller window than the transcript already fills would fail on the
	// first message. claude-* ids have no check.
	if known && w.results != nil {
		last, ok, err := w.results.LastResult(ctx, sessionID)
		if err != nil {
			return errkeys.Connect(connect.CodeInternal, errkeys.KeyInternal, err)
		}
		// ContextTokens is 0 on results from before the worker sent it: no check then.
		if ok && last.ContextTokens > gw.ContextTokens {
			return refuse(connect.CodeFailedPrecondition, "the transcript is %d tokens and %s takes %d", last.ContextTokens, model, gw.ContextTokens)
		}
	}

	if live {
		if err := w.takeDown(ctx, sessionID); err != nil {
			return err
		}
	}
	ok, err := w.sessions.SetModel(ctx, sessionID, model)
	if err != nil {
		return errkeys.Connect(connect.CodeInternal, errkeys.KeyInternal, err)
	}
	if !ok {
		return refuse(connect.CodeFailedPrecondition, "model switch is for repo sessions")
	}
	body, _ := json.Marshal(map[string]string{"from": from, "to": model})
	if _, err := w.transcr.Append(ctx, sessionID, "fleet", string(body), "model_switch", ""); err != nil {
		// The model is saved and the next warm uses it; only the seam in the feed is missing.
		slog.WarnContext(ctx, "dashboard SetSessionModel: recording the switch", "sessionId", sessionID, "error", err)
	}
	slog.InfoContext(ctx, "model switched", "sessionId", sessionID, "from", from, "to", model)
	return nil
}

// takeDown is the locked teardown (docs/adr/0053's resume path, without
// Kill): delete the Job, wait until it is gone, revoke the lease, write a
// non-live phase. Off the caller's cancellation, so a dropped request cannot
// leave a half-torn pod holding a live lease. A failure leaves the model unsaved.
func (w *modelSwitcher) takeDown(ctx context.Context, sessionID string) error {
	ctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 3*time.Minute)
	defer cancel()
	if err := w.pods.TearDownWorker(ctx, sessionID); err != nil {
		return errkeys.Connect(connect.CodeInternal, errkeys.KeyInternal, fmt.Errorf("stopping the pod: %w", err))
	}
	deadline := time.Now().Add(w.waitMax)
	for {
		pods, err := w.pods.ListWorkerPods(ctx)
		if err != nil {
			return errkeys.Connect(connect.CodeInternal, errkeys.KeyInternal, fmt.Errorf("waiting for the pod to go: %w", err))
		}
		if _, up := pods[sessionID]; !up {
			break
		}
		if time.Now().After(deadline) {
			return refuse(connect.CodeUnavailable, "the pod is still shutting down: try again in a moment")
		}
		time.Sleep(w.poll)
	}
	if err := w.sessions.ClearLease(ctx, sessionID); err != nil {
		return errkeys.Connect(connect.CodeInternal, errkeys.KeyInternal, err)
	}
	if err := w.sessions.SetPodPhase(ctx, sessionID, agentfleetv1.PodPhase_POD_PHASE_TERMINATED.String(), "stopped for a model switch"); err != nil {
		return errkeys.Connect(connect.CodeInternal, errkeys.KeyInternal, err)
	}
	return nil
}
