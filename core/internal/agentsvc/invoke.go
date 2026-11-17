package agentsvc

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"
	"unicode/utf8"

	"connectrpc.com/connect"

	"github.com/MohammadBnei/agent-fleet/core/internal/dashboard"
	"github.com/MohammadBnei/agent-fleet/core/internal/decisions"
	"github.com/MohammadBnei/agent-fleet/core/internal/definitions"
	"github.com/MohammadBnei/agent-fleet/core/internal/errkeys"
	"github.com/MohammadBnei/agent-fleet/core/internal/sessions"
	agentfleetv1 "github.com/MohammadBnei/agent-fleet/proto/gen/go/agentfleet/v1"
)

const (
	singleShot = "single-shot"
	maxWait    = 60 * time.Second
)

// endRecheck bounds how late WaitForResult sees an end that appends
// nothing; a var for tests.
var endRecheck = 5 * time.Second

// Invoke is Open with fixed choices (docs/adr/0070 §1): a private
// single-shot thread the client owns, claimed on (invoker, idempotency_key)
// as Open is (docs/adr/0066 §7), warmed through WarmForAppend, then the
// input appended with the key. A request that failed before its input was
// appended re-runs on the same thread; a delivered one returns its thread
// and starts nothing.
func (s *Server) Invoke(ctx context.Context, req *connect.Request[agentfleetv1.InvokeRequest]) (*connect.Response[agentfleetv1.InvokeResponse], error) {
	return s.invoke(ctx, req, sessions.TierFleet)
}

// InvokeUntrusted is Invoke for a run whose input is an untrusted machine event (docs/adr/0086 §3): the
// dispatcher's alert-group runs. The run's set is A plus the definition's trust.untrusted letter, and a
// definition without one refuses it (sessions.ErrNoUntrusted).
func (s *Server) InvokeUntrusted(ctx context.Context, req *connect.Request[agentfleetv1.InvokeRequest]) (*connect.Response[agentfleetv1.InvokeResponse], error) {
	return s.invoke(ctx, req, sessions.TierUntrusted)
}

func (s *Server) invoke(ctx context.Context, req *connect.Request[agentfleetv1.InvokeRequest], tier sessions.Tier) (*connect.Response[agentfleetv1.InvokeResponse], error) {
	p, err := principal(ctx)
	if err != nil {
		return nil, err
	}
	m := req.Msg
	switch {
	case p.Client == "":
		return nil, errkeys.Connect(connect.CodePermissionDenied, errkeys.KeyInternal, errors.New("only clients may invoke"))
	}
	if err := checkInvokeBounds(m.GetIdempotencyKey(), m.GetInput()); err != nil {
		return nil, err
	}
	head, ok := s.usable(p, m.GetDefinition())
	if !ok {
		return nil, errkeys.Connect(connect.CodeNotFound, errkeys.KeyDefinitionUnavailable, definitions.ErrUnknown)
	}
	return s.run(ctx, p, m, head, tier, nil)
}

// checkInvokeBounds is Invoke's bound on a run's key and input, shared by a character's spawn.
func checkInvokeBounds(key, input string) error {
	switch {
	case key == "" || len(key) > maxKey:
		return errkeys.Connect(connect.CodeInvalidArgument, errkeys.KeyInternal, fmt.Errorf("idempotency_key is required and at most %d bytes", maxKey))
	case input == "" || len(input) > maxText:
		return errkeys.Connect(connect.CodeInvalidArgument, errkeys.KeyInternal, fmt.Errorf("input is required and at most %d bytes", maxText))
	}
	return nil
}

// run is invoke after the caller's right to the definition is settled. admit, when set, is a gate on starting a NEW
// run (a spawn's hourly and daily caps): a retry of a run already started never meets it.
func (s *Server) run(ctx context.Context, p Principal, m *agentfleetv1.InvokeRequest, head definitions.Head, tier sessions.Tier, admit func(context.Context) error) (*connect.Response[agentfleetv1.InvokeResponse], error) {
	if err := dashboard.StandingOnly(head.Effective); err != nil {
		return nil, err
	}
	if !allows(head.Effective.Lifecycle, singleShot) {
		return nil, errkeys.Connect(connect.CodeFailedPrecondition, errkeys.KeyInternal, errors.New("this definition does not take single-shot runs"))
	}
	// A first responder is A·B and only the dispatcher's alert runs start one (docs/adr/0086 §3): a client's own
	// Invoke would give it a trusted set.
	if tier != sessions.TierUntrusted && head.Effective.Responder() {
		return nil, errkeys.Connect(connect.CodePermissionDenied, errkeys.KeyInternal, errors.New("a first responder is started by an alert, not an Invoke"))
	}
	var untrusted string
	if head.Effective.Trust != nil {
		untrusted = head.Effective.Trust.Untrusted
	}
	trustSet, err := sessions.TrustSetFor(tier, untrusted)
	if err != nil {
		return nil, errkeys.Connect(connect.CodeFailedPrecondition, errkeys.KeyInternal, err)
	}
	owner, key := p.owner(), m.GetIdempotencyKey()
	// "invoke" first: an Open's claim on the same key never matches.
	claim, delivered, _, release, err := s.claimRequest(ctx, invokeSlots, owner, key, nil, requestSHA("invoke", m.GetDefinition(), m.GetInput()))
	defer release()
	if err != nil {
		return nil, err
	}
	if delivered != "" {
		return connect.NewResponse(&agentfleetv1.InvokeResponse{ThreadId: delivered}), nil
	}

	var t *sessions.Session
	if claim.SessionID != "" {
		got, err := s.sessions.Get(ctx, claim.SessionID)
		if err != nil && !errors.Is(err, sessions.ErrNotFound) {
			return nil, errkeys.Connect(connect.CodeInternal, errkeys.KeyInternal, err)
		}
		if got != nil && got.ArchivedAt == nil {
			t = got
		}
	}
	// Nothing was appended yet (the claim is pending), so a thread pinned to
	// a definition that moved on, or swept, is replaced rather than retried
	// into a warm that can only refuse.
	if t != nil && (deref(t.DefinitionSHA, "") != head.SHA || t.SweptAt != nil) {
		if _, err := s.dash.ArchiveSession(ctx, connect.NewRequest(&agentfleetv1.ArchiveSessionRequest{SessionId: t.ID})); err != nil {
			return nil, err
		}
		t = nil
	}
	if t == nil {
		if admit != nil {
			if err := admit(ctx); err != nil {
				return nil, err
			}
		}
		// Refused, not queued: the claim stays pending, so the same key runs
		// once a parked run moves (docs/adr/0070 §1). Admission-time only: runs
		// still live when admitted may park later and exceed maxParked;
		// maxClientRuns is the hard bound.
		if n, err := s.sessions.ParkedRuns(ctx, owner); err != nil {
			return nil, errkeys.Connect(connect.CodeInternal, errkeys.KeyInternal, err)
		} else if limit := s.maxParked(p.Client); n >= limit {
			return nil, errkeys.Connect(connect.CodeResourceExhausted, errkeys.KeyParkedLimit, fmt.Errorf("this client has %d runs waiting on a decision (maxParked %d); answer one first", n, limit))
		}
		if n, err := s.sessions.UnfinishedRuns(ctx, owner); err != nil {
			return nil, errkeys.Connect(connect.CodeInternal, errkeys.KeyInternal, err)
		} else if n >= maxClientRuns {
			return nil, errkeys.Connect(connect.CodeResourceExhausted, errkeys.KeyThreadCap, fmt.Errorf("this client has %d unfinished runs; wait for some first", n))
		}
		// Cut on a rune boundary: Postgres refuses invalid UTF-8.
		n := min(len(key), maxTitle)
		for n > 0 && n < len(key) && !utf8.RuneStart(key[n]) {
			n--
		}
		title := key[:n]
		if t, err = s.dash.CreateThread(ctx, head, title, sessions.CreateParams{
			Owner: owner, Visibility: "private", Lifecycle: singleShot, Invoker: p.Client,
			TrustSet: trustSet,
		}); err != nil {
			return nil, err
		}
		if err := s.sessions.PointClaim(ctx, owner, key, t.ID); err != nil {
			return nil, errkeys.Connect(connect.CodeInternal, errkeys.KeyInternal, err)
		}
	}

	// Warm BEFORE appending, never after — see dashboard.WarmIfIdle.
	_, unlock, err := s.dash.WarmForAppend(ctx, t.ID)
	defer unlock()
	if err != nil {
		// The claim stays pending and the thread stays: the retry with the
		// same key warms it again (docs/adr/0070 §4).
		if connect.CodeOf(err) == connect.CodeResourceExhausted {
			return nil, capacity(t.ID, err)
		}
		return nil, err
	}
	if _, err := s.sessions.AppendClaimed(ctx, owner, key, sessions.ClaimedEntry{
		SessionID: t.ID, Text: m.GetInput(), AuthorSub: owner, AuthorName: owner,
	}); errors.Is(err, sessions.ErrArchived) {
		return nil, errkeys.Connect(connect.CodeAborted, errkeys.KeyInternal, err)
	} else if err != nil {
		return nil, errkeys.Connect(connect.CodeInternal, errkeys.KeyInternal, err)
	}
	return connect.NewResponse(&agentfleetv1.InvokeResponse{ThreadId: t.ID}), nil
}

// maxParked is client's ParkedCap, or the default when the registry no
// longer lists it (MayOpen has just let it through: only a sync in between).
func (s *Server) maxParked(client string) int {
	for _, c := range s.definitions.Clients() {
		if c.Name == client {
			return c.Spec.ParkedCap()
		}
	}
	return definitions.DefaultMaxParked
}

// capacity is the full fleet's answer: ResourceExhausted with the thread id
// in an InvokeCapacity detail, so the client retries the same key.
func capacity(threadID string, cause error) *connect.Error {
	ce := errkeys.Connect(connect.CodeResourceExhausted, errkeys.KeyInternal, cause)
	if d, err := connect.NewErrorDetail(&agentfleetv1.InvokeCapacity{ThreadId: threadID}); err == nil {
		ce.AddDetail(d)
	}
	return ce
}

// WaitForResult is docs/adr/0070 §4's long poll: it answers at once when the
// run is past "running", else when the transcript moves to such a state or
// wait_seconds pass.
func (s *Server) WaitForResult(ctx context.Context, req *connect.Request[agentfleetv1.WaitForResultRequest]) (*connect.Response[agentfleetv1.WaitForResultResponse], error) {
	p, err := principal(ctx)
	if err != nil {
		return nil, err
	}
	wait := time.Duration(req.Msg.GetWaitSeconds()) * time.Second
	if wait < 0 || wait > maxWait {
		return nil, errkeys.Connect(connect.CodeInvalidArgument, errkeys.KeyInternal, fmt.Errorf("wait_seconds must be 0 to %d", int(maxWait/time.Second)))
	}
	t, _, err := s.lookup(ctx, p, req.Msg.GetThreadId(), true)
	if err != nil {
		return nil, err
	}
	if deref(t.Lifecycle, "resident") != singleShot {
		return nil, errkeys.Connect(connect.CodeFailedPrecondition, errkeys.KeyInternal, errors.New("this thread is not a single-shot run"))
	}
	return s.awaitResult(ctx, t, wait)
}

// awaitResult is WaitForResult's loop on a run the caller may see.
func (s *Server) awaitResult(ctx context.Context, t *sessions.Session, wait time.Duration) (*connect.Response[agentfleetv1.WaitForResultResponse], error) {
	// Subscribe before the first read, so nothing appended between is missed.
	head, err := s.transcr.LatestSeq(ctx, t.ID)
	if err != nil {
		return nil, errkeys.Connect(connect.CodeInternal, errkeys.KeyInternal, err)
	}
	wake, cancel := s.hub.Subscribe(t.ID, head)
	defer cancel()
	deadline := time.NewTimer(wait)
	defer deadline.Stop()
	// An end that appends nothing (endRuns archiving a crashed or stopped
	// run) wakes no subscriber, so the row is re-read every pass and a pass
	// runs at least every recheck.
	recheck := time.NewTicker(endRecheck)
	defer recheck.Stop()
	for {
		if t, err = s.sessions.Get(ctx, t.ID); err != nil {
			return nil, errkeys.Connect(connect.CodeInternal, errkeys.KeyInternal, err)
		}
		out, err := s.result(ctx, t)
		if err != nil {
			return nil, errkeys.Connect(connect.CodeInternal, errkeys.KeyInternal, err)
		}
		if out.GetState() != "running" {
			return connect.NewResponse(out), nil
		}
		select {
		case <-ctx.Done():
			return connect.NewResponse(out), nil
		case <-deadline.C:
			return connect.NewResponse(out), nil
		case <-recheck.C:
		case _, ok := <-wake:
			if !ok {
				return connect.NewResponse(out), nil
			}
		}
	}
}

// resultEntry is the part of the worker's result entry WaitForResult reads
// (docs/adr/0070 §3).
type resultEntry struct {
	Subtype          string          `json:"subtype"`
	ResultText       string          `json:"result_text"`
	StructuredOutput json.RawMessage `json:"structured_output"`
}

// result is §3's final-turn rule, after §6's re-park cap (a run with no live
// pod that parked more than limits.maxParks times is failed, PARK_LIMIT).
// Only an exit result (the pod's last,
// `exit: true`) is judged, in this order:
//  1. a human or answer entry above its consumedSeq: not final (running);
//     core re-warms the run for it (sessions.Loop's exit-window re-warm);
//  2. an open question, permission or plan: waiting, whatever the result
//     says (a parked turn's structured output is never the result), and
//     checked first, since it outranks every result;
//  3. success: done;
//  4. any other subtype: failed.
//
// A run archived without a final result ended another way (§7): a crash is
// AGENT_CRASHED, a clean exit with no result is NO_RESULT, a Stop is
// failed; never running.
func (s *Server) result(ctx context.Context, t *sessions.Session) (*agentfleetv1.WaitForResultResponse, error) {
	sessionID := t.ID
	ex, err := s.sessions.LastExitResult(ctx, sessionID)
	if err != nil {
		return nil, err
	}
	// docs/adr/0070 §6, the re-park cap: past limits.maxParks parks the run
	// ends failed whatever it waits on (endRuns withdraws its decisions within
	// a tick, and warm refuses it meanwhile). A parked turn's result is never
	// the answer.
	if ex.OverParkCap && !sessions.IsPodPhaseLive(t.PodPhase) {
		return &agentfleetv1.WaitForResultResponse{State: "failed", Error: &agentfleetv1.Status{
			State: "error", Code: "PARK_LIMIT", Message: "the run parked more than its limits.maxParks times"}}, nil
	}
	open, err := s.decisions.List(ctx, decisions.Filter{
		SessionID: sessionID, States: []string{decisions.Open},
		Kinds: []decisions.Kind{decisions.Question, decisions.Permission, decisions.Plan},
		Admin: true, Limit: maxDecisionPage,
	})
	if err != nil {
		return nil, err
	}
	if len(open) > 0 {
		out := &agentfleetv1.WaitForResultResponse{State: "waiting"}
		for _, r := range open {
			out.Pending = append(out.Pending, decisionToProto(r))
		}
		return out, nil
	}
	// Core ended it waiting (sessions.Loop's expireRuns, docs/adr/0070 §6):
	// expired outranks every result (§3), and a moved pin is failed.
	switch {
	case ex.Expired:
		return &agentfleetv1.WaitForResultResponse{State: "expired", Error: &agentfleetv1.Status{
			State: "error", Code: "ANSWER_TIMEOUT", Message: "a decision went unanswered past limits.answerTimeout"}}, nil
	case ex.Moved:
		return &agentfleetv1.WaitForResultResponse{State: "failed", Error: &agentfleetv1.Status{
			State: "error", Code: "DEFINITION_CHANGED", Message: "the run's definition changed while it waited on a decision"}}, nil
	}
	// An ended run with no exit result at all (a pod on an older worker at
	// deploy, or a result without queued_turn_count) is judged by the seq
	// rule from before slice 7 rather than read NO_RESULT.
	if !ex.Found && ex.Legacy != "" && t.ArchivedAt != nil {
		ex.Text, ex.Found = ex.Legacy, true
	}
	if (!ex.Found || ex.InputAfter) && t.ArchivedAt != nil {
		e := &agentfleetv1.Status{State: "error", Message: "the run ended without a result"}
		switch phase := deref(t.PodPhase, ""); {
		case phase == agentfleetv1.PodPhase_POD_PHASE_CRASHED.String():
			e = &agentfleetv1.Status{State: "error", Code: "AGENT_CRASHED", Message: "the run's pod crashed"}
		case !ex.Found && !ex.Stopped && phase == agentfleetv1.PodPhase_POD_PHASE_TERMINATED.String():
			// docs/adr/0066 §10 as amended: AGENT_CRASHED is a non-zero exit
			// only; a pod that exited cleanly without its result is this.
			e = &agentfleetv1.Status{State: "error", Code: "NO_RESULT", Message: "the run's pod exited without a result"}
		}
		return &agentfleetv1.WaitForResultResponse{State: "failed", Error: e}, nil
	}
	if !ex.Found || ex.InputAfter {
		return &agentfleetv1.WaitForResultResponse{State: "running"}, nil
	}
	var r resultEntry
	if err := json.Unmarshal([]byte(ex.Text), &r); err != nil {
		return &agentfleetv1.WaitForResultResponse{State: "failed", Error: &agentfleetv1.Status{State: "error", Message: "the result entry is unreadable"}}, nil
	}
	out := &agentfleetv1.WaitForResultResponse{State: "failed", Subtype: r.Subtype, ResultText: r.ResultText}
	if r.Subtype == "success" {
		out.State = "done"
	}
	if len(r.StructuredOutput) > 0 && string(r.StructuredOutput) != "null" {
		out.StructuredOutput = string(r.StructuredOutput)
	}
	return out, nil
}
