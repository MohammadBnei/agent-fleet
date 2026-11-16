package agentsvc

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"
	"unicode/utf8"

	"connectrpc.com/connect"

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

// Invoke is Open with fixed choices (docs/adr/0070 §1): a private
// single-shot thread the client owns, claimed on (invoker, idempotency_key)
// as Open is (docs/adr/0066 §7), warmed through WarmForAppend, then the
// input appended with the key. A request that failed before its input was
// appended re-runs on the same thread; a delivered one returns its thread
// and starts nothing.
func (s *Server) Invoke(ctx context.Context, req *connect.Request[agentfleetv1.InvokeRequest]) (*connect.Response[agentfleetv1.InvokeResponse], error) {
	p, err := principal(ctx)
	if err != nil {
		return nil, err
	}
	m := req.Msg
	switch {
	case p.Client == "":
		return nil, errkeys.Connect(connect.CodePermissionDenied, errkeys.KeyInternal, errors.New("only clients may invoke"))
	case m.GetIdempotencyKey() == "" || len(m.GetIdempotencyKey()) > maxKey:
		return nil, errkeys.Connect(connect.CodeInvalidArgument, errkeys.KeyInternal, fmt.Errorf("idempotency_key is required and at most %d bytes", maxKey))
	case m.GetInput() == "" || len(m.GetInput()) > maxText:
		return nil, errkeys.Connect(connect.CodeInvalidArgument, errkeys.KeyInternal, fmt.Errorf("input is required and at most %d bytes", maxText))
	}
	head, ok := s.usable(p, m.GetDefinition())
	if !ok {
		return nil, errkeys.Connect(connect.CodeNotFound, errkeys.KeyDefinitionUnavailable, definitions.ErrUnknown)
	}
	if !allows(head.Effective.Lifecycle, singleShot) {
		return nil, errkeys.Connect(connect.CodeFailedPrecondition, errkeys.KeyInternal, errors.New("this definition does not take single-shot runs"))
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
	t, _, err := s.thread(ctx, p, req.Msg.GetThreadId())
	if err != nil {
		return nil, err
	}
	if deref(t.Lifecycle, "resident") != singleShot {
		return nil, errkeys.Connect(connect.CodeFailedPrecondition, errkeys.KeyInternal, errors.New("this thread is not a single-shot run"))
	}
	// Subscribe before the first read, so nothing appended between is missed.
	head, err := s.transcr.LatestSeq(ctx, t.ID)
	if err != nil {
		return nil, errkeys.Connect(connect.CodeInternal, errkeys.KeyInternal, err)
	}
	wake, cancel := s.hub.Subscribe(t.ID, head)
	defer cancel()
	deadline := time.NewTimer(wait)
	defer deadline.Stop()
	for {
		out, err := s.result(ctx, t.ID)
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

// result is §3's final-turn rule. Until the worker writes an exit result
// (build order slice 7), rules 2 to 4 apply to the latest result:
//  2. an open question, permission or plan: waiting, whatever the result
//     says (a parked turn's structured output is never the result);
//  3. success: done;
//  4. any other subtype: failed.
//
// ponytail: rule 1 (input above the exit result's consumedSeq) is read by
// seq order for now: a result with turn-starting input after it is not
// final, since a continuously running worker answers that input with a later
// result. Slice 7 moves it to consumedSeq on the exit result.
func (s *Server) result(ctx context.Context, sessionID string) (*agentfleetv1.WaitForResultResponse, error) {
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
	text, inputAfter, found, err := s.sessions.LastResult(ctx, sessionID)
	if err != nil {
		return nil, err
	}
	if !found || inputAfter {
		return &agentfleetv1.WaitForResultResponse{State: "running"}, nil
	}
	var r resultEntry
	if err := json.Unmarshal([]byte(text), &r); err != nil {
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
